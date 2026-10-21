package app

import (
	"context"
	"errors"
	"fmt"
	"github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/observability"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"
)

func Run(ctx context.Context, c config.Startup, d Dependencies, output io.Writer) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return nil
	}
	ln, err := net.Listen("tcp", c.HealthAddress)
	if err != nil {
		return err
	}
	return serve(ctx, ln, c, d, new(Admission), output)
}
func serve(ctx context.Context, ln net.Listener, c config.Startup, d Dependencies, admission *Admission, output io.Writer) error {
	defer ln.Close()
	logger := slog.New(slog.NewJSONHandler(output, nil))
	h := observability.NewHealth(d.Probe, admission.Draining, c.Mode == "dev")
	probeCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	probeDone := make(chan struct{})
	go func() { defer close(probeDone); h.Run(probeCtx) }()
	server := &http.Server{Handler: h.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 150 * time.Second, MaxHeaderBytes: 16 << 10}
	done := make(chan error, 1)
	go func() { done <- server.Serve(ln) }()
	logger.Info("health server listening", "address", ln.Addr().String(), "profile", c.Profile, "mode", c.Mode)
	if c.Mode == "dev" {
		logger.Warn("dev fixtures and simulated clock readiness; no durable storage")
	}
	var serveErr error
	exited := false
	select {
	case serveErr = <-done:
		exited = true
	case <-ctx.Done():
	}
	// Close local admission before readiness or listener shutdown can lag behind.
	admission.Drain()
	cancel()
	shutdown, stop := context.WithTimeout(context.Background(), c.ShutdownTimeout())
	defer stop()
	workErr := admission.Wait(shutdown)
	httpErr := server.Shutdown(shutdown)
	if httpErr != nil {
		_ = server.Close()
	}
	if !exited {
		serveErr = <-done
	}
	if errors.Is(serveErr, http.ErrServerClosed) {
		serveErr = nil
	}
	var probeErr error
	select {
	case <-probeDone:
	case <-shutdown.Done():
		probeErr = shutdown.Err()
	}
	if err := errors.Join(workErr, httpErr, probeErr); err != nil {
		logger.Error("incomplete shutdown", "error", err.Error())
		return errors.Join(serveErr, fmt.Errorf("incomplete shutdown: %w", err))
	}
	logger.Info("shutdown complete")
	return serveErr
}
