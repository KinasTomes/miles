package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/KinasTomes/miles/internal/api/admin"
	"github.com/KinasTomes/miles/internal/app"
	"github.com/KinasTomes/miles/internal/buildinfo"
	"github.com/KinasTomes/miles/internal/config"
)

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("miles", flag.ContinueOnError)
	flags.SetOutput(stderr)
	version := flags.Bool("version", false, "Hiển thị phiên bản")
	file := flags.String("config", "", "Startup JSON config (default runtime is unready until adapters are wired)")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	if *version {
		fmt.Fprintln(stdout, buildinfo.String("miles"))
		return nil
	}
	c := config.DefaultStartup()
	if *file != "" {
		f, err := os.Open(*file)
		if err != nil {
			return err
		}
		c, err = config.LoadStartup(f)
		closeErr := f.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	d, err := app.Bootstrap(c)
	if err != nil {
		return err
	}
	if c.S3Address != "" {
		d.S3Handler = func(a *app.Admission) http.Handler { return s3Handler(c.Region, d, a) }
	}
	if c.AdminAddress != "" && d.Admin != nil {
		h, err := adminHandler(*d.Admin, stderr)
		if err != nil {
			if d.Close != nil {
				err = errors.Join(err, d.Close())
			}
			return err
		}
		d.AdminHandler = h
	}
	return app.Run(ctx, c, d, stderr)
}

// AdminRequestTimeout bounds one Admin request, including its metadata write.
const AdminRequestTimeout = 30 * time.Second

func adminHandler(options admin.Options, logs io.Writer) (func(*app.Admission) http.Handler, error) {
	options.Logger = slog.New(slog.NewJSONHandler(logs, nil))
	server, err := admin.NewServer(options)
	if err != nil {
		return nil, err
	}
	return func(a *app.Admission) http.Handler {
		return requestGate(a, AdminRequestTimeout, server, adminUnavailable)
	}, nil
}

func adminUnavailable(w http.ResponseWriter, _ *http.Request) {
	var id [16]byte
	_, _ = rand.Read(id[:])
	request := hex.EncodeToString(id[:])
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Request-Id", request)
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = io.WriteString(w, `{"error":{"code":"unavailable","message":"gateway is draining","request_id":"`+request+`"}}`)
}
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "miles:", err)
		os.Exit(1)
	}
}
