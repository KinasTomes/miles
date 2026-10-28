package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/storage"
)

func TestLocalPackTransfersUseCallerDeadline(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodHead:
			w.Header().Set("Content-Length", "4")
			return
		case http.MethodPut:
			_, _ = io.Copy(io.Discard, r.Body)
		case http.MethodGet:
			w.Header().Set("Content-Length", "4")
			w.Header().Set("Content-Range", "bytes 0-3/4")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write([]byte("sl"))
			w.(http.Flusher).Flush()
		default:
			t.Errorf("unexpected backend method %s", r.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		timer := time.NewTimer(1200 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-r.Context().Done():
			return
		case <-timer.C:
		}
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte("ow"))
		}
	}))
	defer backend.Close()
	c := config.DefaultStartup()
	c.Mode, c.LocalDataPath, c.PackEndpoint, c.PackBucket = "local", filepath.Join(t.TempDir(), "local"), backend.URL, "packs"
	d, err := Bootstrap(c)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ref := storage.PackRef{ID: "ab", Generation: "1"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.Packs.PutPack(ctx, storage.PackInfo{Version: storage.ModelVersion, Ref: ref, Size: 4, Class: storage.Standard, State: "Warm"}, bytes.NewBufferString("slow")); err != nil {
		t.Fatal("upload must survive a backend response slower than one second:", err)
	}
	body, err := d.Packs.RangeGet(ctx, ref, storage.ByteRange{Offset: 0, Length: 4})
	if err != nil {
		t.Fatal(err)
	}
	got, readErr := io.ReadAll(body)
	body.Close()
	if readErr != nil || string(got) != "slow" {
		t.Fatalf("slow response body lost: %q %v", got, readErr)
	}

	expiring, stop := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer stop()
	body, err = d.Packs.RangeGet(expiring, ref, storage.ByteRange{Offset: 0, Length: 4})
	if err == nil {
		_, err = io.ReadAll(body)
		body.Close()
	}
	if err == nil || !errors.Is(expiring.Err(), context.DeadlineExceeded) {
		t.Fatal("caller deadline did not interrupt transfer")
	}
}

func TestLocalProbeKeepsOwnDeadline(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer backend.Close()
	c := config.DefaultStartup()
	c.Mode, c.LocalDataPath, c.PackEndpoint, c.PackBucket = "local", filepath.Join(t.TempDir(), "local"), backend.URL, "packs"
	d, err := Bootstrap(c)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	parent, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := d.Probe(parent); err == nil || parent.Err() != nil {
		t.Fatal("probe must expire independently while its caller still has time")
	}
}
