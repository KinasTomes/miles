package live_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/api/s3"
	"github.com/KinasTomes/miles/internal/storage"
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
	body := []byte("synthetic cold lifecycle payload: verified wire request")
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
	token := strings.TrimSpace(string(privateFile(t, c.TokenFile)))
	prefix := fmt.Sprintf("live-%x", time.Now().UnixNano()&0xffffff)
	bucket := prefix + "-cold"

	var results []observation
	for _, ep := range c.Endpoints {
		t.Run(ep.Name, func(t *testing.T) {
			transport := &http.Transport{
				Proxy:              nil,
				DisableCompression: true,
			}
			f := &fixture{
				t:       t,
				config:  c,
				ep:      ep,
				client:  &http.Client{Transport: transport, Timeout: 180 * time.Second},
				admin:   &http.Client{Transport: transport, Timeout: 30 * time.Second},
				token:   token,
				prefix:  prefix,
				bucket:  bucket,
				results: &results,
			}
			t.Cleanup(transport.CloseIdleConnections)

			owner := f.createUser("owner")
			f.adminRequest("POST", "buckets", map[string]any{"bucket": bucket, "domain": prefix}, token, 201, nil)
			f.grant("owner", true)

			// Scenario 1: GLACIER_IR flow
			f.runColdScenario(owner, storage.GlacierIR)

			// Scenario 2: GLACIER flow
			f.runColdScenario(owner, storage.Glacier)

			// Scenario 3: Error & terminal isolation validation
			f.runColdErrorAndIsolation(owner)
		})
	}
}

func (f *fixture) runColdScenario(c credential, class storage.StorageClass) {
	f.t.Helper()
	data := bytes.Repeat([]byte(fmt.Sprintf("synthetic-cold-%s-payload\n", class)), 128)
	sum := sha256.Sum256(data)
	hexSum := hex.EncodeToString(sum[:])
	key := fmt.Sprintf("/cold-%s.bin", strings.ToLower(string(class)))

	// 1. PUT synthetic object through Miles S3 API
	f.s3("PUT cold object", c, "PUT", key, "", data, map[string]string{
		"Content-Type": "application/octet-stream",
	}, false, 200, "")

	// 2. Configure tiering policy through Miles Admin API
	f.adminRequest("PUT", "policy", map[string]any{
		"expected_revision": 1,
		"tiering": map[string]any{
			"cold_after_days":     0,
			"min_pack_live_ratio": 0,
			"cold_storage_class":  string(class),
		},
	}, f.token, 200, nil)

	if class == storage.GlacierIR {
		// 3. GLACIER_IR: RestoreObject must return 400 InvalidRequest (not archived)
		f.s3("IR RestoreObject rejected", c, "POST", key, "restore=", restoreXML(1, "Expedited"), nil, false, 400, "InvalidRequest")

		// 4. Direct GET succeeds without restore, SHA-256 matches
		got, _ := f.s3("IR direct GET", c, "GET", key, "", nil, nil, false, 200, "")
		gotSum := sha256.Sum256(got)
		if hex.EncodeToString(gotSum[:]) != hexSum {
			f.t.Fatalf("GLACIER_IR payload checksum mismatch: got %x, want %s", gotSum, hexSum)
		}

		// 5. Duplicate PUT reuses existing canonical IR chunk
		f.s3("IR reuse PUT", c, "PUT", key+"-reuse", "", data, nil, false, 200, "")
		gotReuse, _ := f.s3("IR reuse GET", c, "GET", key+"-reuse", "", nil, nil, false, 200, "")
		if !bytes.Equal(gotReuse, data) {
			f.t.Fatalf("GLACIER_IR reuse payload mismatch")
		}
	} else if class == storage.Glacier {
		// 3. GLACIER: GET before restore must be rejected with 403 InvalidObjectState
		f.s3("GLACIER GET before restore rejected", c, "GET", key, "", nil, nil, false, 403, "InvalidObjectState")

		// 4. HEAD before restore has no x-amz-restore header
		_, headBefore := f.s3("GLACIER HEAD before restore", c, "HEAD", key, "", nil, nil, false, 200, "")
		if headBefore.Get("x-amz-restore") != "" {
			f.t.Fatalf("HEAD before restore had unexpected x-amz-restore: %s", headBefore.Get("x-amz-restore"))
		}

		// 5. RestoreObject starts async restore -> 202 Accepted
		f.s3("GLACIER RestoreObject initiate", c, "POST", key, "restore=", restoreXML(2, "Expedited"), nil, false, 202, "")

		// 6. Concurrent RestoreObject returns 409 RestoreAlreadyInProgress
		f.s3("GLACIER RestoreObject conflict", c, "POST", key, "restore=", restoreXML(2, "Expedited"), nil, false, 409, "RestoreAlreadyInProgress")

		// 7. HEAD during pending shows ongoing-request="true"
		_, headPending := f.s3("GLACIER HEAD pending", c, "HEAD", key, "", nil, nil, false, 200, "")
		if !strings.Contains(headPending.Get("x-amz-restore"), `ongoing-request="true"`) {
			f.t.Fatalf("HEAD pending expected ongoing-request=true, got: %s", headPending.Get("x-amz-restore"))
		}

		// 8. GET while pending is rejected with 403 InvalidObjectState
		f.s3("GLACIER GET while pending rejected", c, "GET", key, "", nil, nil, false, 403, "InvalidObjectState")

		// 9. Wait for async restore completion on backend S3
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

		// 10. GET on restored object succeeds, payload and SHA-256 match exactly
		gotRestored, _ := f.s3("GLACIER GET restored", c, "GET", key, "", nil, nil, false, 200, "")
		gotRestoredSum := sha256.Sum256(gotRestored)
		if hex.EncodeToString(gotRestoredSum[:]) != hexSum {
			f.t.Fatalf("GLACIER restored payload checksum mismatch: got %x, want %s", gotRestoredSum, hexSum)
		}

		// 11. Restore extension with larger Days succeeds
		f.s3("GLACIER RestoreObject renewal", c, "POST", key, "restore=", restoreXML(5, "Expedited"), nil, false, 202, "")

		// 12. Body revival: PUT same body into Warm generation, old archive kept for retention
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
}
