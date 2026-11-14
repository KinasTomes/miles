// recoverybench executes R1/R2 fault contract benchmarks.
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
	"github.com/KinasTomes/miles/internal/recoveryrunner"
	"github.com/KinasTomes/miles/internal/secretfile"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	f := flag.NewFlagSet("recoverybench", flag.ContinueOnError)
	f.SetOutput(stderr)
	config := f.String("config", "", "Private JSON config file (0600)")
	manifest := f.String("manifest", "", "Dataset manifest.json")
	out := f.String("out", "", "New private artifact directory (must not exist)")
	variant := f.String("variant", "r1", "Recovery variant: r1 or r2")
	faultScope := f.String("fault-scope", "", "Approved fault scope")
	concurrency := f.Int("concurrency", 1, "Concurrency (default 1)")
	failFast := f.Bool("fail-fast", false, "Halt on first error")
	retain := f.Bool("retain-objects", false, "Retain objects")
	systemConfigTable := f.String("system-config-table", "miles-dev-SystemConfig", "DynamoDB SystemConfig table name")
	faultCommand := f.String("fault-command", "", "Command to execute for fault injection")
	awsCredentials := f.String("aws-credentials-file", "", "Separate 0600 AWS temporary credentials JSON")
	expectedAccount := f.String("expected-aws-account", "", "Expected 12-digit AWS account ID")
	nativeConfig := f.String("native-config", "", "Private installation binding for native lease-revoke/cut/replay")

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

	opt := recoveryrunner.Options{
		NativeConfig:       *nativeConfig,
		Manifest:           *manifest,
		Out:                *out,
		Variant:            *variant,
		FaultScope:         *faultScope,
		SystemConfigTable:  *systemConfigTable,
		FaultCommand:       *faultCommand,
		AWSCredentialsFile: *awsCredentials,
		ExpectedAWSAccount: *expectedAccount,
		Concurrency:        *concurrency,
		FailFast:           *failFast,
		RetainObjects:      *retain,
	}

	res, err := recoveryrunner.Run(ctx, c, opt)
	if err != nil {
		fmt.Fprintf(stderr, "recoverybench error: %v\n", err)
		return 1
	}
	if res.Status != "passed" {
		fmt.Fprintln(stderr, "recoverybench failed")
		return 1
	}

	fmt.Fprintf(stdout, "recoverybench: variant=%s status=passed recovered_epoch=%d\n",
		res.Variant, res.RecoveryReceipt.RecoveredEpoch)
	return 0
}
