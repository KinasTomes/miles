package benchmark

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/benchmarkdata"
)

// A protocol fixture, never a Miles performance comparator. Real SDK calls run
// over HTTP so pagination, signing, body streaming and failures are exercised.
type s3Fixture struct {
	mu          sync.Mutex
	objects     map[string][]byte
	multiparts  map[string]map[int32][]byte
	calls       int
	active      int
	maxActive   int
	putCalls    int
	deleteCalls int
	listPages   int
	corrupt     bool
	failPut     bool
	badPage     bool
	extraKey    bool
	badHead     bool
	unsigned    bool
}

func (f *s3Fixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.calls++
	f.active++
	f.maxActive = max(f.maxActive, f.active)
	authHeader := r.Header.Get("Authorization")
	isTenantB := strings.Contains(authHeader, "tenant-b-key") || r.Header.Get("X-Amz-Security-Token") == "tenant-b-session"
	if isTenantB && strings.HasPrefix(r.URL.Path, "/test-bucket/") {
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusForbidden)
		_ = xml.NewEncoder(w).Encode(struct {
			XMLName xml.Name `xml:"Error"`
			Code    string   `xml:"Code"`
			Message string   `xml:"Message"`
		}{Code: "AccessDenied", Message: "Access Denied"})
		return
	}
	token := r.Header.Get("X-Amz-Security-Token")
	if !strings.HasPrefix(authHeader, "AWS4-HMAC-SHA256 ") || (token != "private-session" && token != "tenant-b-session") {
		f.unsigned = true
	}
	f.mu.Unlock()
	defer func() { f.mu.Lock(); f.active--; f.mu.Unlock() }()
	key := strings.TrimPrefix(r.URL.Path, "/test-bucket/")
	if strings.HasPrefix(r.URL.Path, "/test-bucket-b/") {
		key = strings.TrimPrefix(r.URL.Path, "/test-bucket-b/")
	}
	if r.URL.Query().Get("list-type") == "2" {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.listPages++
		prefix := r.URL.Query().Get("prefix")
		keys := []string{}
		for k := range f.objects {
			if strings.HasPrefix(k, prefix) {
				keys = append(keys, k)
			}
		}
		slices.Sort(keys)
		offset, _ := strconv.Atoi(r.URL.Query().Get("continuation-token"))
		type item struct {
			Key  string
			Size int
		}
		out := struct {
			XMLName               xml.Name `xml:"ListBucketResult"`
			IsTruncated           bool
			NextContinuationToken string
			Contents              []item
		}{}
		for i := offset; i < min(offset+2, len(keys)); i++ {
			out.Contents = append(out.Contents, item{keys[i], len(f.objects[keys[i]])})
		}
		if offset+2 < len(keys) {
			out.IsTruncated, out.NextContinuationToken = true, strconv.Itoa(offset+2)
			if f.badPage {
				out.NextContinuationToken = "2"
			}
		}
		if f.extraKey && len(keys) > 0 {
			out.Contents = append(out.Contents, item{prefix + "unowned", 0})
		}
		w.Header().Set("Content-Type", "application/xml")
		xml.NewEncoder(w).Encode(out)
		return
	}
	if r.Method == "POST" {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.URL.Query().Has("uploads") {
			if f.multiparts == nil {
				f.multiparts = make(map[string]map[int32][]byte)
			}
			uploadID := "mp-" + strconv.Itoa(len(f.multiparts)+1)
			f.multiparts[uploadID] = make(map[int32][]byte)
			w.Header().Set("Content-Type", "application/xml")
			xml.NewEncoder(w).Encode(struct {
				XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
				Bucket   string   `xml:"Bucket"`
				Key      string   `xml:"Key"`
				UploadId string   `xml:"UploadId"`
			}{Bucket: "test-bucket", Key: key, UploadId: uploadID})
			return
		}
		if r.URL.Query().Has("uploadId") {
			uploadID := r.URL.Query().Get("uploadId")
			partsMap, ok := f.multiparts[uploadID]
			if !ok {
				w.WriteHeader(404)
				return
			}
			var partNums []int
			for pn := range partsMap {
				partNums = append(partNums, int(pn))
			}
			slices.Sort(partNums)
			var combined []byte
			for _, pn := range partNums {
				combined = append(combined, partsMap[int32(pn)]...)
			}
			f.objects[key] = combined
			delete(f.multiparts, uploadID)
			w.Header().Set("Content-Type", "application/xml")
			xml.NewEncoder(w).Encode(struct {
				XMLName  xml.Name `xml:"CompleteMultipartUploadResult"`
				Location string   `xml:"Location"`
				Bucket   string   `xml:"Bucket"`
				Key      string   `xml:"Key"`
				ETag     string   `xml:"ETag"`
			}{Location: "http://localhost/" + key, Bucket: "test-bucket", Key: key, ETag: `"mp-combined-etag"`})
			return
		}
	}
	switch r.Method {
	case "PUT":
		time.Sleep(2 * time.Millisecond)
		b, _ := io.ReadAll(r.Body)
		sum := sha256.Sum256(b)
		if r.Header.Get("X-Amz-Checksum-Sha256") != base64.StdEncoding.EncodeToString(sum[:]) || r.ContentLength != int64(len(b)) || r.Header.Get("Content-Type") != "application/octet-stream" {
			w.WriteHeader(400)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.putCalls++
		if f.failPut {
			w.Header().Set("X-Amz-Request-Id", "FA11AB1E1234ABCD")
			w.WriteHeader(503)
			io.WriteString(w, "<Error><Code>SlowDown</Code><Message>private-secret http://private-host</Message></Error>")
			return
		}
		if r.URL.Query().Has("uploadId") {
			uploadID := r.URL.Query().Get("uploadId")
			pn, _ := strconv.Atoi(r.URL.Query().Get("partNumber"))
			if partsMap, ok := f.multiparts[uploadID]; ok {
				partsMap[int32(pn)] = b
			}
			w.Header().Set("ETag", `"part-etag-`+strconv.Itoa(pn)+`"`)
			w.Header().Set("x-amz-checksum-sha256", base64.StdEncoding.EncodeToString(sum[:]))
			w.WriteHeader(200)
			return
		}
		f.objects[key] = b
		w.WriteHeader(200)
	case "GET", "HEAD":
		f.mu.Lock()
		b, ok := f.objects[key]
		b = slices.Clone(b)
		f.mu.Unlock()
		if !ok {
			w.WriteHeader(404)
			return
		}
		size := len(b)
		if r.Method == "HEAD" && f.badHead {
			size++
		}
		w.Header().Set("Content-Length", strconv.Itoa(size))
		if strings.HasSuffix(key, ".gz") {
			w.Header().Set("Content-Encoding", "gzip")
		}
		if r.Method == "GET" {
			if f.corrupt {
				b[0] ^= 0xff
			}
			w.Write(b)
		}
	case "DELETE":
		f.mu.Lock()
		if r.URL.Query().Has("uploadId") {
			delete(f.multiparts, r.URL.Query().Get("uploadId"))
			f.mu.Unlock()
			w.WriteHeader(204)
			return
		}
		f.deleteCalls++
		delete(f.objects, key)
		f.mu.Unlock()
		w.WriteHeader(204)
	default:
		w.WriteHeader(501)
	}
}

func setup(t *testing.T) (Config, Options, *s3Fixture) {
	t.Helper()
	dir := t.TempDir()
	dataset := filepath.Join(dir, "dataset")
	if _, err := benchmarkdata.Generate(dataset, "smoke", "miles-w02-v1"); err != nil {
		t.Fatal(err)
	}
	cred := filepath.Join(dir, "credentials.json")
	if err := os.WriteFile(cred, []byte(`{"access_key_id":"private-access","secret_access_key":"private-secret","session_token":"private-session"}`), 0600); err != nil {
		t.Fatal(err)
	}
	f := &s3Fixture{objects: map[string][]byte{"outside-run/keep": []byte("unrelated")}, multiparts: make(map[string]map[int32][]byte)}
	s := httptest.NewServer(f)
	t.Cleanup(s.Close)
	c := Config{Target: "local-s3-smoke", Endpoint: s.URL, Bucket: "test-bucket", Region: "us-east-1", CredentialsFile: cred,
		Environment: Environment{Topology: "loopback protocol fixture", Host: "test", CPU: "test", Disk: "test", Network: "loopback", Metadata: "none", Encryption: "none", CacheState: "uncontrolled", Isolation: "fixture", AsyncWork: "none"}}
	return c, Options{Manifest: filepath.Join(dataset, "manifest.json"), Out: filepath.Join(dir, "artifact"), Concurrency: 3, ReadRepeats: 3}, f
}

