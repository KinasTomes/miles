// warmbench measures a pre-provisioned endpoint; it never starts AWS resources.
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

	"github.com/KinasTomes/miles/internal/benchmark"
	"github.com/KinasTomes/miles/internal/secretfile"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	f := flag.NewFlagSet("warmbench", flag.ContinueOnError)
	f.SetOutput(stderr)
	config := f.String("config", "", "Private JSON config file (0600); explicit endpoint and credentials")
	manifest := f.String("manifest", "", "Dataset manifest.json; every source file is hashed before network I/O")
	out := f.String("out", "", "New private artifact directory; must not exist")
	concurrency := f.Int("concurrency", 1, "Concurrent requests, 1..64; same value for both targets")
	repeats := f.Int("read-repeats", 3, "Additional full GET passes, 0..10 (each verifies SHA-256)")
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
	r, err := benchmark.Run(ctx, c, benchmark.Options{Manifest: *manifest, Out: *out, Concurrency: *concurrency, ReadRepeats: *repeats})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "%s: %d files, %d bytes; private artifact: %s/result.json\n", r.Status, len(r.Dataset.Files), r.Dataset.TotalBytes, *out)
	return 0
}
