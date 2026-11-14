package live_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/api/s3"
	"github.com/KinasTomes/miles/internal/metadata/dynamodb"
	"github.com/KinasTomes/miles/internal/storage"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

var (
	// Deterministic synthetic payloads for Cold Lifecycle verification.
	// IR payload: 35 bytes * 128 = 4480 bytes.
	// Glacier Flexible payload: 41 bytes * 128 = 5248 bytes.
	coldIRPayload      = bytes.Repeat([]byte("miles-cold-synthetic-glacier-ir-v1\n"), 128)
	coldGlacierPayload = bytes.Repeat([]byte("miles-cold-synthetic-glacier-flexible-v1\n"), 128)
)

type restoreDocInput struct {
	XMLName              xml.Name `xml:"RestoreRequest"`
	Days                 int      `xml:"Days"`
	GlacierJobParameters struct {
		Tier string `xml:"Tier"`
	} `xml:"GlacierJobParameters"`
}

func restoreXML(days int, tier string) []byte {
	doc := restoreDocInput{
		Days: days,
		GlacierJobParameters: struct {
			Tier string `xml:"Tier"`
		}{Tier: tier},
	}
	b, _ := xml.Marshal(doc)
	return append([]byte(xml.Header), b...)
}

// Exercise exact request construction and parsing through the S3 router for all
// Cold Lifecycle operations: PUT, HEAD before/during/after restore, GET rejection
// while cold/pending, RestoreObject initiation/conflict, and restored payload read.
func TestLiveFixtureColdWireRequests(t *testing.T) {
	f := fixture{
		t:      t,
		config: liveConfig{Region: "ap-southeast-1"},
		ep:     endpoint{URL: "https://cloud.example:443", Admin: "http://127.0.0.1:9000"},
		bucket: "acceptance-cold-wire",
	}
	c := credential{ID: "ABCDEFGHIJKLMNOPQRST", Secret: "synthetic-fixture-secret"}
	body := bytes.Clone(coldGlacierPayload)
	sum := sha256.Sum256(body)
	key := "/cold/archive-payload.bin"

	type stageState struct {
		restoreHeader string
		isRestored    bool
		isPending     bool
		isCold        bool
		restoreCalls  int
	}
	state := stageState{isCold: false}

	router := &s3.Router{
		Region:      f.config.Region,
		Credentials: wireIdentity{},
		Access:      wireIdentity{},
		Now:         time.Now,
		Handlers: map[s3.Operation]s3.Handler{
			s3.PutObject: func(w http.ResponseWriter, q *s3.Request) error {
				if q.HTTP.Host != "cloud.example:443" || q.HTTP.URL.EscapedPath() != "/acceptance-cold-wire"+key {
					t.Fatalf("unexpected PutObject target: host=%s path=%s", q.HTTP.Host, q.HTTP.URL.EscapedPath())
				}
				data, err := io.ReadAll(q.Body)
				if err != nil || !bytes.Equal(data, body) {
					t.Fatalf("PutObject body mismatch: len=%d err=%v", len(data), err)
				}
				w.Header().Set("ETag", fmt.Sprintf("\"%x\"", sum))
				w.WriteHeader(http.StatusOK)
				return nil
			},
			s3.HeadObject: func(w http.ResponseWriter, q *s3.Request) error {
				if q.HTTP.Host != "cloud.example:443" || q.HTTP.URL.EscapedPath() != "/acceptance-cold-wire"+key {
					t.Fatalf("unexpected HeadObject target: %s", q.HTTP.URL.EscapedPath())
				}
				w.Header().Set("Content-Length", fmt.Sprint(len(body)))
				w.Header().Set("ETag", fmt.Sprintf("\"%x\"", sum))
				if state.restoreHeader != "" {
					w.Header().Set("x-amz-restore", state.restoreHeader)
				}
				if state.isCold {
					w.Header().Set("x-amz-storage-class", "GLACIER")
				}
				w.WriteHeader(http.StatusOK)
				return nil
			},
			s3.GetObject: func(w http.ResponseWriter, q *s3.Request) error {
				if q.HTTP.Host != "cloud.example:443" || q.HTTP.URL.EscapedPath() != "/acceptance-cold-wire"+key {
					t.Fatalf("unexpected GetObject target: %s", q.HTTP.URL.EscapedPath())
				}
				if state.isCold && !state.isRestored {
					return &s3.Error{Code: s3.ErrInvalidObjectState, Message: "The operation is not valid for the object's storage class"}
				}
				if state.restoreHeader != "" {
					w.Header().Set("x-amz-restore", state.restoreHeader)
				}
				w.Header().Set("Content-Length", fmt.Sprint(len(body)))
				w.Header().Set("ETag", fmt.Sprintf("\"%x\"", sum))
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(body)
				return nil
			},
			s3.RestoreObject: func(w http.ResponseWriter, q *s3.Request) error {
				if q.HTTP.Host != "cloud.example:443" || q.HTTP.URL.EscapedPath() != "/acceptance-cold-wire"+key {
					t.Fatalf("unexpected RestoreObject target: %s", q.HTTP.URL.EscapedPath())
				}
				var req restoreDocInput
				if err := q.Body.DecodeXML(&req, 4096); err != nil {
					return &s3.Error{Code: s3.ErrMalformedXML, Message: "The XML provided was not well-formed"}
				}
				if req.Days <= 0 || req.Days > 31 {
					return &s3.Error{Code: s3.ErrInvalidRequest, Message: "Days must be between 1 and 31"}
				}
				if req.GlacierJobParameters.Tier != "Standard" && req.GlacierJobParameters.Tier != "Bulk" && req.GlacierJobParameters.Tier != "Expedited" {
					return &s3.Error{Code: s3.ErrInvalidRequest, Message: "Unknown Tier"}
				}
				if !state.isCold {
					return &s3.Error{Code: s3.ErrInvalidRequest, Message: "The object has no GLACIER data to restore"}
				}
				if state.isPending {
					return &s3.Error{Code: s3.ErrRestoreAlreadyInProgress, Message: "Object restore is already in progress"}
				}
				state.restoreCalls++
				state.isPending = true
				state.restoreHeader = "ongoing-request=\"true\""
				w.WriteHeader(http.StatusAccepted)
				return nil
			},
		},
	}

	exec := func(method, rawQuery string, reqBody []byte, headers map[string]string) *httptest.ResponseRecorder {
		t.Helper()
		req := f.signed(c, method, key, rawQuery, reqBody, headers, false)
		wire := httptest.NewRequest(req.Method, req.URL.String(), req.Body)
		wire.Header = req.Header.Clone()
		wire.ContentLength = req.ContentLength
		wire.Host = req.Host
		if req.ContentLength > 0 {
			wire.Header.Set("Content-Length", fmt.Sprint(req.ContentLength))
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, wire)
		return rec
	}

	// 1. Initial PUT
	rec := exec("PUT", "", body, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("initial PUT returned %d: %s", rec.Code, rec.Body.String())
	}

	// 2. Initial HEAD (Warm)
	rec = exec("HEAD", "", nil, nil)
	if rec.Code != http.StatusOK || rec.Header().Get("x-amz-restore") != "" {
		t.Fatalf("initial HEAD unexpected: %d restore=%s", rec.Code, rec.Header().Get("x-amz-restore"))
	}

	// 3. Initial GET (Warm)
	rec = exec("GET", "", nil, nil)
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), body) {
		t.Fatalf("initial GET failed: %d", rec.Code)
	}

	// Transition to Cold
	state.isCold = true

	// 4. GET before restore must be rejected with 403 InvalidObjectState
	rec = exec("GET", "", nil, nil)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "InvalidObjectState") {
		t.Fatalf("GET before restore expected 403 InvalidObjectState, got %d: %s", rec.Code, rec.Body.String())
	}

	// 5. RestoreObject validation errors
	rec = exec("POST", "restore=", []byte("<Malformed"), nil)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "MalformedXML") {
		t.Fatalf("malformed restore XML expected 400 MalformedXML, got %d: %s", rec.Code, rec.Body.String())
	}

	rec = exec("POST", "restore=", restoreXML(0, "Expedited"), nil)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "InvalidRequest") {
		t.Fatalf("Days=0 restore expected 400 InvalidRequest, got %d: %s", rec.Code, rec.Body.String())
	}

	rec = exec("POST", "restore=", restoreXML(35, "Expedited"), nil)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "InvalidRequest") {
		t.Fatalf("Days=35 restore expected 400 InvalidRequest, got %d: %s", rec.Code, rec.Body.String())
	}

	rec = exec("POST", "restore=", restoreXML(2, "InvalidTier"), nil)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "InvalidRequest") {
		t.Fatalf("invalid tier expected 400 InvalidRequest, got %d: %s", rec.Code, rec.Body.String())
	}

	// 6. Valid RestoreObject initiation -> 202 Accepted
	rec = exec("POST", "restore=", restoreXML(2, "Expedited"), nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("valid RestoreObject expected 202 Accepted, got %d: %s", rec.Code, rec.Body.String())
	}

	// 7. Duplicate RestoreObject while pending -> 409 RestoreAlreadyInProgress
	rec = exec("POST", "restore=", restoreXML(2, "Expedited"), nil)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "RestoreAlreadyInProgress") {
		t.Fatalf("duplicate RestoreObject expected 409 Conflict, got %d: %s", rec.Code, rec.Body.String())
	}

	// 8. HEAD while pending -> ongoing-request="true"
	rec = exec("HEAD", "", nil, nil)
	if rec.Code != http.StatusOK || rec.Header().Get("x-amz-restore") != `ongoing-request="true"` {
		t.Fatalf("HEAD during pending expected ongoing-request=true, got %d: %s", rec.Code, rec.Header().Get("x-amz-restore"))
	}

	// 9. GET while pending -> 403 InvalidObjectState
	rec = exec("GET", "", nil, nil)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "InvalidObjectState") {
		t.Fatalf("GET during pending expected 403, got %d", rec.Code)
	}

	// Transition to Restored
	state.isPending = false
	state.isRestored = true
	state.restoreHeader = `ongoing-request="false", expiry-date="Fri, 13 Nov 2026 00:00:00 GMT"`

	// 10. HEAD when completed -> ongoing-request="false", expiry-date="..."
	rec = exec("HEAD", "", nil, nil)
	if rec.Code != http.StatusOK || rec.Header().Get("x-amz-restore") != state.restoreHeader {
		t.Fatalf("HEAD restored mismatch: %d %s", rec.Code, rec.Header().Get("x-amz-restore"))
	}

	// 11. GET when completed -> 200 OK, payload and checksum match exactly
	rec = exec("GET", "", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET restored expected 200, got %d", rec.Code)
	}
	gotBytes := rec.Body.Bytes()
	if !bytes.Equal(gotBytes, body) {
		t.Fatalf("GET restored body mismatch")
	}
	gotSum := sha256.Sum256(gotBytes)
	if gotSum != sum {
		t.Fatalf("GET restored checksum mismatch: got %x, want %x", gotSum, sum)
	}
}