func TestHTTPWorkloadArtifact(t *testing.T) {
	c, o, f := setup(t)
	r, err := Run(context.Background(), c, o)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "passed" || r.Cleanup != "logical_delete_verified_physical_retention_unknown" || f.unsigned || f.putCalls != 9 || f.deleteCalls != 9 || f.maxActive < 2 {
		t.Fatalf("run=%+v fixture=%+v", r.Phases, f)
	}
	if len(f.objects) != 1 || string(f.objects["outside-run/keep"]) != "unrelated" || f.listPages != 7 {
		t.Fatal("cleanup escaped prefix or list pagination was not followed")
	}
	if len(r.Phases) != 10 || r.Phases[1].Name != "put" || r.Phases[1].SuccessfulBytes != r.Dataset.TotalBytes || r.Phases[1].Samples != 9 {
		t.Fatalf("wrong phase accounting: %+v", r.Phases)
	}
	verified := 0
	for _, s := range r.Samples {
		if s.Operation == "GetObject" {
			if len(s.SHA256) != 64 || s.Error != "" {
				t.Fatalf("GET not verified: %+v", s)
			}
			verified++
		}
	}
	if verified != 36 {
		t.Fatalf("verified %d files, expected 4 full passes", verified)
	}
	b, err := os.ReadFile(filepath.Join(o.Out, "result.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{c.Endpoint, c.Bucket, c.CredentialsFile, "private-access", "private-secret", "private-session"} {
		if strings.Contains(string(b), secret) {
			t.Fatalf("private config leaked: %q", secret)
		}
	}
	var persisted Result
	if err = json.Unmarshal(b, &persisted); err != nil || persisted.Status != "passed" || persisted.ManifestSHA256 != r.ManifestSHA256 || persisted.FinishedAt.IsZero() {
		t.Fatal("artifact did not preserve completed run")
	}
	for path, mode := range map[string]os.FileMode{o.Out: 0700, filepath.Join(o.Out, "result.json"): 0600} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("wrong artifact permission on %s", path)
		}
	}
}

func TestFailuresAreRecordedAndRetainOnlyRunObjects(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*s3Fixture)
		code string
	}{
		{"corrupt GET", func(f *s3Fixture) { f.corrupt = true }, "sha256_mismatch"},
		{"failed PUT no retry", func(f *s3Fixture) { f.failPut = true }, "SlowDown"},
		{"wrong HEAD size", func(f *s3Fixture) { f.badHead = true }, "size_mismatch"},
		{"pagination loop", func(f *s3Fixture) { f.badPage = true }, "invalid_pagination"},
		{"extra listed key", func(f *s3Fixture) { f.extraKey = true }, "list_mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, o, f := setup(t)
			tc.set(f)
			r, err := Run(context.Background(), c, o)
			if err == nil || r.Status != "failed" || f.deleteCalls != 0 || f.putCalls != 9 {
				t.Fatalf("failure hidden or cleanup occurred: err=%v result=%+v", err, r.Phases)
			}
			found := false
			for _, s := range r.Samples {
				if s.Error == tc.code {
					found = true
				}
			}
			if !found || r.Phases[len(r.Phases)-1].Failed == 0 {
				t.Fatalf("missing failure sample %s: %+v", tc.code, r.Samples)
			}
			b, e := os.ReadFile(filepath.Join(o.Out, "result.json"))
			if e != nil || !strings.Contains(string(b), `"status": "failed"`) || strings.Contains(string(b), "private-secret") {
				t.Fatal("failure evidence missing or SDK error leaked")
			}
			if tc.code == "SlowDown" {
				if r.Phases[1].SuccessfulBytes != 0 {
					t.Fatal("failed PUT bytes counted as throughput")
				}
				for _, sample := range r.Samples {
					if sample.Error == "SlowDown" && (sample.HTTPStatus != 503 || sample.RequestID != "FA11AB1E1234ABCD") {
						t.Fatalf("missing HTTP diagnostic: %+v", sample)
					}
				}
			}
		})
	}
}

func TestInputFailureBeforeNetwork(t *testing.T) {
	for _, tc := range []string{"bad hash", "missing file", "outside symlink", "bad total", "duplicate", "traversal", "existing output", "shared credential", "nonlocal http", "bad concurrency", "canceled"} {
		t.Run(tc, func(t *testing.T) {
			c, o, f := setup(t)
			b, _ := os.ReadFile(o.Manifest)
			var m benchmarkdata.Manifest
			json.Unmarshal(b, &m)
			ctx := context.Background()
			switch tc {
			case "bad hash":
				os.WriteFile(filepath.Join(filepath.Dir(o.Manifest), m.Files[0].Path), make([]byte, m.Files[0].Bytes), 0600)
			case "missing file":
				os.Remove(filepath.Join(filepath.Dir(o.Manifest), m.Files[0].Path))
			case "outside symlink":
				source := filepath.Join(filepath.Dir(o.Manifest), m.Files[0].Path)
				outside := filepath.Join(t.TempDir(), "outside")
				os.Rename(source, outside)
				os.Symlink(outside, source)
			case "bad total":
				m.TotalBytes++
			case "duplicate":
				m.Files = append(m.Files, m.Files[0])
			case "traversal":
				m.Files[0].Path = "../outside"
			case "existing output":
				os.Mkdir(o.Out, 0700)
				os.WriteFile(filepath.Join(o.Out, "keep"), []byte("keep"), 0600)
			case "shared credential":
				os.Chmod(c.CredentialsFile, 0644)
			case "nonlocal http":
				c.Endpoint = "http://example.com"
			case "bad concurrency":
				o.Concurrency = 0
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			b, _ = json.Marshal(m)
			os.WriteFile(o.Manifest, b, 0600)
			if _, err := Run(ctx, c, o); err == nil || f.calls != 0 {
				t.Fatalf("invalid input reached network: err=%v calls=%d", err, f.calls)
			}
			if tc == "existing output" {
				b, _ := os.ReadFile(filepath.Join(o.Out, "keep"))
				if string(b) != "keep" {
					t.Fatal("existing output modified")
				}
			}
		})
	}
}

func TestCancellationWaitsForWorkers(t *testing.T) {
	c, o, _ := setup(t)
	started := make(chan struct{})
	released := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			io.WriteString(w, "<ListBucketResult><IsTruncated>false</IsTruncated></ListBucketResult>")
			return
		}
		select {
		case started <- struct{}{}:
		default:
		}
		<-released
		w.WriteHeader(503)
	}))
	defer s.Close()
	c.Endpoint = s.URL
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Result, 1)
	go func() { r, _ := Run(ctx, c, o); done <- r }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("PUT never started")
	}
	cancel()
	select {
	case r := <-done:
		close(released)
		if r.Status != "failed" || r.Phases[1].Failed != 9 || r.Cleanup != "objects_may_remain_on_failure" {
			t.Fatalf("canceled work not recorded: %+v", r.Phases)
		}
	case <-time.After(5 * time.Second):
		close(released)
		t.Fatal("caller cancellation did not abort SDK work")
	}
}

