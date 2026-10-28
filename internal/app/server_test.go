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
			go func() { done <- serve(ctx, ln, c, d, new(Admission), io.Discard) }()
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

func TestShutdownWaitsForAdmittedWorkAndReportsTimeout(t *testing.T) {
	for _, complete := range []bool{true, false} {
		t.Run(map[bool]string{true: "complete", false: "timeout"}[complete], func(t *testing.T) {
			c := config.DefaultStartup()
			c.Mode = "dev"
			c.ShutdownSeconds = 1
			d, err := Bootstrap(c)
			if err != nil {
				t.Fatal(err)
			}
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			closed := false
			d.Close = func() error { closed = true; return nil }
			a := new(Admission)
			release, ok := a.Acquire()
			if !ok {
				t.Fatal("initial admission")
			}
			defer release()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- serve(ctx, ln, c, d, a, io.Discard) }()
			cancel()
			deadline := time.Now().Add(time.Second)
			for !a.Draining() {
				if time.Now().After(deadline) {
					t.Fatal("did not drain")
				}
				time.Sleep(time.Millisecond)
			}
			if _, ok := a.Acquire(); ok {
				t.Fatal("new work accepted")
			}
			response, err := http.Get("http://" + ln.Addr().String() + "/readyz")
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != 503 {
				t.Fatal("draining readiness", response.StatusCode)
			}
			if complete {
				release()
			}
			select {
			case err := <-done:
				if complete && err != nil {
					t.Fatal(err)
				}
				if closed != complete {
					t.Fatalf("backend closed=%v, work quiescent=%v", closed, complete)
				}
				if !complete && err == nil {
					t.Fatal("timed out work reported shutdown complete")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("shutdown was not bounded")
			}
		})
	}
}
