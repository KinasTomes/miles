package main

import (
	"bytes"
	"context"
	"testing"
)

func TestFlags(t *testing.T) {
	for _, tc := range []struct {
		args []string
		code int
		text string
	}{
		{[]string{"--help"}, 0, "Concurrent requests"},
		{nil, 2, "require --config"},
		{[]string{"--unknown"}, 2, "flag provided but not defined"},
		{[]string{"--config", "missing", "--manifest", "missing", "--out", "missing"}, 1, "cannot inspect secret file"},
	} {
		var out, err bytes.Buffer
		if code := run(context.Background(), tc.args, &out, &err); code != tc.code || !bytes.Contains(err.Bytes(), []byte(tc.text)) {
			t.Fatalf("args=%v code=%d stderr=%s", tc.args, code, err.String())
		}
	}
}