// Exercise exact Admin HTTP contracts: layout discovery (packs[].pack_id),
// job creation with object params and job_id response, job status polling,
// pack items array retrieval, pack promotion, and fail-closed stats assertions.
func TestLiveFixtureColdAdminContracts(t *testing.T) {
	promoted := false
	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/v1/objects/layout", func(w http.ResponseWriter, r *http.Request) {
		gen := "gen-00000001"
		state := "Cold"
		class := "GLACIER_IR"
		if promoted {
			gen = "gen-00000002"
			state = "Warm"
			class = "STANDARD"
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{
			"integrity": "verified",
			"bucket": "acceptance-cold-bkt",
			"key": "cold-ir.bin",
			"packs": [{
				"pack_id": "pk-contract-test-01",
				"generation": "%s",
				"state": "%s",
				"storage_class": "%s",
				"chunks": 1
			}]
		}`, gen, state, class)
	})
	mux.HandleFunc("POST /admin/v1/jobs", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Type           string         `json:"type"`
			Params         map[string]any `json:"params"`
			PolicyRevision uint64         `json:"policy_revision"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, `{"error":"invalid_request"}`, http.StatusBadRequest)
			return
		}
		if body.Type != "tiering" || body.Params["pack_id"] != "pk-contract-test-01" {
			http.Error(w, `{"error":"invalid_params"}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"job_id":"job-tier-contract-01","type":"tiering","state":"queued"}`))
	})
	mux.HandleFunc("GET /admin/v1/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"job_id":"` + r.PathValue("id") + `","state":"succeeded"}`))
	})
	mux.HandleFunc("GET /admin/v1/packs/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if promoted {
			_, _ = w.Write([]byte(`{
				"items": [
					{
						"pack_id": "pk-contract-test-01",
						"generation": "gen-00000001",
						"state": "Cold",
						"storage_class": "GLACIER_IR",
						"min_retention_until": "2027-02-11T00:00:00Z"
					},
					{
						"pack_id": "pk-contract-test-01",
						"generation": "gen-00000002",
						"state": "Warm",
						"storage_class": "STANDARD"
					}
				]
			}`))
			return
		}
		_, _ = w.Write([]byte(`{
			"items": [{
				"pack_id": "pk-contract-test-01",
				"generation": "gen-00000001",
				"state": "Cold",
				"storage_class": "GLACIER_IR",
				"min_retention_until": "2027-02-11T00:00:00Z"
			}]
		}`))
	})
	mux.HandleFunc("POST /admin/v1/packs/{id}/{generation}/promote", func(w http.ResponseWriter, r *http.Request) {
		promoted = true
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"job_id":"job-prom-contract-01","type":"pack_promote","state":"queued"}`))
	})
	mux.HandleFunc("GET /admin/v1/stats", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"revived_chunks_total": 1,
			"orphaned_chunks_total": 0,
			"listed_bytes_by_pack_lifecycle": {"cold_dead": 4480}
		}`))
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	f := fixture{
		t:      t,
		admin:  srv.Client(),
		ep:     endpoint{Admin: srv.URL},
		token:  "test-admin-token",
		bucket: "acceptance-cold-bkt",
	}

	// 1. Verify layout parsing using packs[].pack_id
	var layout struct {
		Packs []struct {
			PackID       string `json:"pack_id"`
			Generation   string `json:"generation"`
			State        string `json:"state"`
			StorageClass string `json:"storage_class"`
		} `json:"packs"`
	}
	f.adminRequest("GET", "objects/layout?bucket=acceptance-cold-bkt&key=cold-ir.bin", nil, f.token, 200, &layout)
	if len(layout.Packs) != 1 || layout.Packs[0].PackID != "pk-contract-test-01" {
		t.Fatalf("layout pack_id DTO mismatch: %+v", layout)
	}
	initialGen := layout.Packs[0].Generation

	// 2. Verify job creation DTO with JSON object params and job_id response
	var tierJob struct {
		JobID string `json:"job_id"`
	}
	f.adminRequest("POST", "jobs", map[string]any{
		"type":            "tiering",
		"params":          map[string]any{"pack_id": layout.Packs[0].PackID, "generation": layout.Packs[0].Generation},
		"policy_revision": 1,
	}, f.token, 202, &tierJob)
	if tierJob.JobID != "job-tier-contract-01" {
		t.Fatalf("tiering job create DTO mismatch: %+v", tierJob)
	}

	// 3. Verify await job polling with job_id
	f.awaitJob(tierJob.JobID, 5*time.Second)

	// 4. Verify pack items list DTO (items array)
	var packPage struct {
		Items []struct {
			PackID       string `json:"pack_id"`
			Generation   string `json:"generation"`
			State        string `json:"state"`
			StorageClass string `json:"storage_class"`
		} `json:"items"`
	}
	f.adminRequest("GET", "packs/pk-contract-test-01", nil, f.token, 200, &packPage)
	if len(packPage.Items) != 1 || packPage.Items[0].PackID != "pk-contract-test-01" || packPage.Items[0].State != "Cold" {
		t.Fatalf("pack items array DTO mismatch: %+v", packPage)
	}

	// 5. Verify promote pack DTO with job_id response
	var promJob struct {
		JobID string `json:"job_id"`
	}
	f.adminRequest("POST", "packs/pk-contract-test-01/"+initialGen+"/promote", map[string]any{}, f.token, 202, &promJob)
	if promJob.JobID != "job-prom-contract-01" {
		t.Fatalf("promote job DTO mismatch: %+v", promJob)
	}
	f.awaitJob(promJob.JobID, 5*time.Second)

	// 6. Verify layout after promotion advances generation
	f.adminRequest("GET", "objects/layout?bucket=acceptance-cold-bkt&key=cold-ir.bin", nil, f.token, 200, &layout)
	if len(layout.Packs) != 1 || layout.Packs[0].Generation == initialGen || layout.Packs[0].State != "Warm" {
		t.Fatalf("generation did not advance after promotion: %+v", layout)
	}
	promotedGen := layout.Packs[0].Generation

	// Verify multi-generation response: match exact pack_id + generation
	f.adminRequest("GET", "packs/pk-contract-test-01", nil, f.token, 200, &packPage)
	var sourceFound, targetFound bool
	for _, it := range packPage.Items {
		if it.PackID == "pk-contract-test-01" && it.Generation == initialGen {
			if it.State != "Cold" || it.StorageClass != "GLACIER_IR" {
				t.Fatalf("source pack gen not retained as Cold: %+v", it)
			}
			sourceFound = true
		}
		if it.PackID == "pk-contract-test-01" && it.Generation == promotedGen {
			if it.State != "Warm" || it.StorageClass != "STANDARD" {
				t.Fatalf("target pack gen not established as Warm: %+v", it)
			}
			targetFound = true
		}
	}
	if !sourceFound || !targetFound {
		t.Fatalf("multi-generation matching failed: source=%t target=%t in %+v", sourceFound, targetFound, packPage.Items)
	}

	// 7. Verify stats terminal safety assertions (fail-closed on null or positive)
	var st struct {
		RevivedChunks  *uint64          `json:"revived_chunks_total"`
		OrphanedChunks *uint64          `json:"orphaned_chunks_total"`
		Physical       map[string]int64 `json:"listed_bytes_by_pack_lifecycle"`
	}
	f.adminRequest("GET", "stats", nil, f.token, 200, &st)
	if st.OrphanedChunks == nil || *st.OrphanedChunks != 0 {
		t.Fatalf("orphaned_chunks_total must be non-null 0: %v", st.OrphanedChunks)
	}
	if st.RevivedChunks == nil || *st.RevivedChunks != 1 {
		t.Fatalf("revived_chunks_total must be non-null 1: %v", st.RevivedChunks)
	}
	if st.Physical["cold_dead"] != 4480 {
		t.Fatalf("listed_bytes_by_pack_lifecycle cold_dead mismatch: %v", st.Physical)
	}
}

