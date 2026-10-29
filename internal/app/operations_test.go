package app

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/jobs"
	opstest "github.com/KinasTomes/miles/internal/jobs/contracttest"
	"github.com/KinasTomes/miles/internal/metadata/badger"
	"github.com/KinasTomes/miles/internal/observability"
)

func TestOperationalRuntimePublishesHeartbeatAndShutdownDrain(t *testing.T) {
	for _, mode := range []string{"dev", "runtime"} {
		t.Run(mode, func(t *testing.T) {
			store, e := badger.Open(badger.Options{Path: t.TempDir()})
			if e != nil {
				t.Fatal(e)
			}
			defer store.Close()
			registry := &jobs.Registry{Store: store, Clock: clock.NewReal(), Identity: opstest.Node("app-node")}
			cfg := config.DefaultStartup()
			cfg.Mode = mode
			cfg.ShutdownSeconds = 2
			d, e := Bootstrap(cfg)
			if e != nil {
				t.Fatal(e)
			}
			d.Operations = &Operational{Registry: registry, Metrics: new(observability.Operations)}
			if e = d.Operations.validate(); e != nil {
				t.Fatal(e)
			}
			listener, e := net.Listen("tcp", "127.0.0.1:0")
			if e != nil {
				t.Fatal(e)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- serve(ctx, listener, cfg, d, new(Admission), io.Discard) }()
			client := &http.Client{Timeout: time.Second}
			defer client.CloseIdleConnections()
			deadline := time.Now().Add(3 * time.Second)
			for {
				n, e := store.GetNode(context.Background(), "app-node")
				response, httpErr := client.Get("http://" + listener.Addr().String() + "/readyz")
				if httpErr != nil {
					t.Fatal(httpErr)
				}
				status := response.StatusCode
				response.Body.Close()
				want := 200
				if mode == "runtime" {
					want = 503
				}
				if e == nil && n.Revision > 0 && status == want {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("heartbeat/readiness did not converge", n, e, status)
				}
				time.Sleep(5 * time.Millisecond)
			}
			cancel()
			select {
			case e := <-done:
				if e != nil {
					t.Fatal(e)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("operational workers did not stop")
			}
			n, e := store.GetNode(context.Background(), "app-node")
			if e != nil || !n.Draining || n.Ready {
				t.Fatal("shutdown did not persist drain", n, e)
			}
		})
	}
}

func TestOperationalWorkersTreatParentTerminationAsNormal(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		if deadline {
			cancel()
			ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		} else {
			cancel()
		}
		o := &Operational{Registry: &jobs.Registry{Clock: clock.NewReal()}}
		if e := o.run(ctx, observability.NewHealth(nil, nil, false), new(Admission)); e != nil {
			t.Fatal(e)
		}
		cancel()
	}
}
