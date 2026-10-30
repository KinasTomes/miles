package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestCLIProducesPrivateReproducibleArtifactWithoutOverwriting(t *testing.T) {
	out := filepath.Join(t.TempDir(), "result.json")
	args := []string{"-trace", "../../benchmarks/traces/edge-revival.json", "-pricing", "../../configs/pricing/pricing.ap-southeast-1.example.json", "-out", out}
	var stdout, stderr bytes.Buffer
	if exit := run(context.Background(), args, &stdout, &stderr); exit != 0 {
		t.Fatal(exit, stderr.String())
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var a artifact
	if err = json.Unmarshal(b, &a); err != nil || len(a.TraceSHA256) != 64 || len(a.PricingSHA256) != 64 || a.Metadata != "badger" || a.Cost.CompleteTotalUSD != nil {
		t.Fatal(a, err)
	}
	info, err := os.Stat(out)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("artifact is not private", err)
	}
	if run(context.Background(), args, &stdout, &stderr) != 1 {
		t.Fatal("existing artifact overwritten")
	}
	again, _ := os.ReadFile(out)
	if !bytes.Equal(b, again) {
		t.Fatal("existing artifact changed")
	}
	if run(context.Background(), []string{"-trace", args[1]}, &stdout, &stderr) != 2 {
		t.Fatal("missing arguments accepted")
	}
	if run(context.Background(), []string{"-help"}, &stdout, &stderr) != 0 {
		t.Fatal("help failed")
	}
}