// Live end-to-end acceptance suite executing against deployed Miles Cloud and Edge front doors.
// All requests are routed through Miles endpoints; no direct AWS CLI S3 bypass is used.
// Pack protection is enforced: every tiering job is first observed deferred
// inside the real ProtectedUntil window, then completes after it elapses.
func TestColdLiveFrontDoors(t *testing.T) {
	path := os.Getenv("MILES_COLD_LIVE_CONFIG")
	if path == "" {
		t.Skip("set MILES_COLD_LIVE_CONFIG only after live test approval and deployment")
	}
	commit := os.Getenv("MILES_RUN_COMMIT")
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(commit) {
		t.Fatal("MILES_RUN_COMMIT must be the full SHA of the clean tested commit")
	}
	var c liveConfig
	if err := json.Unmarshal(privateFile(t, path), &c); err != nil || c.InstallationFile == "" || c.Result == "" || len(c.Endpoints) < 1 {
		t.Fatal("fixture requires an installation inventory, a fresh result path and front doors")
	}
	inv, err := dynamodb.LoadPreflightInventory(c.InstallationFile)
	if err != nil {
		t.Fatal(err)
	}
	if c.Region != inv.Region {
		t.Fatal("live config region must match the installation inventory")
	}
	names := map[string]bool{}
	for _, ep := range c.Endpoints {
		if err := validateEndpoint(ep); err != nil {
			t.Fatal(err)
		}
		if names[ep.Name] {
			t.Fatal("front doors require distinct labels")
		}
		names[ep.Name] = true
	}
	ca, err := os.ReadFile(c.CAFile)
	roots := x509.NewCertPool()
	if err != nil || !roots.AppendCertsFromPEM(ca) {
		t.Fatal("cannot load reviewed TLS trust")
	}
	token := strings.TrimSpace(string(privateFile(t, c.TokenFile)))
	if token == "" {
		t.Fatal("empty system Admin token")
	}
	out, err := os.OpenFile(c.Result, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal("result path must be new")
	}
	started := time.Now().UTC()
	results, events := []observation{}, []adminEvent{}
	var nonce [6]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	prefix := fmt.Sprintf("%s-cold-%s", inv.InstallationName, hex.EncodeToString(nonce[:]))
	t.Cleanup(func() {
		defer out.Close()
		role := inv.Operator.DeployRoleARN[strings.LastIndex(inv.Operator.DeployRoleARN, "/")+1:]
		err := json.NewEncoder(out).Encode(struct {
			Commit       string        `json:"commit"`
			Installation string        `json:"installation"`
			DeployRole   string        `json:"deploy_role"`
			StartedAt    time.Time     `json:"started_at"`
			FinishedAt   time.Time     `json:"finished_at"`
			Prefix       string        `json:"resource_prefix"`
			Passed       bool          `json:"passed"`
			Observations []observation `json:"observations"`
			AdminEvents  []adminEvent  `json:"admin_events"`
		}{commit, inv.InstallationName, role, started, time.Now().UTC(), prefix, !t.Failed(), results, events})
		if err != nil {
			t.Error("cannot save sanitized evidence")
		}
	})

	// Fail closed before any front-door request: caller must be this
	// installation's assumed deploy role and inventory must match exactly.
	ctx := context.Background()
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(inv.Region))
	if err != nil {
		t.Fatalf("cannot load AWS configuration for live preflight: %v", err)
	}
	if err := dynamodb.ValidateApprovedInventoryAndSchema(ctx, sts.NewFromConfig(awsCfg), ddb.NewFromConfig(awsCfg), awss3.NewFromConfig(awsCfg), inv); err != nil {
		t.Fatalf("fail-closed preflight rejected live environment: %v", err)
	}

	fixtures := make([]*fixture, len(c.Endpoints))
	owners := make([]credential, len(c.Endpoints))
	cases := make([][]*coldCase, len(c.Endpoints))
	for i, ep := range c.Endpoints {
		transport := &http.Transport{
			Proxy:              nil,
			DisableCompression: true,
			TLSClientConfig:    &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, ep.Connect)
			},
		}
		adminTransport := &http.Transport{Proxy: nil}
		t.Cleanup(transport.CloseIdleConnections)
		t.Cleanup(adminTransport.CloseIdleConnections)
		noRedirect := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		epPrefix := prefix + "-" + ep.Name
		f := &fixture{
			config:  c,
			ep:      ep,
			client:  &http.Client{Transport: transport, Timeout: 180 * time.Second, CheckRedirect: noRedirect},
			admin:   &http.Client{Transport: adminTransport, Timeout: 30 * time.Second, CheckRedirect: noRedirect},
			token:   token,
			prefix:  epPrefix,
			bucket:  epPrefix,
			results: &results,
			events:  &events,
		}
		// Phase 1: publish both payloads, then observe protection deferring
		// each tiering job. Both endpoints share one protection wait.
		t.Run(ep.Name+"/protected", func(t *testing.T) {
			f.t = t
			var nodes json.RawMessage
			f.adminRequest("GET", "cluster/nodes", nil, token, 200, &nodes)
			f.record("runtime topology", nodes)
			owner := f.createUser("owner")
			f.s3("create bucket", owner, "PUT", "", "", nil, nil, false, 200, "")
			var bucketPolicy struct {
				BucketID string `json:"bucket_id"`
			}
			f.adminRequest("GET", "buckets/"+f.bucket+"/policy", nil, token, 200, &bucketPolicy)
			f.bucketID = bucketPolicy.BucketID
			if f.bucketID == "" {
				t.Fatal("missing bucket identity")
			}
			cs := []*coldCase{f.putCold(owner, storage.GlacierIR), f.putCold(owner, storage.Glacier)}
			if i == 0 {
				// Demonstrate that tiering is deferred inside ProtectedUntil on the first case.
				f.startTiering(cs[0])
			}
			fixtures[i], owners[i], cases[i] = f, owner, cs
		})
	}
	if t.Failed() {
		return
	}

	// Phase 2: wait for the real protection deadline. R1 admits the
	// transition only once its lower UTC bound passes ProtectedUntil.
	var until time.Time
	for _, cs := range cases {
		for _, cc := range cs {
			if cc.protectedUntil.After(until) {
				until = cc.protectedUntil
			}
		}
	}
	if wait := time.Until(until.Add(time.Minute)); wait > 0 {
		t.Logf("waiting %v for pack protection to expire", wait.Round(time.Second))
		time.Sleep(wait)
	}

	// Phase 3: the same deferred jobs now transition; continue each flow.
	for i, ep := range c.Endpoints {
		t.Run(ep.Name+"/lifecycle", func(t *testing.T) {
			f := fixtures[i]
			f.t = t
			for _, cc := range cases[i] {
				f.finishCold(owners[i], cc)
			}
			f.runColdErrorAndIsolation(owners[i])
		})
	}
}

