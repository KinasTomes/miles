package app

import (
	"context"
	"github.com/KinasTomes/miles/internal/config"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestHealthServerBootstrap(t *testing.T) {
	for _, mode := range []string{"dev", "runtime"} {
		t.Run(mode, func(t *testing.T) {
			c := config.DefaultStartup()
			c.Mode = mode
			d, err := Bootstrap(c)
			if err != nil {
				t.Fatal(err)
			}
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- serve(ctx, ln, c, d, io.Discard) }()
			client := &http.Client{Timeout: time.Second}
			defer client.CloseIdleConnections()
			deadline := time.Now().Add(3 * time.Second)
			for {
				r, err := client.Get("http://" + ln.Addr().String() + "/readyz")
				if err != nil {
					t.Fatal(err)
				}
				_, _ = io.Copy(io.Discard, r.Body)
				r.Body.Close()
				expected := 200
				if mode == "runtime" {
					expected = 503
				}
				if r.StatusCode == expected {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("readiness=%d want %d", r.StatusCode, expected)
				}
			}
			r, err := client.Get("http://" + ln.Addr().String() + "/healthz")
			if err != nil {
				t.Fatal(err)
			}
			r.Body.Close()
			if r.StatusCode != 200 {
				t.Fatal(r.StatusCode)
			}
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("server did not stop")
			}
		})
	}
}
