// costsim produces an offline accounting prototype artifact; it never calls AWS.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime/debug"

	"github.com/KinasTomes/miles/internal/buildinfo"
	"github.com/KinasTomes/miles/internal/pricing"
	"github.com/KinasTomes/miles/internal/simulator"
)

type artifact struct {
	simulator.Result
	TraceSHA256    string `json:"trace_sha256"`
	PricingSHA256  string `json:"pricing_sha256"`
	Tool           string `json:"tool"`
	SourceRevision string `json:"source_revision"`
}

func readFile(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(b)) > limit {
		return nil, errors.New("input cannot be read or exceeds size limit")
	}
	return b, nil
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	f := flag.NewFlagSet("costsim", flag.ContinueOnError)
	f.SetOutput(stderr)
	tracePath := f.String("trace", "", "Version 1 accounting trace JSON")
	pricingPath := f.String("pricing", "configs/pricing/pricing.ap-southeast-1.example.json", "Shared pricing catalog JSON")
	out := f.String("out", "", "New private result JSON file; parent must exist")
	if err := f.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if f.NArg() != 0 || *tracePath == "" || *out == "" {
		fmt.Fprintln(stderr, "require -trace and -out; no positional arguments")
		return 2
	}
	fail := func(err error) int { fmt.Fprintln(stderr, "costsim:", err); return 1 }
	rawTrace, err := readFile(*tracePath, simulator.MaxTraceBytes)
	if err != nil {
		return fail(err)
	}
	trace, err := simulator.Read(bytes.NewReader(rawTrace))
	if err != nil {
		return fail(err)
	}
	rawPricing, err := readFile(*pricingPath, 2<<20)
	if err != nil {
		return fail(err)
	}
	catalog, err := pricing.Parse(rawPricing)
	if err != nil {
		return fail(err)
	}
	result, err := simulator.Run(ctx, trace, catalog)
	if err != nil {
		return fail(err)
	}
	tHash, pHash := sha256.Sum256(rawTrace), sha256.Sum256(rawPricing)
	a := artifact{Result: result, TraceSHA256: hex.EncodeToString(tHash[:]), PricingSHA256: hex.EncodeToString(pHash[:]), Tool: buildinfo.String("costsim"), SourceRevision: "unavailable"}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" {
				a.SourceRevision = setting.Value
			}
			if setting.Key == "vcs.modified" && setting.Value == "true" {
				a.Tool += " dirty-source"
			}
		}
	}
	data, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return fail(err)
	}
	file, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fail(errors.New("out must be a new file with an existing parent"))
	}
	_, writeErr := file.Write(append(data, '\n'))
	syncErr, closeErr := file.Sync(), file.Close()
	if err = errors.Join(writeErr, syncErr, closeErr); err != nil {
		return fail(err)
	}
	fmt.Fprintln(stdout, "Prototype artifact written; priced subtotal only, no final performance gate.")
	return 0
}

func main() { os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr)) }
