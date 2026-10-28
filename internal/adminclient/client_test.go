package adminclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/api/admin"
)

func tokenFile(t *testing.T) (string, string) {
	t.Helper()
	_, _, token, err := admin.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(token+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return path, token
}

func TestBearerRoutesUnsupportedAndNoSecretReflection(t *testing.T) {
	path, token := tokenFile(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token || r.Method != "GET" {
			t.Error("wrong authentication transport")
		}
		switch r.URL.Path {
		case "/admin/v1/stats":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"echo":"` + token + `"}`))
		case "/admin/v1/jobs":
			w.WriteHeader(501)
			w.Write([]byte(token))
		default:
			w.WriteHeader(401)
			w.Write([]byte(token))
		}
	}))
	defer server.Close()
	client, err := New(server.URL, path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	body, err := client.Get(context.Background(), "/admin/v1/stats")
	if err != nil || strings.Contains(string(body), token) || !strings.Contains(string(body), "[REDACTED]") {
		t.Fatal("successful response leaked token or request failed")
	}
	if _, err := client.Get(context.Background(), "/admin/v1/jobs"); !errors.Is(err, ErrUnsupported) {
		t.Fatal(err)
	}
	if _, err := client.Get(context.Background(), "/admin/v1/users"); err == nil || strings.Contains(err.Error(), token) {
		t.Fatal("auth failure leaked token")
	}
	if _, err := client.Get(context.Background(), "/bucket/key"); err == nil {
		t.Fatal("sent bearer to S3 route")
	}
	if _, err := client.Get(context.Background(), "//example.com/admin/v1/stats"); err == nil {
		t.Fatal("allowed cross-origin route")
	}
}

func TestRedirectNeverForwardsBearer(t *testing.T) {
	path, _ := tokenFile(t)
	hits := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++; w.Write([]byte(`{}`)) }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL+"/admin/v1/stats", 302) }))
	defer server.Close()
	client, err := New(server.URL, path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.Get(context.Background(), "/admin/v1/stats"); err == nil || hits != 0 {
		t.Fatal("followed bearer redirect")
	}
}

func TestEndpointPermissionsTimeoutAndBoundedResponse(t *testing.T) {
	path, _ := tokenFile(t)
	for _, endpoint := range []string{"http://10.0.0.1:9000", "http://example.com", "http://user:secret@127.0.0.1:9000", "http://127.0.0.1:9000/?token=x", "http://127.0.0.1:9000/?", "http://127.0.0.1:9000/#", "http://127.0.0.1:9000/admin/v1", "ftp://127.0.0.1"} {
		if _, err := New(endpoint, path, time.Second); err == nil {
			t.Fatal("accepted unsafe Admin endpoint")
		}
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := New("http://127.0.0.1:9000", path, time.Second); err == nil {
		t.Fatal("accepted public token file")
	}
	os.Chmod(path, 0600)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/admin/v1/jobs" {
			<-r.Context().Done()
			return
		}
		if r.URL.Path == "/admin/v1/stats" {
			w.Write([]byte(`{"large":"` + strings.Repeat("a", maxResponseBytes) + `"}`))
			return
		}
		w.Write([]byte("not JSON"))
	}))
	defer server.Close()
	client, err := New(server.URL, path, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	for _, route := range []string{"/admin/v1/jobs", "/admin/v1/stats", "/admin/v1/users"} {
		if _, err := client.Get(context.Background(), route); err == nil {
			t.Fatal("accepted stalled/oversized/invalid response", route)
		}
	}
}
