// datasetgen generates local synthetic inputs. It never contacts AWS.
package main

import (
	"flag"
	"fmt"
	"github.com/KinasTomes/miles/internal/benchmarkdata"
	"os"
)

func main() {
	out := flag.String("out", "", "New output directory (must not exist)")
	profile := flag.String("profile", "smoke", "smoke or backup30; generate backup30 only on a dedicated test host")
	seed := flag.String("seed", "miles-w02-v1", "Reproducible synthetic-data seed")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "Unexpected positional arguments")
		os.Exit(2)
	}
	m, err := benchmarkdata.Generate(*out, *profile, *seed)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("Generated %d files, %d bytes; manifest: %s/manifest.json (no benchmark run)\n", len(m.Files), m.TotalBytes, *out)
}
