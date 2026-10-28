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
		return closeDependencies(d, err)
	}
	if ctx.Err() != nil {
		return closeDependencies(d, nil)
	}
	ln, err := net.Listen("tcp", c.HealthAddress)
	if err != nil {
		return closeDependencies(d, err)
	}
	return serve(ctx, ln, c, d, new(Admission), output)
}

func closeDependencies(d Dependencies, err error) error {
	if d.Close != nil {
		return errors.Join(err, d.Close())
	}
	return err
}

func serve(ctx context.Context, ln net.Listener, c config.Startup, d Dependencies, admission *Admission, output io.Writer) error {
	defer ln.Close()
	logger := slog.New(slog.NewJSONHandler(output, nil))
	h := observability.NewHealth(d.Probe, admission.Draining, c.Mode != "runtime")
	health := &http.Server{Handler: h.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 150 * time.Second, MaxHeaderBytes: 16 << 10}
	servers := []*http.Server{health}
	listeners := []net.Listener{ln}
	if c.S3Address != "" {
		if d.S3Handler == nil || c.Mode == "runtime" {
			return closeDependencies(d, ErrRuntimeUnavailable)
		}
		s3ln, err := net.Listen("tcp", c.S3Address)
		if err != nil {
			return closeDependencies(d, err)
		}
		defer s3ln.Close()
		servers = append(servers, &http.Server{Handler: d.S3Handler(admission), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 150 * time.Second, MaxHeaderBytes: 16 << 10})
		listeners = append(listeners, s3ln)
	}
	probeCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	probeDone := make(chan struct{})
	go func() { defer close(probeDone); h.Run(probeCtx) }()
	done := make(chan error, len(servers))
	for i, s := range servers {
		listener := listeners[i]
		go func() { done <- s.Serve(listener) }()
		name := "health"
		if i > 0 {
			name = "s3"
		}
		logger.Info(name+" server listening", "address", listener.Addr().String(), "profile", c.Profile, "mode", c.Mode)
	}
	if c.Mode != "runtime" {
		logger.Warn("development installation; simulated clock readiness, no distributed R1/R2", "mode", c.Mode)
	}
	var serveErr error
	exited := 0
	select {
	case serveErr = <-done:
		if errors.Is(serveErr, http.ErrServerClosed) {
			serveErr = nil
		}
		exited++
	case <-ctx.Done():
	}
	admission.Drain()
	cancel()
	shutdown, stop := context.WithTimeout(context.Background(), c.ShutdownTimeout())
	defer stop()
	workErr := admission.Wait(shutdown)
	var httpErr error
	for _, s := range servers {
		err := s.Shutdown(shutdown)
		if err != nil {
			_ = s.Close()
		}
		httpErr = errors.Join(httpErr, err)
	}
	for exited < len(servers) {
		err := <-done
		exited++
		if !errors.Is(err, http.ErrServerClosed) {
			serveErr = errors.Join(serveErr, err)
		}
	}
	var probeErr error
	select {
	case <-probeDone:
	case <-shutdown.Done():
		probeErr = shutdown.Err()
	}
	if err := errors.Join(workErr, httpErr, probeErr); err != nil {
		// Do not close storage underneath work that has not reached quiescence.
		logger.Error("incomplete shutdown", "error", err.Error())
		return errors.Join(serveErr, fmt.Errorf("incomplete shutdown: %w", err))
	}
	if err := closeDependencies(d, serveErr); err != nil {
		return err
	}
	logger.Info("shutdown complete")
	return nil
}