func TestPercentilesUseAllSamplesAndPhaseWallTime(t *testing.T) {
	r := Result{}
	err := finishPhase(&r, "example", time.Now().Add(-time.Second), []Sample{
		{LatencyNS: 2, PayloadBytes: 100}, {LatencyNS: 100, PayloadBytes: 9999, Error: "failed"}, {LatencyNS: 1, PayloadBytes: 200},
	})
	p := r.Phases[0]
	if err == nil || p.Samples != 3 || p.Failed != 1 || p.SuccessfulBytes != 300 || p.P50NS != 2 || p.P95NS != 100 || p.BytesPerSecond > 300 || p.BytesPerSecond < 290 {
		t.Fatalf("incorrect statistics %+v", p)
	}
}

func TestRedirectCannotForwardCredentials(t *testing.T) {
	c, o, _ := setup(t)
	forwarded := false
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded = true
	}))
	defer destination.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	c.Endpoint = origin.URL
	r, err := Run(context.Background(), c, o)
	if err == nil || r.Status != "failed" || forwarded {
		t.Fatal("redirect was followed or run reported success")
	}
}

func TestRetainObjectsForExternalStatistics(t *testing.T) {
	c, o, f := setup(t)
	o.RetainObjects = true
	result, err := Run(t.Context(), c, o)
	if err != nil || result.Cleanup != "retained_owned_prefix_for_observation" || f.deleteCalls != 0 || len(f.objects) != len(result.Dataset.Files)+1 {
		t.Fatal("retained run deleted data or hid cleanup", err, result.Cleanup, f.deleteCalls)
	}
}

func TestFailFastDisabledPreservesBaseline(t *testing.T) {
	for _, conc := range []int{1, 3} {
		t.Run(strconv.Itoa(conc), func(t *testing.T) {
			c, o, f := setup(t)
			o.Concurrency = conc
			o.FailFast = false
			f.failPut = true
			r, err := Run(context.Background(), c, o)
			if err == nil || r.Status != "failed" || f.putCalls != 9 {
				t.Fatalf("expected all 9 PUT calls with FailFast=false: putCalls=%d err=%v", f.putCalls, err)
			}
			putPhase := r.Phases[1]
			if putPhase.Name != "put" || putPhase.Samples != 9 || putPhase.Planned != 9 || putPhase.Dispatched != 9 || putPhase.Completed != 9 || putPhase.Skipped != 0 || putPhase.Failed != 9 {
				t.Fatalf("unexpected phase stats: %+v", putPhase)
			}
			if r.StopTrigger != "" {
				t.Fatalf("expected empty StopTrigger when FailFast=false, got %q", r.StopTrigger)
			}
		})
	}
}

func TestFailFastSequentialFirstFailure(t *testing.T) {
	c, o, f := setup(t)
	o.Concurrency = 1
	o.FailFast = true
	f.failPut = true
	r, err := Run(context.Background(), c, o)
	if err == nil || r.Status != "failed" {
		t.Fatalf("expected failure, got err=%v result=%+v", err, r)
	}
	if f.putCalls != 1 {
		t.Fatalf("expected exactly 1 PUT call on FailFast with Concurrency=1, got %d", f.putCalls)
	}
	putPhase := r.Phases[1]
	if putPhase.Name != "put" || putPhase.Samples != 1 || putPhase.Planned != 9 || putPhase.Dispatched != 1 || putPhase.Completed != 1 || putPhase.Skipped != 8 || putPhase.Failed != 1 {
		t.Fatalf("unexpected phase stats: %+v", putPhase)
	}
	if putPhase.StopTrigger != "SlowDown" || r.StopTrigger != "SlowDown" {
		t.Fatalf("unexpected StopTrigger: phase=%q result=%q", putPhase.StopTrigger, r.StopTrigger)
	}
	if len(r.Phases) != 2 {
		t.Fatalf("expected only preflight-list and put phases, got %d phases", len(r.Phases))
	}
}

func TestFailFastConcurrentInFlightDrain(t *testing.T) {
	c, o, _ := setup(t)
	o.Concurrency = 4
	o.FailFast = true

	var mu sync.Mutex
	putCount := 0
	inFlight := 0
	maxInFlight := 0

	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Query().Get("list-type") == "2" {
			w.Header().Set("Content-Type", "application/xml")
			io.WriteString(w, "<ListBucketResult><IsTruncated>false</IsTruncated></ListBucketResult>")
			return
		}
		if r.Method == "PUT" {
			mu.Lock()
			putCount++
			callIndex := putCount
			inFlight++
			if inFlight > maxInFlight {
				maxInFlight = inFlight
			}
			mu.Unlock()

			defer func() {
				mu.Lock()
				inFlight--
				mu.Unlock()
			}()

			// Call 2 fails with 503 clock_uncertainty_exceeded.
			// Wait until all 4 concurrent workers have dispatched before call 2 fails,
			// ensuring that calls 1, 3, 4 are truly in-flight when fail-fast triggers.
			if callIndex == 2 {
				for i := 0; i < 50; i++ {
					mu.Lock()
					cnt := putCount
					mu.Unlock()
					if cnt >= 4 {
						break
					}
					time.Sleep(2 * time.Millisecond)
				}
				w.Header().Set("X-Amz-Request-Id", "FAILFAST12345678")
				w.WriteHeader(503)
				io.WriteString(w, "<Error><Code>clock_uncertainty_exceeded</Code><Message>clock drift</Message></Error>")
				return
			}

			time.Sleep(30 * time.Millisecond)
			w.WriteHeader(200)
			return
		}
		w.WriteHeader(200)
	}))
	defer s.Close()
	c.Endpoint = s.URL

	r, err := Run(context.Background(), c, o)
	if err == nil || r.Status != "failed" {
		t.Fatalf("expected run failure, got err=%v", err)
	}

	mu.Lock()
	totalPuts := putCount
	mu.Unlock()

	// With Concurrency=4 and failure on call 2, at most 4 jobs were dispatched.
	// Jobs 5..9 should NEVER be dispatched!
	if totalPuts != 4 {
		t.Fatalf("expected exactly 4 dispatched PUTs, got %d", totalPuts)
	}

	putPhase := r.Phases[1]
	if putPhase.Name != "put" {
		t.Fatalf("expected put phase at index 1, got %s", putPhase.Name)
	}
	if putPhase.Planned != 9 || putPhase.Dispatched != 4 || putPhase.Completed != 4 || putPhase.Skipped != 5 || putPhase.Failed != 1 {
		t.Fatalf("wrong accounting: planned=%d dispatched=%d completed=%d skipped=%d failed=%d",
			putPhase.Planned, putPhase.Dispatched, putPhase.Completed, putPhase.Skipped, putPhase.Failed)
	}
	if putPhase.StopTrigger != "clock_uncertainty_exceeded" || r.StopTrigger != "clock_uncertainty_exceeded" {
		t.Fatalf("wrong StopTrigger: phase=%q result=%q", putPhase.StopTrigger, r.StopTrigger)
	}

	// Verify in-flight requests that completed successfully had their bytes counted!
	if putPhase.SuccessfulBytes <= 0 {
		t.Fatalf("expected in-flight successful bytes > 0, got %d", putPhase.SuccessfulBytes)
	}

	// Verify artifact file permission is 0600
	info, err := os.Stat(filepath.Join(o.Out, "result.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("wrong artifact permission: info=%v perm=%v", info, info.Mode().Perm())
	}
}

func TestFailFastFailureOnLastJob(t *testing.T) {
	c, o, _ := setup(t)
	o.Concurrency = 3
	o.FailFast = true

	var mu sync.Mutex
	putCount := 0

	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Query().Get("list-type") == "2" {
			w.Header().Set("Content-Type", "application/xml")
			io.WriteString(w, "<ListBucketResult><IsTruncated>false</IsTruncated></ListBucketResult>")
			return
		}
		if r.Method == "PUT" {
			mu.Lock()
			putCount++
			callIndex := putCount
			mu.Unlock()

			if callIndex == 9 {
				w.Header().Set("X-Amz-Request-Id", "LASTJOBFAIL12345")
				w.WriteHeader(503)
				io.WriteString(w, "<Error><Code>SlowDown</Code><Message>last job failed</Message></Error>")
				return
			}
			w.WriteHeader(200)
			return
		}
		w.WriteHeader(200)
	}))
	defer s.Close()
	c.Endpoint = s.URL

	r, err := Run(context.Background(), c, o)
	if err == nil || r.Status != "failed" {
		t.Fatalf("expected failure on last job, got err=%v", err)
	}

	putPhase := r.Phases[1]
	if putPhase.Planned != 9 || putPhase.Dispatched != 9 || putPhase.Completed != 9 || putPhase.Skipped != 0 || putPhase.Failed != 1 {
		t.Fatalf("wrong accounting for failure on last job: %+v", putPhase)
	}
	if putPhase.StopTrigger != "SlowDown" || r.StopTrigger != "SlowDown" {
		t.Fatalf("wrong stop trigger: phase=%q result=%q", putPhase.StopTrigger, r.StopTrigger)
	}
}

