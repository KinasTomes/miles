package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	opstest "github.com/KinasTomes/miles/internal/jobs/contracttest"
)

func TestCloudDrainMarkerIsPrivateAndBoundToInvocation(t *testing.T) {
	t.Setenv("INVOCATION_ID", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	dir := t.TempDir()
	n := opstest.Node("node-a")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- requestCloudDrain(ctx, dir, n) }()
	path := filepath.Join(dir, "drain-request.json")
	deadline := time.Now().Add(time.Second)
	for {
		data, e := os.ReadFile(path)
		if e == nil {
			var marker map[string]string
			if json.Unmarshal(data, &marker) != nil || marker["invocation"] != os.Getenv("INVOCATION_ID") || marker["node_id"] != n.ID || marker["incarnation"] != n.Incarnation {
				t.Fatal(string(data))
			}
			st, e := os.Stat(path)
			if e != nil || st.Mode().Perm() != 0600 {
				t.Fatal(st, e)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no drain marker", e)
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case e := <-done:
		t.Fatal("worker exited before controller stopped service", e)
	default:
	}
	cancel()
	if e := <-done; !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
}
func TestCloudDrainRejectsUnboundAndSymlinkMarkers(t *testing.T) {
	n := opstest.Node("node-a")
	dir := t.TempDir()
	t.Setenv("INVOCATION_ID", "")
	if e := requestCloudDrain(context.Background(), dir, n); e == nil {
		t.Fatal("unbound invocation accepted")
	}
	t.Setenv("INVOCATION_ID", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	target := filepath.Join(t.TempDir(), "foreign")
	if e := os.WriteFile(target, []byte("preserve"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink(target, filepath.Join(dir, "drain-request.json")); e != nil {
		t.Fatal(e)
	}
	if e := requestCloudDrain(context.Background(), dir, n); e == nil {
		t.Fatal("symlink marker accepted")
	}
	b, e := os.ReadFile(target)
	if e != nil || string(b) != "preserve" {
		t.Fatal(string(b), e)
	}
}
