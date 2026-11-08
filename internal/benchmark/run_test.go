package benchmark

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/benchmarkdata"
)

// A protocol fixture, never a Miles performance comparator. Real SDK calls run
// over HTTP so pagination, signing, body streaming and failures are exercised.
type s3Fixture struct {
	mu          sync.Mutex
	objects     map[string][]byte
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
	if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") || r.Header.Get("X-Amz-Security-Token") != "private-session" {
		f.unsigned = true
	}
	f.mu.Unlock()
	defer func() { f.mu.Lock(); f.active--; f.mu.Unlock() }()
	key := strings.TrimPrefix(r.URL.Path, "/test-bucket/")
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
	f := &s3Fixture{objects: map[string][]byte{"outside-run/keep": []byte("unrelated")}}
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

			// Call 2 fails with 503 clock_uncertainty_exceeded immediately.
			// Calls 1, 3, 4 sleep briefly to simulate in-flight work and then succeed.
			if callIndex == 2 {
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