func TestFailFastGetMismatchStopsFurtherReadPasses(t *testing.T) {
	c, o, f := setup(t)
	o.Concurrency = 2
	o.FailFast = true
	o.ReadRepeats = 3
	f.corrupt = true // will corrupt GET payloads

	r, err := Run(context.Background(), c, o)
	if err == nil || r.Status != "failed" {
		t.Fatalf("expected run failure due to corrupt GET, got err=%v", err)
	}

	// PUT succeeded
	if r.Phases[1].Failed != 0 {
		t.Fatalf("PUT should have succeeded, got failed=%d", r.Phases[1].Failed)
	}

	// verify-get failed with sha256_mismatch and triggered fail-fast
	var verifyGetPhase *Phase
	for i := range r.Phases {
		if r.Phases[i].Name == "verify-get" {
			verifyGetPhase = &r.Phases[i]
			break
		}
	}
	if verifyGetPhase == nil {
		t.Fatal("missing verify-get phase")
	}
	if verifyGetPhase.StopTrigger != "sha256_mismatch" || r.StopTrigger != "sha256_mismatch" {
		t.Fatalf("wrong StopTrigger: phase=%q result=%q", verifyGetPhase.StopTrigger, r.StopTrigger)
	}

	// Ensure no repeat-get phases were run!
	for _, p := range r.Phases {
		if strings.HasPrefix(p.Name, "repeat-get") {
			t.Fatalf("repeat-get phase should not have run: %s", p.Name)
		}
	}
}

func TestFailFastIndeterminatePut(t *testing.T) {
	c, o, _ := setup(t)
	o.Concurrency = 1
	o.FailFast = true

	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Query().Get("list-type") == "2" {
			w.Header().Set("Content-Type", "application/xml")
			io.WriteString(w, "<ListBucketResult><IsTruncated>false</IsTruncated></ListBucketResult>")
			return
		}
		if r.Method == "PUT" {
			// Hijack and abruptly close the TCP connection to simulate network drop / status 0
			hj, ok := w.(http.Hijacker)
			if !ok {
				http.Error(w, "cannot hijack", 500)
				return
			}
			conn, _, _ := hj.Hijack()
			conn.Close()
			return
		}
		w.WriteHeader(200)
	}))
	defer s.Close()
	c.Endpoint = s.URL

	r, err := Run(context.Background(), c, o)
	if err == nil || r.Status != "failed" {
		t.Fatalf("expected run failure, got err=%v", err)
	}

	if !r.NeedsReconciliation {
		t.Fatal("expected result.NeedsReconciliation to be true")
	}
	if r.Cleanup != "objects_may_remain_indeterminate_outcome" {
		t.Fatalf("expected Cleanup='objects_may_remain_indeterminate_outcome', got %q", r.Cleanup)
	}

	foundIndeterminate := false
	for _, s := range r.Samples {
		if s.Operation == "PutObject" && s.Indeterminate {
			foundIndeterminate = true
			if s.Reconciliation != "needs_reconciliation" {
				t.Fatalf("expected sample.Reconciliation='needs_reconciliation', got %q", s.Reconciliation)
			}
			if s.HTTPStatus != 0 {
				t.Fatalf("expected HTTPStatus=0 for abrupt drop, got %d", s.HTTPStatus)
			}
		}
	}
	if !foundIndeterminate {
		t.Fatal("expected at least one PutObject sample marked indeterminate")
	}
}

func TestFailFastHighConcurrencyRace(t *testing.T) {
	c, o, _ := setup(t)
	o.Concurrency = 8
	o.FailFast = true

	var mu sync.Mutex
	putCount := 0

	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Query().Get("list-type") == "2" {
			w.Header().Set("Content-Type", "application/xml")
			io.WriteString(w, "<ListBucketResult><IsTruncated>false</IsTruncated></ListBucketResult>")
			return
		}
		if r.Method == "PUT" {
			mu.Lock()
			putCount++
			idx := putCount
			mu.Unlock()

			if idx == 3 {
				w.WriteHeader(503)
				io.WriteString(w, "<Error><Code>SlowDown</Code></Error>")
				return
			}
			time.Sleep(5 * time.Millisecond)
			w.WriteHeader(200)
			return
		}
		w.WriteHeader(200)
	}))
	defer s.Close()
	c.Endpoint = s.URL

	r, err := Run(context.Background(), c, o)
	if err == nil || r.Status != "failed" {
		t.Fatalf("expected run failure, got err=%v", err)
	}

	putPhase := r.Phases[1]
	if putPhase.Dispatched+putPhase.Skipped != putPhase.Planned {
		t.Fatalf("planned != dispatched + skipped: %+v", putPhase)
	}
	if putPhase.Completed != putPhase.Dispatched {
		t.Fatalf("completed != dispatched: %+v", putPhase)
	}
}

func TestFailFastSIGINTDoesNotAbortInFlightPUTUntilServerSettles(t *testing.T) {
	c, o, _ := setup(t)
	o.Concurrency = 1
	o.FailFast = true

	started := make(chan struct{}, 1)
	var serverFinished atomic.Bool

	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Query().Get("list-type") == "2" {
			w.Header().Set("Content-Type", "application/xml")
			io.WriteString(w, "<ListBucketResult><IsTruncated>false</IsTruncated></ListBucketResult>")
			return
		}
		if r.Method == "PUT" {
			select {
			case started <- struct{}{}:
			default:
			}
			time.Sleep(60 * time.Millisecond)
			serverFinished.Store(true)
			w.WriteHeader(200)
			return
		}
		w.WriteHeader(200)
	}))
	defer s.Close()
	c.Endpoint = s.URL

	ctx, cancel := context.WithCancel(context.Background())
	type runRes struct {
		r   Result
		err error
	}
	done := make(chan runRes, 1)
	go func() {
		res, err := Run(ctx, c, o)
		done <- runRes{res, err}
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("PUT never started")
	}

	// Cancel parent context while PUT is in-flight
	cancel()

	var result runRes
	select {
	case result = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run timed out after cancel")
	}

	// Crucial assertion: server handler finished BEFORE Run returned
	if !serverFinished.Load() {
		t.Fatal("Run returned before in-flight server PUT finished (graceful drain was bypassed)")
	}

	// Interrupted run must be recorded as failed, not passed
	if result.r.Status != "failed" || !errors.Is(result.err, ErrInterrupted) {
		t.Fatalf("expected Status='failed' and ErrInterrupted, got status=%q err=%v", result.r.Status, result.err)
	}
	if result.r.Error != "interrupted" || result.r.StopTrigger != "interrupted" {
		t.Fatalf("wrong error/StopTrigger: error=%q stopTrigger=%q", result.r.Error, result.r.StopTrigger)
	}

	// The PUT settled with 200 OK, so no indeterminate outcome
	if result.r.NeedsReconciliation {
		t.Fatal("settled in-flight PUT should not demand reconciliation")
	}
	if result.r.Cleanup == "objects_may_remain_indeterminate_outcome" {
		t.Fatalf("unexpected cleanup state: %q", result.r.Cleanup)
	}

	// Subsequent phases (head, verify-get, delete) must NOT have run
	if len(result.r.Phases) != 2 {
		t.Fatalf("expected exactly 2 phases (preflight-list and put), got %d phases", len(result.r.Phases))
	}

	putPhase := result.r.Phases[1]
	if putPhase.Dispatched != 1 || putPhase.Completed != 1 || putPhase.Failed != 0 || putPhase.Skipped != 8 {
		t.Fatalf("wrong accounting: %+v", putPhase)
	}
}

