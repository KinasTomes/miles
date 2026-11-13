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

type comparisonArtifact struct {
	simulator.ComparisonResult
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
	compare := f.Bool("compare", false, "Output four-policy comparative cost artifact")
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

	tHash, pHash := sha256.Sum256(rawTrace), sha256.Sum256(rawPricing)
	traceSHA := hex.EncodeToString(tHash[:])
	pricingSHA := hex.EncodeToString(pHash[:])
	tool := buildinfo.String("costsim")
	sourceRev := "unavailable"

	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" {
				sourceRev = setting.Value
			}
			if setting.Key == "vcs.modified" && setting.Value == "true" {
				tool += " dirty-source"
			}
		}
	}

	var data []byte
	if *compare {
		compResult, err := simulator.ComparePolicies(ctx, trace, catalog)
		if err != nil {
			return fail(err)
		}
		ca := comparisonArtifact{
			ComparisonResult: compResult,
			TraceSHA256:      traceSHA,
			PricingSHA256:    pricingSHA,
			Tool:             tool,
			SourceRevision:   sourceRev,
		}
		data, err = json.MarshalIndent(ca, "", "  ")
		if err != nil {
			return fail(err)
		}
	} else {
		result, err := simulator.Run(ctx, trace, catalog)
		if err != nil {
			return fail(err)
		}
		a := artifact{
			Result:         result,
			TraceSHA256:    traceSHA,
			PricingSHA256:  pricingSHA,
			Tool:           tool,
			SourceRevision: sourceRev,
		}
		data, err = json.MarshalIndent(a, "", "  ")
		if err != nil {
			return fail(err)
		}
	}

	file, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fail(errors.New("output file already exists; refusing to overwrite"))
		}
		return fail(errors.New("out must be a new file with an existing parent"))
	}
	_, writeErr := file.Write(append(data, '\n'))
	syncErr, closeErr := file.Sync(), file.Close()
	if err = errors.Join(writeErr, syncErr, closeErr); err != nil {
		return fail(err)
	}
	if *compare {
		fmt.Fprintln(stdout, "Four-policy comparative cost artifact written; priced subtotal only, no final performance gate.")
	} else {
		fmt.Fprintln(stdout, "Prototype artifact written; priced subtotal only, no final performance gate.")
	}
	return 0
}

func main() { os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr)) }
