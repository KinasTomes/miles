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
	"strings"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/api/s3"
	"github.com/KinasTomes/miles/internal/metadata/dynamodb"
	"github.com/KinasTomes/miles/internal/storage"
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
			"items": [{
				"pack_id": "pk-contract-test-01",
				"generation": "%s",
				"state": "%s",
				"storage_class": "%s",
				"min_retention_until": "2027-02-11T00:00:00Z"
			}]
		}`, gen, state, class)
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
			"physical_lifecycle_bytes": {"cold_dead": 4480}
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

	// 7. Verify stats terminal safety assertions (fail-closed on null or positive)
	var st struct {
		RevivedChunks  *uint64           `json:"revived_chunks_total"`
		OrphanedChunks *uint64           `json:"orphaned_chunks_total"`
		Physical       map[string]uint64 `json:"physical_lifecycle_bytes"`
	}
	f.adminRequest("GET", "stats", nil, f.token, 200, &st)
	if st.OrphanedChunks == nil || *st.OrphanedChunks != 0 {
		t.Fatalf("orphaned_chunks_total must be non-null 0: %v", st.OrphanedChunks)
	}
	if st.RevivedChunks == nil || *st.RevivedChunks != 1 {
		t.Fatalf("revived_chunks_total must be non-null 1: %v", st.RevivedChunks)
	}
	if st.Physical["cold_dead"] == 0 {
		t.Fatalf("cold_dead physical accounting missing: %+v", st.Physical)
	}
}

// Live end-to-end acceptance suite executing against deployed Miles Cloud and Edge front doors.
// All requests are routed through Miles endpoints; no direct AWS CLI S3 bypass is used.
func TestColdLiveFrontDoors(t *testing.T) {
	path := os.Getenv("MILES_COLD_LIVE_CONFIG")
	if path == "" {
		t.Skip("set MILES_COLD_LIVE_CONFIG only after live test approval and deployment")
	}
	var c liveConfig
	body := privateFile(t, path)
	if err := json.Unmarshal(body, &c); err != nil || c.Region == "" || c.Result == "" || len(c.Endpoints) < 1 {
		t.Fatal("fixture requires a region, a fresh result path and front doors")
	}
	for _, ep := range c.Endpoints {
		if err := validateEndpoint(ep); err != nil {
			t.Fatal(err)
		}
	}
	ca, err := os.ReadFile(c.CAFile)
	roots := x509.NewCertPool()
	if err != nil || !roots.AppendCertsFromPEM(ca) {
		t.Fatal("cannot load reviewed TLS trust")
	}
	out, err := os.OpenFile(c.Result, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal("result path must be new")
	}
	results := []observation{}
	var nonce [6]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	installation := c.InstallationName
	if installation == "" {
		installation = "miles-chien"
	}
	accountID := c.AccountID
	if accountID == "" {
		accountID = "499388132734"
	}
	preflightCfg := dynamodb.PreflightInventoryConfig{
		AccountID:        accountID,
		Region:           c.Region,
		InstallationName: installation,
		TablePrefix:      installation + "-",
		BucketName:       fmt.Sprintf("%s-data-%s-%s", installation, accountID, c.Region),
		RequiredTags: map[string]string{
			"Project": "Miles",
		},
	}
	if err := dynamodb.ValidateApprovedInventoryAndSchema(context.Background(), nil, nil, preflightCfg); err != nil {
		t.Fatalf("fail-closed preflight rejected environment parameters: %v", err)
	}
	prefix := fmt.Sprintf("%s-cold-%s", installation, hex.EncodeToString(nonce[:]))
	t.Cleanup(func() {
		defer out.Close()
		err := json.NewEncoder(out).Encode(struct {
			Prefix       string        `json:"resource_prefix"`
			Passed       bool          `json:"passed"`
			Observations []observation `json:"observations"`
		}{prefix, !t.Failed(), results})
		if err != nil {
			t.Error("cannot save sanitized evidence")
		}
	})

	token := strings.TrimSpace(string(privateFile(t, c.TokenFile)))
	if token == "" {
		t.Fatal("empty system Admin token")
	}

	for _, ep := range c.Endpoints {
		t.Run(ep.Name, func(t *testing.T) {
			epPrefix := prefix + "-" + ep.Name
			transport := &http.Transport{
				Proxy:              nil,
				DisableCompression: true,
				TLSClientConfig:    &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
				DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
					return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, ep.Connect)
				},
			}
			defer transport.CloseIdleConnections()

			adminTransport := &http.Transport{Proxy: nil}
			defer adminTransport.CloseIdleConnections()
			noRedirect := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

			f := &fixture{
				t:       t,
				config:  c,
				ep:      ep,
				client:  &http.Client{Transport: transport, Timeout: 180 * time.Second, CheckRedirect: noRedirect},
				admin:   &http.Client{Transport: adminTransport, Timeout: 30 * time.Second, CheckRedirect: noRedirect},
				token:   token,
				prefix:  epPrefix,
				bucket:  epPrefix,
				results: &results,
			}

			// Create dedicated user and logical bucket via S3 API
			owner := f.createUser("owner")
			f.s3("create bucket", owner, "PUT", "", "", nil, nil, false, 200, "")

			// Resolve bucket identity from Admin API
			var bucketPolicy struct {
				BucketID string `json:"bucket_id"`
			}
			f.adminRequest("GET", "buckets/"+f.bucket+"/policy", nil, token, 200, &bucketPolicy)
			f.bucketID = bucketPolicy.BucketID
			if f.bucketID == "" {
				t.Fatal("missing bucket identity")
			}
			f.grant("owner", true)

			// Discover current global policy revision dynamically
			var curPolicy struct {
				Revision int `json:"revision"`
			}
			f.adminRequest("GET", "policy", nil, token, 200, &curPolicy)

			// Scenario 1: GLACIER_IR flow with pack physical transition & promotion
			f.runColdScenario(owner, storage.GlacierIR, &curPolicy.Revision)

			// Scenario 2: GLACIER flow with async S3 restore observation & body revival
			f.runColdScenario(owner, storage.Glacier, &curPolicy.Revision)

			// Scenario 3: Error & terminal isolation validation (0 unknown writes, 0 protection leaks)
			f.runColdErrorAndIsolation(owner)
		})
	}
}

func (f *fixture) runColdScenario(c credential, class storage.StorageClass, polRev *int) {
	f.t.Helper()
	var data []byte
	if class == storage.GlacierIR {
		data = bytes.Clone(coldIRPayload)
	} else {
		data = bytes.Clone(coldGlacierPayload)
	}
	sum := sha256.Sum256(data)
	hexSum := hex.EncodeToString(sum[:])
	key := fmt.Sprintf("/cold-%s.bin", strings.ToLower(string(class)))

	// 1. PUT synthetic object through Miles S3 API
	f.s3("PUT cold object", c, "PUT", key, "", data, map[string]string{
		"Content-Type": "application/octet-stream",
	}, false, 200, "")

	// 2. Discover physical pack and generation via object layout (packs[].pack_id)
	var layout struct {
		Packs []struct {
			PackID       string `json:"pack_id"`
			Generation   string `json:"generation"`
			State        string `json:"state"`
			StorageClass string `json:"storage_class"`
		} `json:"packs"`
	}
	f.adminRequest("GET", "objects/layout?bucket="+url.QueryEscape(f.bucket)+"&key="+url.QueryEscape(strings.TrimPrefix(key, "/")), nil, f.token, 200, &layout)
	if len(layout.Packs) == 0 {
		f.t.Fatal("no physical pack mapped for object")
	}
	packID := layout.Packs[0].PackID
	packGen := layout.Packs[0].Generation

	// 3. Configure tiering policy using current pinned revision
	f.adminRequest("PUT", "policy", map[string]any{
		"expected_revision": *polRev,
		"tiering": map[string]any{
			"cold_after_days":     0,
			"min_pack_live_ratio": 0,
			"cold_storage_class":  string(class),
		},
	}, f.token, 200, nil)
	*polRev++

	// 4. Enqueue tiering job with object params and await physical transition
	var tierJob struct {
		JobID string `json:"job_id"`
	}
	f.adminRequest("POST", "jobs", map[string]any{
		"type":            "tiering",
		"params":          map[string]any{"pack_id": packID, "generation": packGen},
		"policy_revision": *polRev,
	}, f.token, 202, &tierJob)
	f.awaitJob(tierJob.JobID, 3*time.Minute)

	// 5. Inspect physical pack state after tiering (items array)
	var packPage struct {
		Items []struct {
			PackID       string `json:"pack_id"`
			Generation   string `json:"generation"`
			State        string `json:"state"`
			StorageClass string `json:"storage_class"`
		} `json:"items"`
	}
	f.adminRequest("GET", "packs/"+packID, nil, f.token, 200, &packPage)
	if len(packPage.Items) == 0 || packPage.Items[0].State != "Cold" || packPage.Items[0].StorageClass != string(class) {
		f.t.Fatalf("pack %s state=%s class=%s, want Cold/%s", packID, packPage.Items[0].State, packPage.Items[0].StorageClass, class)
	}

	if class == storage.GlacierIR {
		// 6. GLACIER_IR: RestoreObject must return 400 InvalidRequest (not archived)
		f.s3("IR RestoreObject rejected", c, "POST", key, "restore=", restoreXML(1, "Expedited"), nil, false, 400, "InvalidRequest")

		// 7. Direct GET succeeds without restore, SHA-256 matches
		got, _ := f.s3("IR direct GET", c, "GET", key, "", nil, nil, false, 200, "")
		gotSum := sha256.Sum256(got)
		if hex.EncodeToString(gotSum[:]) != hexSum {
			f.t.Fatalf("GLACIER_IR payload checksum mismatch: got %x, want %s", gotSum, hexSum)
		}

		// 8. Duplicate PUT reuses existing canonical IR chunk
		f.s3("IR reuse PUT", c, "PUT", key+"-reuse", "", data, nil, false, 200, "")
		gotReuse, _ := f.s3("IR reuse GET", c, "GET", key+"-reuse", "", nil, nil, false, 200, "")
		if !bytes.Equal(gotReuse, data) {
			f.t.Fatalf("GLACIER_IR reuse payload mismatch")
		}

		// 9. Admin promote pack back to Warm generation
		var promJob struct {
			JobID string `json:"job_id"`
		}
		f.adminRequest("POST", "packs/"+packID+"/"+packGen+"/promote", map[string]any{}, f.token, 202, &promJob)
		f.awaitJob(promJob.JobID, 3*time.Minute)

		f.adminRequest("GET", "packs/"+packID, nil, f.token, 200, &packPage)
		if len(packPage.Items) == 0 || packPage.Items[0].State != "Warm" {
			f.t.Fatalf("pack %s state=%s, want Warm after promotion", packID, packPage.Items[0].State)
		}

		// Verify layout after promotion advances generation
		f.adminRequest("GET", "objects/layout?bucket="+url.QueryEscape(f.bucket)+"&key="+url.QueryEscape(strings.TrimPrefix(key, "/")), nil, f.token, 200, &layout)
		if len(layout.Packs) == 0 || layout.Packs[0].Generation == packGen {
			f.t.Fatalf("generation did not advance after promotion: %s", layout.Packs[0].Generation)
		}

		gotPromoted, _ := f.s3("IR read after promote", c, "GET", key, "", nil, nil, false, 200, "")
		if !bytes.Equal(gotPromoted, data) {
			f.t.Fatalf("promoted object payload mismatch")
		}
	} else if class == storage.Glacier {
		// 6. GLACIER: GET before restore must be rejected with 403 InvalidObjectState
		f.s3("GLACIER GET before restore rejected", c, "GET", key, "", nil, nil, false, 403, "InvalidObjectState")

		// 7. HEAD before restore has no x-amz-restore header
		_, headBefore := f.s3("GLACIER HEAD before restore", c, "HEAD", key, "", nil, nil, false, 200, "")
		if headBefore.Get("x-amz-restore") != "" {
			f.t.Fatalf("HEAD before restore had unexpected x-amz-restore: %s", headBefore.Get("x-amz-restore"))
		}

		// 8. RestoreObject starts async restore -> 202 Accepted
		f.s3("GLACIER RestoreObject initiate", c, "POST", key, "restore=", restoreXML(2, "Expedited"), nil, false, 202, "")

		// 9. Concurrent RestoreObject returns 409 RestoreAlreadyInProgress
		f.s3("GLACIER RestoreObject conflict", c, "POST", key, "restore=", restoreXML(2, "Expedited"), nil, false, 409, "RestoreAlreadyInProgress")

		// 10. HEAD during pending shows ongoing-request="true"
		_, headPending := f.s3("GLACIER HEAD pending", c, "HEAD", key, "", nil, nil, false, 200, "")
		if !strings.Contains(headPending.Get("x-amz-restore"), `ongoing-request="true"`) {
			f.t.Fatalf("HEAD pending expected ongoing-request=true, got: %s", headPending.Get("x-amz-restore"))
		}

		// 11. GET while pending is rejected with 403 InvalidObjectState
		f.s3("GLACIER GET while pending rejected", c, "GET", key, "", nil, nil, false, 403, "InvalidObjectState")

		// 12. Wait for async restore completion on backend S3
		deadline := time.Now().Add(10 * time.Minute)
		restored := false
		var headRestored http.Header
		for time.Now().Before(deadline) {
			time.Sleep(5 * time.Second)
			_, h := f.s3("GLACIER HEAD poll", c, "HEAD", key, "", nil, nil, false, 200, "")
			if strings.Contains(h.Get("x-amz-restore"), `ongoing-request="false"`) {
				restored = true
				headRestored = h
				break
			}
		}
		if !restored {
			f.t.Fatal("asynchronous S3 restore timed out after 10 minutes")
		}
		if !strings.Contains(headRestored.Get("x-amz-restore"), `expiry-date="`) {
			f.t.Fatalf("completed restore header missing expiry-date: %s", headRestored.Get("x-amz-restore"))
		}

		// 13. GET on restored object succeeds, payload and SHA-256 match exactly
		gotRestored, _ := f.s3("GLACIER GET restored", c, "GET", key, "", nil, nil, false, 200, "")
		gotRestoredSum := sha256.Sum256(gotRestored)
		if hex.EncodeToString(gotRestoredSum[:]) != hexSum {
			f.t.Fatalf("GLACIER restored payload checksum mismatch: got %x, want %s", gotRestoredSum, hexSum)
		}

		// 14. Restore extension with larger Days succeeds
		f.s3("GLACIER RestoreObject renewal", c, "POST", key, "restore=", restoreXML(5, "Expedited"), nil, false, 202, "")

		// 15. Body revival: PUT same body into Warm generation, old archive kept for retention
		f.s3("GLACIER body revival PUT", c, "PUT", key+"-revived", "", data, nil, false, 200, "")
		gotRevived, _ := f.s3("GLACIER revived GET", c, "GET", key+"-revived", "", nil, nil, false, 200, "")
		if !bytes.Equal(gotRevived, data) {
			f.t.Fatalf("GLACIER revived payload mismatch")
		}
	}
}

