// pricingcheck validates catalog inputs without estimating costs or calling AWS.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/KinasTomes/miles/internal/pricing"
)

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("pricingcheck", flag.ContinueOnError)
	flags.SetOutput(stderr)
	file := flags.String("file", "configs/pricing/pricing.ap-southeast-1.example.json", "Pricing catalog JSON path")
	var required []string
	flags.Func("require-rate", "Required verified rate ID (repeatable)", func(id string) error {
		required = append(required, id)
		return nil
	})
	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "Unexpected positional arguments")
		return 2
	}
	data, err := os.ReadFile(*file)
	var catalog *pricing.Catalog
	if err == nil {
		catalog, err = pricing.Parse(data, required...)
	}
	if err != nil {
		fmt.Fprintf(stderr, "Pricing validation failed: %v\n", err)
		return 1
	}
	verified := 0
	for _, rate := range catalog.Rates {
		if rate.Status == "verified" {
			verified++
		}
	}
	fmt.Fprintf(stdout, "Contract valid: %d rates, %d verified, %d unpriced. No cost/benchmark calculated.\n", len(catalog.Rates), verified, len(catalog.Rates)-verified)
	return 0
}

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }
