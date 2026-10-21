package app

import (
	"context"
	"errors"
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
	ln, err := net.Listen("tcp", c.HealthAddress)
	if err != nil {
		return err
	}
	return serve(ctx, ln, c, d, output)
}
func serve(ctx context.Context, ln net.Listener, c config.Startup, d Dependencies, output io.Writer) error {
	defer ln.Close()
	logger := slog.New(slog.NewJSONHandler(output, nil))
	h := observability.NewHealth(d.Probe, nil)
	probeCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go h.Run(probeCtx)
	server := &http.Server{Handler: h.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 150 * time.Second, MaxHeaderBytes: 16 << 10}
	done := make(chan error, 1)
	go func() { done <- server.Serve(ln) }()
	logger.Info("health server listening", "address", ln.Addr().String(), "profile", c.Profile, "mode", c.Mode)
	if c.Mode == "dev" {
		logger.Warn("dev fixtures and simulated clock readiness; no durable storage")
	}
	select {
	case err := <-done:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
		_ = server.Close()
		err := <-done
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}