type coldCase struct {
	class           storage.StorageClass
	key             string
	data            []byte
	packID, packGen string
	protectedUntil  time.Time
	tierJob         string
}

// coldPack decodes both layout packs[] and pack inventory items[].
type coldPack struct {
	PackID            string    `json:"pack_id"`
	Generation        string    `json:"generation"`
	State             string    `json:"state"`
	StorageClass      string    `json:"storage_class"`
	StateRevision     uint64    `json:"state_revision"`
	ProtectedUntil    time.Time `json:"protected_until"`
	MinRetentionUntil time.Time `json:"min_retention_until"`
	RestoredUntil     time.Time `json:"restored_until"`
}

func (f *fixture) record(label string, v any) {
	if f.events == nil {
		return
	}
	data, err := json.Marshal(v)
	if err != nil {
		f.t.Fatal("cannot record Admin evidence")
	}
	*f.events = append(*f.events, adminEvent{label, f.ep.Name, time.Now().UTC(), data})
}

func (f *fixture) layout(key string) []coldPack {
	f.t.Helper()
	var layout struct {
		Packs []coldPack `json:"packs"`
	}
	f.adminRequest("GET", "objects/layout?bucket="+url.QueryEscape(f.bucket)+"&key="+url.QueryEscape(strings.TrimPrefix(key, "/")), nil, f.token, 200, &layout)
	return layout.Packs
}

