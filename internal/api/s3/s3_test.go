package s3

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
)

const fixtureDir = "../../../tests/fixtures/s3/"

func readFixture(t *testing.T, name string, v any) {
	t.Helper()
	data, err := os.ReadFile(fixtureDir + name)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
}

// routes.json: each request (raw HTTP lines) resolves to op, or fails with
// error; bucket and key are those of the Route or of the Error.
func TestRouteFixtures(t *testing.T) {
	var fixtures []struct {
		Name    string
		Request []string
		Op      Operation
		Error   ErrorCode
		Bucket  string
		Key     string
	}
	readFixture(t, "routes.json", &fixtures)
	type result struct {
		op          Operation
		code        ErrorCode
		bucket, key string
	}
	for _, f := range fixtures {
		t.Run(f.Name, func(t *testing.T) {
			raw := strings.Join(f.Request, "\r\n") + "\r\n\r\n"
			req, err := http.ReadRequest(bufio.NewReader(strings.NewReader(raw)))
			if err != nil {
				t.Fatal(err)
			}
			route, e := Resolve(req)
			got := result{op: route.Op, bucket: route.Bucket, key: route.Key}
			if e != nil {
				got = result{code: e.Code, bucket: e.Bucket, key: e.Key}
			}
			if want := (result{f.Op, f.Error, f.Bucket, f.Key}); got != want {
				t.Errorf("got %+v, want %+v (error %v)", got, want, e)
			}
		})
	}
}

// errors.json: codes pins the HTTP status of every API v2 2.4 code;
// responses pins the full error response written for a request method.
func TestErrorFixtures(t *testing.T) {
	var f struct {
		RequestID string `json:"request_id"`
		Codes     []struct {
			Code   ErrorCode
			Status int
		}
		Responses []struct {
			Name    string
			Method  string
			Error   Error
			Status  int
			Headers map[string]string
			Body    string
		}
	}
	readFixture(t, "errors.json", &f)
	for _, c := range f.Codes {
		if got := (&Error{Code: c.Code}).Status(); got != c.Status {
			t.Errorf("%s: status %d, want %d", c.Code, got, c.Status)
		}
	}
	for _, r := range f.Responses {
		t.Run(r.Name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			if err := WriteError(rec, httptest.NewRequest(r.Method, "/", nil), &r.Error, f.RequestID); err != nil {
				t.Fatal(err)
			}
			if rec.Code != r.Status {
				t.Errorf("status %d, want %d", rec.Code, r.Status)
			}
			for k, v := range r.Headers {
				if got := rec.Header().Get(k); got != v {
					t.Errorf("header %s = %q, want %q", k, got, v)
				}
			}
			if got := rec.Body.String(); got != r.Body {
				t.Errorf("body\n got %q\nwant %q", got, r.Body)
			}
			if r.Body != "" && rec.Header().Get("Content-Length") != strconv.Itoa(len(r.Body)) {
				t.Errorf("Content-Length %s, want %d", rec.Header().Get("Content-Length"), len(r.Body))
			}
		})
	}
}

type closedClient struct{ *httptest.ResponseRecorder }

func (closedClient) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

// A body write that fails after the status is sent must reach the caller.
func TestWriteErrorReturnsWriteFailure(t *testing.T) {
	err := WriteError(closedClient{httptest.NewRecorder()}, httptest.NewRequest(http.MethodGet, "/", nil), &Error{Code: ErrNoSuchKey}, "R")
	if !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("err = %v, want %v", err, io.ErrClosedPipe)
	}
}
