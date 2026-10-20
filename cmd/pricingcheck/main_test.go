package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommand(t *testing.T) {
	fixture, err := filepath.Abs(filepath.Join("..", "..", "configs", "pricing", "pricing.ap-southeast-1.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	invalid := filepath.Join(t.TempDir(), "invalid.json")
	if err := os.WriteFile(invalid, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		args   []string
		code   int
		output string
	}{
		{"valid catalog and repeated requirements", []string{"--file", fixture, "--require-rate", "ddb.storage", "--require-rate", "network.nat_hour"}, 0, "50 rates, 30 verified, 20 unpriced"},
		{"unpriced required rate", []string{"--file", fixture, "--require-rate", "ec2.compute"}, 1, "missing verified price"},
		{"unknown required rate", []string{"--file", fixture, "--require-rate", "missing"}, 1, "missing verified price"},
		{"missing file", []string{"--file", filepath.Join(t.TempDir(), "missing.json")}, 1, "Pricing validation failed"},
		{"invalid catalog", []string{"--file", invalid}, 1, "pricing schema"},
		{"unknown flag", []string{"--unknown"}, 2, "flag provided but not defined"},
		{"unexpected argument", []string{"extra"}, 2, "Unexpected positional arguments"},
		{"help", []string{"--help"}, 0, "Usage of pricingcheck"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(tc.args, &stdout, &stderr); code != tc.code {
				t.Fatalf("exit %d, want %d: %s", code, tc.code, stderr.String())
			}
			if !strings.Contains(stdout.String()+stderr.String(), tc.output) {
				t.Fatalf("missing %q: stdout=%s stderr=%s", tc.output, stdout.String(), stderr.String())
			}
			if tc.code != 0 && stdout.Len() != 0 {
				t.Fatal("validation failure printed a success summary")
			}
		})
	}
}