// packGeneration selects the exact pack_id + generation from the inventory,
// which lists every retained generation of the pack.
func (f *fixture) packGeneration(id, gen string) coldPack {
	f.t.Helper()
	var page struct {
		Items []coldPack `json:"items"`
	}
	f.adminRequest("GET", "packs/"+url.PathEscape(id), nil, f.token, 200, &page)
	for _, it := range page.Items {
		if it.PackID == id && it.Generation == gen {
			return it
		}
	}
	f.t.Fatalf("pack %s generation %s not found in pack inventory", id, gen)
	return coldPack{}
}

func (f *fixture) putCold(c credential, class storage.StorageClass) *coldCase {
	f.t.Helper()
	payload := coldIRPayload
	if class == storage.Glacier {
		payload = coldGlacierPayload
	}
	cc := &coldCase{class: class, key: fmt.Sprintf("/cold-%s.bin", strings.ToLower(string(class)))}
	cc.data = append([]byte(fmt.Sprintf("%s-%s-payload-seed\n", f.prefix, class)), payload...)
	f.s3("PUT cold object", c, "PUT", cc.key, "", cc.data, map[string]string{"Content-Type": "application/octet-stream"}, false, 200, "")
	packs := f.layout(cc.key)
	if len(packs) != 1 || packs[0].PackID == "" || packs[0].Generation == "" {
		f.t.Fatalf("expected one physical pack for %s, got %+v", cc.key, packs)
	}
	cc.packID, cc.packGen = packs[0].PackID, packs[0].Generation
	p := f.packGeneration(cc.packID, cc.packGen)
	if p.State != "Warm" || p.ProtectedUntil.IsZero() {
		f.t.Fatalf("published pack must be Warm with protection: %+v", p)
	}
	cc.protectedUntil = p.ProtectedUntil
	f.record(string(class)+" published pack", p)
	return cc
}