func TestFailFastSIGINTEmergencyDrainTimeoutEscalation(t *testing.T) {
	c, o, _ := setup(t)
	o.Concurrency = 1
	o.FailFast = true
	o.DrainTimeout = 50 * time.Millisecond

	started := make(chan struct{}, 1)
	var (
		releaseServer     = make(chan struct{})
		releaseServerOnce sync.Once
	)
	safeRelease := func() {
		releaseServerOnce.Do(func() {
			close(releaseServer)
		})
	}
	defer safeRelease()

	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Query().Get("list-type") == "2" {
			w.Header().Set("Content-Type", "application/xml")
			io.WriteString(w, "<ListBucketResult><IsTruncated>false</IsTruncated></ListBucketResult>")
			return
		}
		if r.Method == "PUT" {
			select {
			case started <- struct{}{}:
			default:
			}
			// Hang indefinitely until released or client disconnects
			select {
			case <-releaseServer:
			case <-r.Context().Done():
			}
			return
		}
		w.WriteHeader(200)
	}))
	defer s.Close()
	c.Endpoint = s.URL

	ctx, cancel := context.WithCancel(context.Background())
	type runRes struct {
		r   Result
		err error
	}
	done := make(chan runRes, 1)
	go func() {
		res, err := Run(ctx, c, o)
		done <- runRes{res, err}
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("PUT never started")
	}

	cancel()

	var result runRes
	select {
	case result = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("emergency drain timeout did not terminate in-flight request")
	}
	safeRelease()

	if result.r.Status != "failed" || !errors.Is(result.err, ErrInterrupted) {
		t.Fatalf("expected Status='failed' and ErrInterrupted, got status=%q err=%v", result.r.Status, result.err)
	}
	if !result.r.NeedsReconciliation {
		t.Fatal("expected NeedsReconciliation=true after emergency drain abort")
	}
	if result.r.Cleanup != "objects_may_remain_indeterminate_outcome" {
		t.Fatalf("expected Cleanup='objects_may_remain_indeterminate_outcome', got %q", result.r.Cleanup)
	}

	// Verify sample was recorded as indeterminate
	foundIndeterminate := false
	for _, sample := range result.r.Samples {
		if sample.Operation == "PutObject" && sample.Indeterminate {
			foundIndeterminate = true
			if sample.Reconciliation != "needs_reconciliation" {
				t.Fatalf("expected Reconciliation='needs_reconciliation', got %q", sample.Reconciliation)
			}
		}
	}
	if !foundIndeterminate {
		t.Fatal("expected at least one PutObject sample marked indeterminate")
	}
}

func TestFailFastSIGINTAllInFlightSucceedFailsBenchmark(t *testing.T) {
	c, o, _ := setup(t)
	o.Concurrency = 4
	o.FailFast = true

	var putDispatched atomic.Int32
	started := make(chan struct{}, 1)

	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Query().Get("list-type") == "2" {
			w.Header().Set("Content-Type", "application/xml")
			io.WriteString(w, "<ListBucketResult><IsTruncated>false</IsTruncated></ListBucketResult>")
			return
		}
		if r.Method == "PUT" {
			d := putDispatched.Add(1)
			if d == 1 {
				select {
				case started <- struct{}{}:
				default:
				}
			}
			time.Sleep(30 * time.Millisecond)
			w.WriteHeader(200)
			return
		}
		w.WriteHeader(200)
	}))
	defer s.Close()
	c.Endpoint = s.URL

	ctx, cancel := context.WithCancel(context.Background())
	type runRes struct {
		r   Result
		err error
	}
	done := make(chan runRes, 1)
	go func() {
		res, err := Run(ctx, c, o)
		done <- runRes{res, err}
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("PUT never started")
	}

	cancel()

	var result runRes
	select {
	case result = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run timed out after cancel")
	}

	// Crucial invariant: Even though 0 PUTs failed (all in-flight completed with 200 OK),
	// the benchmark MUST still fail with "interrupted", never report "passed".
	if result.r.Status != "failed" {
		t.Fatalf("interrupted benchmark must have Status='failed', got %q", result.r.Status)
	}
	if !errors.Is(result.err, ErrInterrupted) {
		t.Fatalf("expected ErrInterrupted, got %v", result.err)
	}
	if result.r.Error != "interrupted" || result.r.StopTrigger != "interrupted" {
		t.Fatalf("expected Error and StopTrigger to be 'interrupted', got error=%q stopTrigger=%q", result.r.Error, result.r.StopTrigger)
	}

	putPhase := result.r.Phases[1]
	if putPhase.Failed != 0 {
		t.Fatalf("expected 0 failed samples in putPhase, got %d", putPhase.Failed)
	}
	if putPhase.Dispatched == 0 || putPhase.Completed != putPhase.Dispatched {
		t.Fatalf("dispatched=%d completed=%d", putPhase.Dispatched, putPhase.Completed)
	}
	if putPhase.Skipped == 0 || putPhase.Dispatched+putPhase.Skipped != putPhase.Planned {
		t.Fatalf("skipped=%d dispatched=%d planned=%d", putPhase.Skipped, putPhase.Dispatched, putPhase.Planned)
	}

	// Verify subsequent phases (head, verify-get, repeat-get, delete) did NOT run
	for _, p := range result.r.Phases {
		if p.Name != "preflight-list" && p.Name != "put" {
			t.Fatalf("subsequent phase %q should not have executed", p.Name)
		}
	}
}

