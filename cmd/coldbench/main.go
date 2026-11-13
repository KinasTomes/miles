// coldbench measures pre-provisioned S3 Cold storage classes (GLACIER_IR and GLACIER).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/KinasTomes/miles/internal/benchmark"
	"github.com/KinasTomes/miles/internal/coldrunner"
	"github.com/KinasTomes/miles/internal/secretfile"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	f := flag.NewFlagSet("coldbench", flag.ContinueOnError)
	f.SetOutput(stderr)
	config := f.String("config", "", "Private JSON config file (0600)")
	manifest := f.String("manifest", "", "Dataset manifest.json")
	out := f.String("out", "", "New private artifact directory (must not exist)")
	variant := f.String("variant", "ir", "Cold variant: ir or flexible")
	concurrency := f.Int("concurrency", 1, "Concurrency (default 1)")
	failFast := f.Bool("fail-fast", false, "Halt on first error")
	retain := f.Bool("retain-objects", false, "Retain objects")
	pollInterval := f.Duration("poll-interval", 5*time.Second, "Native restore HEAD polling interval")
	maxPollWait := f.Duration("max-poll-wait", 15*time.Minute, "Bounded native restore completion window")

	if err := f.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if f.NArg() != 0 || *config == "" || *manifest == "" || *out == "" {
		fmt.Fprintln(stderr, "require --config, --manifest, --out and no positional arguments")
		return 2
	}

	b, err := secretfile.Read(*config, 32<<10)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	var c benchmark.Config
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err = d.Decode(&c); err != nil || d.Decode(new(any)) != io.EOF {
		fmt.Fprintln(stderr, "invalid config JSON or unknown field")
		return 1
	}

	opt := coldrunner.Options{
		Manifest:      *manifest,
		Out:           *out,
		Variant:       *variant,
		Concurrency:   *concurrency,
		FailFast:      *failFast,
		RetainObjects: *retain,
		PollInterval:  *pollInterval,
		MaxPollWait:   *maxPollWait,
	}

	res, err := coldrunner.Run(ctx, c, opt)
	if err != nil {
		fmt.Fprintf(stderr, "coldbench error: %v\n", err)
		return 1
	}
	if res.Status != "passed" {
		fmt.Fprintln(stderr, "coldbench failed")
		return 1
	}

	fmt.Fprintf(stdout, "coldbench: variant=%s status=passed observed_class=%s\n",
		res.Variant, res.ColdReceipt.ObservedBackendStorageClass)
	return 0
}
