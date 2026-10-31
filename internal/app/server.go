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
	if err := d.Operations.validate(); err != nil {
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
	var operationalReady func() bool
	var operationMetrics *observability.Operations
	if d.Operations != nil {
		operationalReady = d.Operations.Registry.Healthy
		operationMetrics = d.Operations.Metrics
	}
	h.AttachOperations(operationMetrics, func() int64 { n, _ := admission.Status(); return n }, operationalReady)
	health := &http.Server{Handler: h.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 150 * time.Second, MaxHeaderBytes: 16 << 10}
	servers := []*http.Server{health}
	listeners := []net.Listener{ln}
	names := []string{"health"}
	for _, l := range []struct {
		name, address string
		handler       func(*Admission) http.Handler
	}{{"s3", c.S3Address, d.S3Handler}, {"admin", c.AdminAddress, d.AdminHandler}} {
		if l.address == "" {
			continue
		}
		if l.handler == nil || c.Mode == "runtime" {
			return closeDependencies(d, ErrRuntimeUnavailable)
		}
		listener, err := net.Listen("tcp", l.address)
		if err != nil {
			return closeDependencies(d, err)
		}
		defer listener.Close()
		servers = append(servers, &http.Server{Handler: l.handler(admission), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 150 * time.Second, MaxHeaderBytes: 16 << 10})
		listeners = append(listeners, listener)
		names = append(names, l.name)
	}
	probeCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var statsDone chan struct{}
	if d.Statistics != nil {
		statsDone = make(chan struct{})
		go func() { defer close(statsDone); d.Statistics.Run(probeCtx) }()
	}
	probeDone := make(chan struct{})
	go func() { defer close(probeDone); h.Run(probeCtx) }()
	var operationsDone chan error
	if d.Operations != nil {
		operationsDone = make(chan error, 1)
		go func() { operationsDone <- d.Operations.run(probeCtx, h, admission) }()
	}
	done := make(chan error, len(servers))
	for i, s := range servers {
		listener := listeners[i]
		go func() { done <- s.Serve(listener) }()
		logger.Info(names[i]+" server listening", "address", listener.Addr().String(), "profile", c.Profile, "mode", c.Mode)
	}
	if c.Mode != "runtime" {
		logger.Warn("development installation; simulated clock readiness, no distributed R1/R2", "mode", c.Mode)
	}
	var serveErr error
	exited := 0
	operationsExited := false
	select {
	case err := <-operationsDone:
		operationsExited = true
		if ctx.Err() == nil {
			serveErr = errors.Join(errors.New("operational workers stopped"), err)
		} else {
			serveErr = err
		}
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
	var drainErr error
	if d.Operations != nil {
		call, done := context.WithTimeout(shutdown, 5*time.Second)
		_, drainErr = d.Operations.Registry.RequestDrain(call, d.Operations.Registry.Identity.ID, admission)
		done()
	}
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
	var operationsErr error
	if statsDone != nil {
		select {
		case <-statsDone:
		case <-shutdown.Done():
			probeErr = shutdown.Err()
		}
	}
	select {
	case <-probeDone:
	case <-shutdown.Done():
		probeErr = shutdown.Err()
	}
	if operationsDone != nil && !operationsExited {
		select {
		case err := <-operationsDone:
			operationsErr = err
		case <-shutdown.Done():
			probeErr = errors.Join(probeErr, shutdown.Err())
		}
	}
	if err := errors.Join(workErr, httpErr, probeErr); err != nil {
		// Do not close storage underneath work that has not reached quiescence.
		logger.Error("incomplete shutdown", "error", err.Error())
		return errors.Join(serveErr, drainErr, operationsErr, fmt.Errorf("incomplete shutdown: %w", err))
	}
	// A failed operational marker is not evidence of active storage users.
	// Once all workers quiesce, still flush/close storage and retain the error.
	closedErr := closeDependencies(d, errors.Join(serveErr, operationsErr))
	if drainErr != nil {
		err := errors.Join(closedErr, drainErr)
		logger.Error("incomplete shutdown", "error", err.Error())
		return fmt.Errorf("incomplete shutdown: %w", err)
	}
	if err := closedErr; err != nil {
		return err
	}
	logger.Info("shutdown complete")
	return nil
}