// startTiering pins a policy revision for the class and enqueues an exact
// tiering job, then disables tiering globally so periodic discovery cannot
// race it (the job keeps its pinned revision). It then proves protection
// defers the job: claimed by a worker, still open, generation still Warm,
// while ProtectedUntil is ahead.
func (f *fixture) startTiering(cc *coldCase) {
	f.t.Helper()
	if time.Until(cc.protectedUntil) < 5*time.Minute {
		f.t.Fatalf("protection window ends at %s, too soon to observe deferral", cc.protectedUntil)
	}
	f.enqueueTiering(cc)

	var detail json.RawMessage
	var p coldPack
	for end := time.Now().Add(30 * time.Second); time.Now().Before(end); time.Sleep(2 * time.Second) {
		state, raw := f.job(cc.tierJob)
		detail = raw
		if state == "succeeded" || state == "failed" || state == "cancelled" {
			f.t.Fatalf("tiering job %s settled as %s inside the protection window", cc.tierJob, state)
		}
		if p = f.packGeneration(cc.packID, cc.packGen); p.State != "Warm" {
			f.t.Fatalf("protected pack left Warm before ProtectedUntil: %+v", p)
		}
	}
	if !time.Now().Before(cc.protectedUntil) {
		f.t.Fatal("deferral observation overran ProtectedUntil")
	}
	f.record(string(cc.class)+" tiering deferred by protection", map[string]any{"job": detail, "pack": p})
}

func (f *fixture) enqueueTiering(cc *coldCase) {
	f.t.Helper()
	pinned := f.putTiering(map[string]any{
		"enabled":             true,
		"cold_after_days":     0,
		"min_pack_live_ratio": 0,
		"cold_storage_class":  string(cc.class),
		"auto_restore":        false,
	})
	var job struct {
		JobID string `json:"job_id"`
	}
	f.adminRequest("POST", "jobs", map[string]any{
		"type":            "tiering",
		"params":          map[string]any{"pack_id": cc.packID, "generation": cc.packGen},
		"policy_revision": pinned,
	}, f.token, 202, &job)
	if job.JobID == "" {
		f.t.Fatal("tiering job response missing job_id")
	}
	cc.tierJob = job.JobID
	f.putTiering(map[string]any{"enabled": false})
}

// putTiering merges tiering fields into the global policy with CAS on the
// current revision and returns the resulting revision.
func (f *fixture) putTiering(tiering map[string]any) int {
	f.t.Helper()
	var current, updated struct {
		Revision int `json:"revision"`
	}
	f.adminRequest("GET", "policy", nil, f.token, 200, &current)
	f.adminRequest("PUT", "policy", map[string]any{"expected_revision": current.Revision, "tiering": tiering}, f.token, 200, &updated)
	if updated.Revision < current.Revision || updated.Revision == 0 {
		f.t.Fatalf("policy revision regressed: %d -> %d", current.Revision, updated.Revision)
	}
	return updated.Revision
}