func TestFailFastAdversarialStopConcurrentWithJobReadiness(t *testing.T) {
	c, o, _ := setup(t)
	o.Concurrency = 3
	o.FailFast = true

	var (
		mu                sync.Mutex
		arrivedCount      int
		threeArrived      = make(chan struct{})
		threeArrivedOnce  sync.Once
		releaseInFlight   = make(chan struct{})
		releaseOnce       sync.Once
		postStopAttempted atomic.Bool
		postStopCount     atomic.Int32
	)
	safeRelease := func() {
		releaseOnce.Do(func() { close(releaseInFlight) })
	}
	defer safeRelease()

	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Query().Get("list-type") == "2" {
			w.Header().Set("Content-Type", "application/xml")
			io.WriteString(w, "<ListBucketResult><IsTruncated>false</IsTruncated></ListBucketResult>")
			return
		}
		if r.Method == "PUT" {
			key := r.URL.Path

			mu.Lock()
			arrivedCount++
			count := arrivedCount
			if count == 3 {
				threeArrivedOnce.Do(func() { close(threeArrived) })
			}
			mu.Unlock()

			// Check which file this PUT is for based on manifest ordering
			if strings.HasSuffix(key, "low-entropy-v00.bin") {
				// Barrier: wait for all 3 concurrent PUTs to arrive at server
				// before returning 503 to latch stop.
				select {
				case <-threeArrived:
				case <-releaseInFlight:
				case <-r.Context().Done():
				}
				w.WriteHeader(503)
				io.WriteString(w, "<Error><Code>SlowDown</Code><Message>rate limited</Message></Error>")
				return
			}

			if strings.HasSuffix(key, "compressed-identical-v00.gz") || strings.HasSuffix(key, "random-distinct-v00.bin") {
				// Jobs 1 & 2: in-flight requests that hold until released
				select {
				case <-releaseInFlight:
				case <-r.Context().Done():
				}
				w.WriteHeader(200)
				return
			}

			// Any request for other files represents a post-stop leak!
			postStopAttempted.Store(true)
			postStopCount.Add(1)
			w.WriteHeader(500)
			return
		}
		w.WriteHeader(200)
	}))
	defer s.Close()
	c.Endpoint = s.URL

	type runRes struct {
		r   Result
		err error
	}
	done := make(chan runRes, 1)
	go func() {
		res, err := Run(context.Background(), c, o)
		done <- runRes{res, err}
	}()

	// Wait until all 3 initial PUTs have arrived at the server
	select {
	case <-threeArrived:
	case <-time.After(5 * time.Second):
		t.Fatal("initial 3 concurrent PUTs never arrived")
	}

	// At this point:
	// - Job 0 returns 503 and latches stop in Worker 0.
	// - Worker 0 finishes job 0 and becomes ready to claim the next job.
	// - Workers 1 & 2 are still in-flight holding on the server.
	// Give worker 0 ample time to attempt claiming a job if there were any race.
	time.Sleep(30 * time.Millisecond)

	if postStopAttempted.Load() {
		t.Fatalf("concurrency race: %d post-stop operations started while in-flight drain was active", postStopCount.Load())
	}

	// Release in-flight operations 1 and 2 to drain
	safeRelease()

	var res runRes
	select {
	case res = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run timed out waiting for drain")
	}

	// Verify no post-stop operation ever started
	if postStopAttempted.Load() {
		t.Fatalf("concurrency race: %d post-stop operations started after stop latched", postStopCount.Load())
	}

	if res.r.Status != "failed" {
		t.Fatalf("expected Status='failed', got %q", res.r.Status)
	}
	if res.r.StopTrigger != "SlowDown" {
		t.Fatalf("expected StopTrigger='SlowDown', got %q", res.r.StopTrigger)
	}

	putPhase := res.r.Phases[1]
	if putPhase.Name != "put" {
		t.Fatalf("expected put phase, got %q", putPhase.Name)
	}

	// Verify exact accounting invariants required by Gate F:
	// planned = dispatched + skipped
	// completed = dispatched
	if putPhase.Planned != 9 {
		t.Fatalf("expected Planned=9, got %d", putPhase.Planned)
	}
	if putPhase.Dispatched != 3 {
		t.Fatalf("expected Dispatched=3, got %d", putPhase.Dispatched)
	}
	if putPhase.Completed != 3 {
		t.Fatalf("expected Completed=3, got %d", putPhase.Completed)
	}
	if putPhase.Skipped != 6 {
		t.Fatalf("expected Skipped=6, got %d", putPhase.Skipped)
	}
	if putPhase.Failed != 1 {
		t.Fatalf("expected Failed=1, got %d", putPhase.Failed)
	}
	if putPhase.Planned != putPhase.Dispatched+putPhase.Skipped {
		t.Fatalf("broken invariant: planned (%d) != dispatched (%d) + skipped (%d)",
			putPhase.Planned, putPhase.Dispatched, putPhase.Skipped)
	}
	if putPhase.Completed != putPhase.Dispatched {
		t.Fatalf("broken invariant: completed (%d) != dispatched (%d)",
			putPhase.Completed, putPhase.Dispatched)
	}

	// In-flight operations 1 & 2 completed successfully and their payload bytes were counted
	if putPhase.SuccessfulBytes <= 0 {
		t.Fatalf("expected SuccessfulBytes > 0 from in-flight drain, got %d", putPhase.SuccessfulBytes)
	}
}

func TestFailFastSIGINTAfterFailureArmsDrainTimeout(t *testing.T) {
	c, o, _ := setup(t)
	o.Concurrency = 2
	o.FailFast = true
	o.DrainTimeout = 50 * time.Millisecond

	var (
		job0Done        = make(chan struct{})
		job1Started     = make(chan struct{})
		releaseJob1     = make(chan struct{})
		releaseJob1Once sync.Once
	)
	safeRelease := func() {
		releaseJob1Once.Do(func() { close(releaseJob1) })
	}
	defer safeRelease()

	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Query().Get("list-type") == "2" {
			w.Header().Set("Content-Type", "application/xml")
			io.WriteString(w, "<ListBucketResult><IsTruncated>false</IsTruncated></ListBucketResult>")
			return
		}
		if r.Method == "PUT" {
			key := r.URL.Path

			if strings.HasSuffix(key, "low-entropy-v00.bin") {
				// Job 0: fail with 503 to latch stop first
				// Keep both requests in flight before the failure can stop dispatch.
				select {
				case <-job1Started:
				case <-r.Context().Done():
					return
				}
				defer func() {
					select {
					case <-job0Done:
					default:
						close(job0Done)
					}
				}()
				w.Header().Set("Content-Type", "application/xml")
				w.Header().Set("X-Amz-Request-Id", "SLOWDOWN12345678")
				w.WriteHeader(503)
				io.WriteString(w, "<Error><Code>SlowDown</Code><Message>rate limited</Message></Error>")
				return
			}

			if strings.HasSuffix(key, "compressed-identical-v00.gz") {
				// Job 1: signal started, then hold until released or client aborts
				select {
				case <-job1Started:
				default:
					close(job1Started)
				}
				select {
				case <-releaseJob1:
					w.WriteHeader(200)
				case <-r.Context().Done():
					// client canceled due to emergency drain timeout
				}
				return
			}

			w.WriteHeader(500)
			return
		}
		w.WriteHeader(200)
	}))
	defer s.Close()
	c.Endpoint = s.URL

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type runRes struct {
		r   Result
		err error
	}
	done := make(chan runRes, 1)
	go func() {
		res, err := Run(ctx, c, o)
		done <- runRes{res, err}
	}()

	// Wait for job 0 to finish with 503 (latching stopReason="SlowDown", stopped=true)
	select {
	case <-job0Done:
	case <-time.After(5 * time.Second):
		t.Fatal("job 0 never completed")
	}

	// Wait for job 1 to be active in-flight on the server
	select {
	case <-job1Started:
	case <-time.After(5 * time.Second):
		t.Fatal("job 1 never started")
	}

	// At this point:
	// - Job 0 failed with 503, so coord.stopped is ALREADY true and stopReason is "SlowDown".
	// - Job 1 is currently in-flight, blocked on the server.
	// Give worker 0 brief slack to process the 503 response and latch triggerStop("SlowDown").
	time.Sleep(25 * time.Millisecond)

	// Now simulate late operator SIGINT / cancellation while job 1 is in-flight:
	cancel()

	// Measure that Run returns within bounded emergency drain timeout (50ms + slack),
	// NOT waiting for 30-minute requestTimeout.
	var res runRes
	select {
	case res = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("emergency drain timeout was not armed when SIGINT arrived after stop was already latched")
	}
	safeRelease()

	// Invariant checks:
	// 1. Status is failed, error is interrupted
	if res.r.Status != "failed" {
		t.Fatalf("expected Status='failed', got %q", res.r.Status)
	}
	if !errors.Is(res.err, ErrInterrupted) {
		t.Fatalf("expected ErrInterrupted, got %v", res.err)
	}
	if res.r.Error != "interrupted" {
		t.Fatalf("expected Error='interrupted', got %q", res.r.Error)
	}

	// 2. Initial stop trigger is preserved
	if res.r.StopTrigger != "SlowDown" {
		t.Fatalf("expected initial StopTrigger='SlowDown' preserved, got %q", res.r.StopTrigger)
	}

	// 3. Unresolved in-flight PUT was aborted by emergency drain timeout, so it is indeterminate
	// and demands durable reconciliation
	if !res.r.NeedsReconciliation {
		t.Fatal("expected NeedsReconciliation=true for aborted in-flight PUT")
	}
	if res.r.Cleanup != "objects_may_remain_indeterminate_outcome" {
		t.Fatalf("expected Cleanup='objects_may_remain_indeterminate_outcome', got %q", res.r.Cleanup)
	}

	// Verify sample 1 is marked indeterminate
	foundIndeterminate := false
	for _, s := range res.r.Samples {
		if strings.HasSuffix(s.File, "compressed-identical-v00.gz") {
			if !s.Indeterminate {
				t.Fatalf("expected sample to be marked indeterminate: %+v", s)
			}
			if s.Reconciliation != "needs_reconciliation" {
				t.Fatalf("expected Reconciliation='needs_reconciliation', got %q", s.Reconciliation)
			}
			foundIndeterminate = true
		}
	}
	if !foundIndeterminate {
		t.Fatal("expected indeterminate sample for held in-flight PUT")
	}
}