func (f *fixture) runColdErrorAndIsolation(c credential) {
	f.t.Helper()
	// Unrestored archive read must be refused before any header/payload commit
	f.s3("non-existent key 404", c, "GET", "/cold-nonexistent.bin", "", nil, nil, false, 404, "NoSuchKey")

	// Invalid restore bodies
	f.s3("malformed XML restore 400", c, "POST", "/cold-glacier.bin", "restore=", []byte("<broken>"), nil, false, 400, "MalformedXML")
	f.s3("zero days restore 400", c, "POST", "/cold-glacier.bin", "restore=", restoreXML(0, "Expedited"), nil, false, 400, "InvalidRequest")
	f.s3("invalid tier restore 400", c, "POST", "/cold-glacier.bin", "restore=", restoreXML(2, "NonExistent"), nil, false, 400, "InvalidRequest")

	// Fail-closed terminal safety assertions
	var st struct {
		RevivedChunks  *uint64           `json:"revived_chunks_total"`
		OrphanedChunks *uint64           `json:"orphaned_chunks_total"`
		Physical       map[string]uint64 `json:"physical_lifecycle_bytes"`
	}
	f.adminRequest("GET", "stats", nil, f.token, 200, &st)
	if st.OrphanedChunks == nil || *st.OrphanedChunks != 0 {
		f.t.Fatalf("terminal safety failure: orphaned chunks must be non-null and exactly 0, got %v", st.OrphanedChunks)
	}
	if st.RevivedChunks == nil || *st.RevivedChunks == 0 {
		f.t.Fatalf("expected non-zero revived chunks after revival flow")
	}
	if st.Physical["cold_dead"] == 0 {
		f.t.Fatalf("expected cold_dead accounting for superseded archive pack")
	}
}

func (f *fixture) awaitJob(id string, timeout time.Duration) {
	f.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var j struct {
			JobID string `json:"job_id"`
			State string `json:"state"`
		}
		f.adminRequest("GET", "jobs/"+id, nil, f.token, 200, &j)
		switch j.State {
		case "succeeded":
			return
		case "failed", "cancelled":
			f.t.Fatalf("job %s entered terminal state: %s", id, j.State)
		default:
			time.Sleep(500 * time.Millisecond)
		}
	}
	f.t.Fatalf("job %s did not settle within %v", id, timeout)
}