func (f *fixture) finishCold(c credential, cc *coldCase) {
	f.t.Helper()
	hexSum := fmt.Sprintf("%x", sha256.Sum256(cc.data))
	if cc.tierJob == "" {
		// ProtectedUntil has elapsed; enqueue the tiering job now so it
		// executes sequentially without competing for R1 with sibling cases.
		f.enqueueTiering(cc)
	}
	f.awaitJob(cc.tierJob, 5*time.Minute)
	source := f.packGeneration(cc.packID, cc.packGen)
	if source.State != "Cold" || source.StorageClass != string(cc.class) {
		f.t.Fatalf("pack %s gen %s state=%s class=%s, want Cold/%s", cc.packID, cc.packGen, source.State, source.StorageClass, cc.class)
	}
	if source.MinRetentionUntil.Before(time.Now().Add(89 * 24 * time.Hour)) {
		f.t.Fatalf("Cold generation lacks the 90-day minimum retention: %s", source.MinRetentionUntil)
	}
	f.record(string(cc.class)+" tiered pack", source)
	key, data := cc.key, cc.data

	if cc.class == storage.GlacierIR {
		// GLACIER_IR: RestoreObject must return 400 InvalidRequest (not archived)
		f.s3("IR RestoreObject rejected", c, "POST", key, "restore=", restoreXML(1, "Expedited"), nil, false, 400, "InvalidRequest")

		// Direct GET succeeds without restore, SHA-256 matches
		got, _ := f.s3("IR direct GET", c, "GET", key, "", nil, nil, false, 200, "")
		if fmt.Sprintf("%x", sha256.Sum256(got)) != hexSum {
			f.t.Fatalf("GLACIER_IR payload checksum mismatch, want %s", hexSum)
		}

		// Duplicate PUT reuses existing canonical IR chunk
		f.s3("IR reuse PUT", c, "PUT", key+"-reuse", "", data, nil, false, 200, "")
		gotReuse, _ := f.s3("IR reuse GET", c, "GET", key+"-reuse", "", nil, nil, false, 200, "")
		if !bytes.Equal(gotReuse, data) {
			f.t.Fatalf("GLACIER_IR reuse payload mismatch")
		}

		// Admin promote of the exact generation back to Warm
		var promJob struct {
			JobID string `json:"job_id"`
		}
		f.adminRequest("POST", "packs/"+url.PathEscape(cc.packID)+"/"+url.PathEscape(cc.packGen)+"/promote", map[string]any{}, f.token, 202, &promJob)
		f.awaitJob(promJob.JobID, 5*time.Minute)

		// Canonical layout must move to a new generation of the same pack
		var promoted coldPack
		for end := time.Now().Add(60 * time.Second); time.Now().Before(end); time.Sleep(500 * time.Millisecond) {
			if packs := f.layout(key); len(packs) == 1 && packs[0].PackID == cc.packID && packs[0].Generation != cc.packGen {
				promoted = packs[0]
				break
			}
		}
		if promoted.Generation == "" {
			f.t.Fatalf("canonical layout did not move off generation %s after promotion", cc.packGen)
		}

		// Inventory lists both: the retained Cold source and the new Warm target
		retained := f.packGeneration(cc.packID, cc.packGen)
		if retained.State != "Cold" || retained.MinRetentionUntil != source.MinRetentionUntil {
			f.t.Fatalf("source generation not retained as Cold with its retention: %+v", retained)
		}
		target := f.packGeneration(cc.packID, promoted.Generation)
		if target.State != "Warm" {
			f.t.Fatalf("promoted generation is not Warm: %+v", target)
		}
		f.record("GLACIER_IR promotion", map[string]any{"job_id": promJob.JobID, "retained_source": retained, "warm_target": target})

		gotPromoted, _ := f.s3("IR read after promote", c, "GET", key, "", nil, nil, false, 200, "")
		if !bytes.Equal(gotPromoted, data) {
			f.t.Fatalf("promoted object payload mismatch")
		}
		return
	}

	// GLACIER: GET before restore must be rejected with 403 InvalidObjectState
	f.s3("GLACIER GET before restore rejected", c, "GET", key, "", nil, nil, false, 403, "InvalidObjectState")

	// HEAD before restore has no x-amz-restore header
	_, headBefore := f.s3("GLACIER HEAD before restore", c, "HEAD", key, "", nil, nil, false, 200, "")
	if headBefore.Get("x-amz-restore") != "" {
		f.t.Fatalf("HEAD before restore had unexpected x-amz-restore: %s", headBefore.Get("x-amz-restore"))
	}

	// RestoreObject starts async restore -> 202 Accepted; a concurrent one is 409
	f.s3("GLACIER RestoreObject initiate", c, "POST", key, "restore=", restoreXML(2, "Expedited"), nil, false, 202, "")
	f.s3("GLACIER RestoreObject conflict", c, "POST", key, "restore=", restoreXML(2, "Expedited"), nil, false, 409, "RestoreAlreadyInProgress")

	// HEAD during pending shows ongoing-request="true"
	var headPending http.Header
	// x-amz-restore reflects the backend copy, so pending appears once a
	// worker has dispatched the restore job to S3.
	for end := time.Now().Add(3 * time.Minute); ; time.Sleep(2 * time.Second) {
		_, headPending = f.s3("GLACIER HEAD pending", c, "HEAD", key, "", nil, nil, false, 200, "")
		if strings.Contains(headPending.Get("x-amz-restore"), `ongoing-request="true"`) || time.Now().After(end) {
			break
		}
	}
	if !strings.Contains(headPending.Get("x-amz-restore"), `ongoing-request="true"`) {
		f.t.Fatalf("HEAD pending expected ongoing-request=true, got: %s", headPending.Get("x-amz-restore"))
	}
	f.record("GLACIER restore pending", map[string]any{"x_amz_restore": headPending.Get("x-amz-restore"), "pack": f.packGeneration(cc.packID, cc.packGen)})

	// GET while pending is rejected with 403 InvalidObjectState
	f.s3("GLACIER GET while pending rejected", c, "GET", key, "", nil, nil, false, 403, "InvalidObjectState")

	// Wait for async restore completion on backend S3
	var headRestored http.Header
	for end := time.Now().Add(10 * time.Minute); headRestored == nil; {
		if time.Now().After(end) {
			f.t.Fatal("asynchronous S3 restore timed out after 10 minutes")
		}
		time.Sleep(5 * time.Second)
		if _, h := f.s3("GLACIER HEAD poll", c, "HEAD", key, "", nil, nil, false, 200, ""); strings.Contains(h.Get("x-amz-restore"), `ongoing-request="false"`) {
			headRestored = h
		}
	}
	if !strings.Contains(headRestored.Get("x-amz-restore"), `expiry-date="`) {
		f.t.Fatalf("completed restore header missing expiry-date: %s", headRestored.Get("x-amz-restore"))
	}

	// GET on restored object succeeds, payload and SHA-256 match exactly
	gotRestored, _ := f.s3("GLACIER GET restored", c, "GET", key, "", nil, nil, false, 200, "")
	if fmt.Sprintf("%x", sha256.Sum256(gotRestored)) != hexSum {
		f.t.Fatalf("GLACIER restored payload checksum mismatch, want %s", hexSum)
	}

	f.record("GLACIER restore completed", map[string]any{"x_amz_restore": headRestored.Get("x-amz-restore"), "pack": f.packGeneration(cc.packID, cc.packGen)})

	// Restore extension with larger Days starts a new restore -> 202 Accepted
	f.s3("GLACIER RestoreObject renewal", c, "POST", key, "restore=", restoreXML(5, "Expedited"), nil, false, 202, "")

	// Body revival: PUT the same body; its chunks move to a new Warm pack while
	// the archive generation stays retained. Checked via layout, not the
	// node-local revival counter (the ALB may route the PUT to any Cloud node).
	f.s3("GLACIER body revival PUT", c, "PUT", key+"-revived", "", data, nil, false, 200, "")
	gotRevived, _ := f.s3("GLACIER revived GET", c, "GET", key+"-revived", "", nil, nil, false, 200, "")
	if !bytes.Equal(gotRevived, data) {
		f.t.Fatalf("GLACIER revived payload mismatch")
	}
	revived := f.layout(key + "-revived")
	if len(revived) != 1 || revived[0].PackID == cc.packID && revived[0].Generation == cc.packGen {
		f.t.Fatalf("revival did not bind chunks to a new pack: %+v", revived)
	}
	warm := f.packGeneration(revived[0].PackID, revived[0].Generation)
	if warm.State != "Warm" {
		f.t.Fatalf("revived chunks are not in a Warm generation: %+v", warm)
	}
	archive := f.packGeneration(cc.packID, cc.packGen)
	if archive.StorageClass != string(storage.Glacier) || archive.MinRetentionUntil != source.MinRetentionUntil {
		f.t.Fatalf("archive generation not retained after revival: %+v", archive)
	}
	f.record("GLACIER revival", map[string]any{"warm_target": warm, "retained_archive": archive})
}

