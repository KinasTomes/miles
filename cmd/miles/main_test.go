package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestVersionAndInvalidArguments(t *testing.T) {
	var out, log bytes.Buffer
	if err := run(context.Background(), []string{"-version"}, &out, &log); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "miles ") {
		t.Fatal(out.String())
	}
	for _, args := range [][]string{{"unexpected"}, {"-invalid"}, {"-config", "/nonexistent/miles.json"}} {
		if err := run(context.Background(), args, &out, &log); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}