func TestFailFastStopVsDispatchStress(t *testing.T) {
	for iter := 0; iter < 50; iter++ {
		c, o, _ := setup(t)
		o.Concurrency = 4
		o.FailFast = true

		var (
			mu        sync.Mutex
			dispathed int
			failedIdx = (iter % 3) + 1 // varies between call 1, 2, 3
		)

		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == "GET" && r.URL.Query().Get("list-type") == "2" {
				w.Header().Set("Content-Type", "application/xml")
				io.WriteString(w, "<ListBucketResult><IsTruncated>false</IsTruncated></ListBucketResult>")
				return
			}
			if r.Method == "PUT" {
				mu.Lock()
				dispathed++
				idx := dispathed
				mu.Unlock()

				if idx == failedIdx {
					w.WriteHeader(503)
					io.WriteString(w, "<Error><Code>SlowDown</Code></Error>")
					return
				}
				time.Sleep(2 * time.Millisecond)
				w.WriteHeader(200)
				return
			}
			w.WriteHeader(200)
		}))

		c.Endpoint = s.URL
		r, err := Run(context.Background(), c, o)
		s.Close()

		if err == nil || r.Status != "failed" {
			t.Fatalf("iteration %d: expected failed status", iter)
		}
		putPhase := r.Phases[1]
		if putPhase.Planned != 9 {
			t.Fatalf("iteration %d: planned=%d", iter, putPhase.Planned)
		}
		if putPhase.Planned != putPhase.Dispatched+putPhase.Skipped {
			t.Fatalf("iteration %d: planned (%d) != dispatched (%d) + skipped (%d)",
				iter, putPhase.Planned, putPhase.Dispatched, putPhase.Skipped)
		}
		if putPhase.Completed != putPhase.Dispatched {
			t.Fatalf("iteration %d: completed (%d) != dispatched (%d)",
				iter, putPhase.Completed, putPhase.Dispatched)
		}
	}
}

func TestMultipartScenario(t *testing.T) {
	dir := t.TempDir()
	dataset := filepath.Join(dir, "boundary-dataset")
	if _, err := benchmarkdata.Generate(dataset, "boundary-8mib", "mp-seed"); err != nil {
		t.Fatal(err)
	}
	cred := filepath.Join(dir, "credentials.json")
	if err := os.WriteFile(cred, []byte(`{"access_key_id":"private-access","secret_access_key":"private-secret","session_token":"private-session"}`), 0600); err != nil {
		t.Fatal(err)
	}
	f := &s3Fixture{objects: map[string][]byte{}, multiparts: make(map[string]map[int32][]byte)}
	s := httptest.NewServer(f)
	defer s.Close()

	c := Config{
		Target:          "local-s3-smoke",
		Endpoint:        s.URL,
		Bucket:          "test-bucket",
		Region:          "us-east-1",
		CredentialsFile: cred,
		Environment: Environment{
			Topology: "loopback protocol fixture", Host: "test", CPU: "test", Disk: "test",
			Network: "loopback", Metadata: "none", Encryption: "none", CacheState: "uncontrolled",
			Isolation: "fixture", AsyncWork: "none",
		},
	}

	o := Options{
		Manifest:    filepath.Join(dataset, "manifest.json"),
		Out:         filepath.Join(dir, "artifact-mp"),
		Concurrency: 1,
		ReadRepeats: 1,
		Scenario:    "multipart",
	}

	r, err := Run(context.Background(), c, o)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if r.Status != "passed" {
		t.Fatalf("expected passed, got %s", r.Status)
	}
	if len(r.Dataset.Files) != 3 {
		t.Fatalf("expected 3 files, got %d", len(r.Dataset.Files))
	}

	// Verify dispatch types in PUT phase samples
	var singlePutCount, mpCount int
	for _, s := range r.Samples {
		if s.Phase == "put" {
			if s.DispatchedType == "single-put" {
				singlePutCount++
			} else if s.DispatchedType == "multipart" {
				mpCount++
				if s.PartCount != 2 {
					t.Fatalf("expected 2 parts for 8MiB+1, got %d", s.PartCount)
				}
			}
		}
	}
	if singlePutCount != 2 {
		t.Fatalf("expected 2 single-put samples (minus-one and equal), got %d", singlePutCount)
	}
	if mpCount != 1 {
		t.Fatalf("expected 1 multipart sample (plus-one), got %d", mpCount)
	}

	// Test variant filtering: threshold-minus-one only
	oMinusOne := Options{
		Manifest:    filepath.Join(dataset, "manifest.json"),
		Out:         filepath.Join(dir, "artifact-minus-one"),
		Concurrency: 1,
		Scenario:    "multipart",
		Variant:     "threshold-minus-one",
	}
	rMinus, err := Run(context.Background(), c, oMinusOne)
	if err != nil {
		t.Fatalf("Run minus-one failed: %v", err)
	}
	if len(rMinus.Dataset.Files) != 1 || rMinus.Dataset.Files[0].Bytes != 8*1024*1024-1 {
		t.Fatalf("unexpected files in minus-one run: %+v", rMinus.Dataset.Files)
	}

	// Test variant filtering: threshold-plus-one only
	oPlusOne := Options{
		Manifest:    filepath.Join(dataset, "manifest.json"),
		Out:         filepath.Join(dir, "artifact-plus-one"),
		Concurrency: 1,
		Scenario:    "multipart",
		Variant:     "threshold-plus-one",
	}
	rPlus, err := Run(context.Background(), c, oPlusOne)
	if err != nil {
		t.Fatalf("Run plus-one failed: %v", err)
	}
	if len(rPlus.Dataset.Files) != 1 || rPlus.Dataset.Files[0].Bytes != 8*1024*1024+1 {
		t.Fatalf("unexpected files in plus-one run: %+v", rPlus.Dataset.Files)
	}
}

func TestOverwriteScenario(t *testing.T) {
	c, o, f := setup(t)
	o.Scenario = "overwrite"
	o.Variant = "overwrite-versions"
	o.Out = filepath.Join(t.TempDir(), "artifact-overwrite")

	r, err := Run(context.Background(), c, o)
	if err != nil {
		t.Fatalf("Run overwrite failed: %v", err)
	}
	if r.Status != "passed" {
		t.Fatalf("expected passed, got %s", r.Status)
	}
	if r.HistoricalBytes != r.Dataset.TotalBytes {
		t.Fatalf("historical bytes %d != total %d", r.HistoricalBytes, r.Dataset.TotalBytes)
	}
	if r.LiveLogicalBytes <= 0 || r.LiveLogicalBytes >= r.HistoricalBytes {
		t.Fatalf("live bytes (%d) must be positive and < historical bytes (%d)", r.LiveLogicalBytes, r.HistoricalBytes)
	}

	// Confirm that phases include overwrite-put, overwrite-head, overwrite-get
	phaseNames := make(map[string]bool)
	for _, p := range r.Phases {
		phaseNames[p.Name] = true
	}
	for _, expected := range []string{"overwrite-put", "overwrite-head", "overwrite-get", "list", "delete"} {
		if !phaseNames[expected] {
			t.Fatalf("missing expected phase %s in %+v", expected, r.Phases)
		}
	}
	if len(f.objects) != 1 {
		t.Fatalf("expected 1 object remaining (outside-run), got %d", len(f.objects))
	}
}