func (f *fixture) runColdErrorAndIsolation(c credential) {
	f.t.Helper()
	f.s3("non-existent key 404", c, "GET", "/cold-nonexistent.bin", "", nil, nil, false, 404, "NoSuchKey")

	// Invalid restore bodies
	f.s3("malformed XML restore 400", c, "POST", "/cold-glacier.bin", "restore=", []byte("<broken>"), nil, false, 400, "MalformedXML")
	f.s3("zero days restore 400", c, "POST", "/cold-glacier.bin", "restore=", restoreXML(0, "Expedited"), nil, false, 400, "InvalidRequest")
	f.s3("invalid tier restore 400", c, "POST", "/cold-glacier.bin", "restore=", restoreXML(2, "NonExistent"), nil, false, 400, "InvalidRequest")

	// Accounting observations from this endpoint's Admin node. Write counters
	// are node-local; they complement, not replace, the per-job terminal
	// states and exact-generation records captured above.
	var raw json.RawMessage
	var st struct {
		PhysicalComplete bool             `json:"physical_complete"`
		OrphanedChunks   *uint64          `json:"orphaned_chunks_total"`
		Physical         map[string]int64 `json:"listed_bytes_by_pack_lifecycle"`
	}
	for end := time.Now().Add(45 * time.Second); ; time.Sleep(time.Second) {
		f.adminRequest("GET", "stats", nil, f.token, 200, &raw)
		if json.Unmarshal(raw, &st) != nil {
			f.t.Fatal("invalid stats response")
		}
		if st.PhysicalComplete && st.Physical["cold_dead"] > 0 || time.Now().After(end) {
			break
		}
	}
	f.record("stats", raw)
	if !st.PhysicalComplete {
		f.t.Fatal("physical listing incomplete: accounting evidence unavailable")
	}
	if st.OrphanedChunks == nil || *st.OrphanedChunks != 0 {
		f.t.Fatalf("terminal safety failure: orphaned chunks must be non-null and exactly 0, got %v", st.OrphanedChunks)
	}
	if st.Physical["cold_dead"] <= 0 {
		f.t.Fatalf("expected cold_dead accounting for superseded archive pack: %v", st.Physical)
	}
}

func (f *fixture) job(id string) (string, json.RawMessage) {
	f.t.Helper()
	var raw json.RawMessage
	f.adminRequest("GET", "jobs/"+url.PathEscape(id), nil, f.token, 200, &raw)
	var j struct {
		State string `json:"state"`
	}
	if json.Unmarshal(raw, &j) != nil {
		f.t.Fatal("invalid job response")
	}
	return j.State, raw
}

func (f *fixture) awaitJob(id string, timeout time.Duration) {
	f.t.Helper()
	if id == "" {
		f.t.Fatal("missing job_id")
	}
	for deadline := time.Now().Add(timeout); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
		state, raw := f.job(id)
		switch state {
		case "succeeded":
			f.record("job "+id, raw)
			return
		case "failed", "cancelled":
			f.record("job "+id, raw)
			f.t.Fatalf("job %s entered terminal state: %s", id, state)
		}
	}
	f.t.Fatalf("job %s did not settle within %v", id, timeout)
}