func TestDomainScenario(t *testing.T) {
	c, o, _ := setup(t)
	o.Scenario = "domain"
	o.Variant = "shared-default"
	o.Domain = "shared-default"
	o.Out = filepath.Join(t.TempDir(), "artifact-domain-no-second")

	// 1. Without --second-config, domain scenario must fail closed
	_, err := Run(context.Background(), c, o)
	if err == nil || !strings.Contains(err.Error(), "requires --second-config") {
		t.Fatalf("expected error about --second-config, got %v", err)
	}

	// 2. Setup Tenant B credentials and config
	temp := t.TempDir()
	credsBPath := filepath.Join(temp, "creds-b.json")
	if err := os.WriteFile(credsBPath, []byte(`{"access_key_id":"tenant-b-key","secret_access_key":"tenant-b-secret","session_token":"tenant-b-session"}`), 0600); err != nil {
		t.Fatal(err)
	}
	c2 := c
	c2.Bucket = "test-bucket-b"
	c2.CredentialsFile = credsBPath
	c2Bytes, _ := json.Marshal(c2)
	c2Path := filepath.Join(temp, "tenant2-config.json")
	if err := os.WriteFile(c2Path, c2Bytes, 0600); err != nil {
		t.Fatal(err)
	}

	// 3. Run shared-default with second-config
	oShared := o
	oShared.SecondConfig = c2Path
	oShared.Out = filepath.Join(t.TempDir(), "artifact-domain-shared")
	rShared, err := Run(context.Background(), c, oShared)
	if err != nil {
		t.Fatalf("Run domain shared failed: %v", err)
	}
	if rShared.Status != "passed" || !rShared.DomainCrossOwnerRejected || !rShared.SecondTenantVerified || rShared.Cleanup != "domain_two_tenant_isolation_verified" {
		t.Fatalf("domain shared run mismatch: %+v", rShared)
	}

	// 4. Run separated with second-config
	oSep := oShared
	oSep.Variant = "separated"
	oSep.Domain = "separated"
	oSep.Out = filepath.Join(t.TempDir(), "artifact-domain-sep")
	rSep, err := Run(context.Background(), c, oSep)
	if err != nil {
		t.Fatalf("Run domain separated failed: %v", err)
	}
	if rSep.Status != "passed" || !rSep.DomainCrossOwnerRejected || !rSep.SecondTenantVerified || rSep.Cleanup != "domain_two_tenant_isolation_verified" {
		t.Fatalf("domain separated run mismatch: %+v", rSep)
	}
}

func TestEdgeCloneScenario(t *testing.T) {
	c, o, f := setup(t)
	o.Scenario = "edge-clone"
	o.Variant = "clone-upload"
	o.RetainObjects = true
	o.Out = filepath.Join(t.TempDir(), "artifact-edge-upload")

	r, err := Run(context.Background(), c, o)
	if err != nil {
		t.Fatalf("Run edge clone-upload failed: %v", err)
	}
	if r.Status != "passed" || r.Cleanup != "retained_owned_prefix_for_observation" {
		t.Fatalf("edge clone upload mismatch: %+v", r)
	}

	// 1. clone-download without --source-prefix must fail closed
	oNoSource := o
	oNoSource.Variant = "clone-download"
	oNoSource.SourcePrefix = ""
	oNoSource.Out = filepath.Join(t.TempDir(), "artifact-edge-no-source")
	_, err = Run(context.Background(), c, oNoSource)
	if err == nil || !strings.Contains(err.Error(), "requires non-empty --source-prefix") {
		t.Fatalf("expected error about --source-prefix, got %v", err)
	}

	// 2. clone-download with valid source prefix
	oDownload := o
	oDownload.Variant = "clone-download"
	oDownload.RetainObjects = false
	oDownload.SourcePrefix = r.Prefix
	oDownload.Out = filepath.Join(t.TempDir(), "artifact-edge-download")
	rDown, err := Run(context.Background(), c, oDownload)
	if err != nil {
		t.Fatalf("Run edge clone-download failed: %v", err)
	}
	if rDown.Status != "passed" || rDown.Cleanup != "edge_clone_download_verified" {
		t.Fatalf("edge clone download mismatch: %+v", rDown)
	}

	// Clean up retained upload prefix
	f.mu.Lock()
	for k := range f.objects {
		if strings.HasPrefix(k, r.Prefix) {
			delete(f.objects, k)
		}
	}
	f.mu.Unlock()
}

func TestWarmSinglePutNeverDispatchesMultipart(t *testing.T) {
	dir := t.TempDir()
	dataset := filepath.Join(dir, "boundary-dataset")
	if _, err := benchmarkdata.Generate(dataset, "boundary-8mib", "test-seed"); err != nil {
		t.Fatal(err)
	}

	f := &s3Fixture{objects: make(map[string][]byte)}
	srv := httptest.NewServer(f)
	defer srv.Close()

	credsPath := filepath.Join(dir, "creds.json")
	if err := os.WriteFile(credsPath, []byte(`{"access_key_id":"test-key","secret_access_key":"test-secret","session_token":"private-session"}`), 0600); err != nil {
		t.Fatal(err)
	}

	c := Config{
		Target: "local-s3-smoke", Endpoint: srv.URL, Region: "us-east-1", Bucket: "test-bucket", CredentialsFile: credsPath,
		Environment: Environment{Topology: "cloud", Host: "test", CPU: "test", Disk: "test", Network: "test", Metadata: "badger", Encryption: "test", CacheState: "test", Isolation: "test", AsyncWork: "test"},
	}

	// Run with Scenario == "warm" on boundary dataset (which contains > 8 MiB file)
	o := Options{
		Manifest:    filepath.Join(dataset, "manifest.json"),
		Out:         filepath.Join(dir, "artifact-warm-single-put"),
		Concurrency: 1,
		Scenario:    "warm",
	}

	r, err := Run(context.Background(), c, o)
	if err != nil {
		t.Fatalf("Run warm failed: %v", err)
	}
	if r.Status != "passed" {
		t.Fatalf("expected passed, got %s", r.Status)
	}

	// Verify that EVERY sample in put phase is single-put, NEVER multipart (BEN-03 compliance)
	for _, s := range r.Samples {
		if s.Phase == "put" {
			if s.DispatchedType != "single-put" || s.PartCount != 0 {
				t.Fatalf("BEN-03 violation: Warm C1 dispatched type %q with part count %d (must be single-put only)", s.DispatchedType, s.PartCount)
			}
		}
	}
}

func TestColdStorageClass(t *testing.T) {
	c, o, _ := setup(t)
	c.StorageClass = "GLACIER_IR"
	o.Scenario = "cold"
	o.Variant = "ir"
	o.Out = filepath.Join(t.TempDir(), "artifact-cold-ir")

	r, err := Run(context.Background(), c, o)
	if err != nil {
		t.Fatalf("Run cold IR failed: %v", err)
	}
	if r.Status != "passed" {
		t.Fatalf("cold IR run failed: %+v", r)
	}

	cFlex := c
	cFlex.StorageClass = "GLACIER"
	oFlex := o
	oFlex.Variant = "flexible"
	oFlex.Out = filepath.Join(t.TempDir(), "artifact-cold-flex")
	rFlex, err := Run(context.Background(), cFlex, oFlex)
	if err != nil {
		t.Fatalf("Run cold flexible failed: %v", err)
	}
	if rFlex.Status != "passed" {
		t.Fatalf("cold flexible run failed: %+v", rFlex)
	}
}

func TestRecoveryScenario(t *testing.T) {
	c, o, _ := setup(t)
	o.Scenario = "recovery"
	o.Variant = "r1"
	o.Out = filepath.Join(t.TempDir(), "artifact-recovery-r1")

	r, err := Run(context.Background(), c, o)
	if err != nil {
		t.Fatalf("Run recovery r1 failed: %v", err)
	}
	if r.Status != "passed" || r.Scenario != "recovery" || r.Variant != "r1" {
		t.Fatalf("unexpected recovery r1 result: %+v", r)
	}

	o2 := o
	o2.Variant = "r2"
	o2.Out = filepath.Join(t.TempDir(), "artifact-recovery-r2")
	r2, err := Run(context.Background(), c, o2)
	if err != nil {
		t.Fatalf("Run recovery r2 failed: %v", err)
	}
	if r2.Status != "passed" || r2.Scenario != "recovery" || r2.Variant != "r2" {
		t.Fatalf("unexpected recovery r2 result: %+v", r2)
	}
}
