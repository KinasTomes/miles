package cache

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestDiskCrashRecovery(t *testing.T) {
	if root := os.Getenv("MILES_CACHE_CRASH_DIR"); root != "" {
		d, err := NewDisk(root, Limits{4, 2, 4})
		if err != nil {
			t.Fatal(err)
		}
		if err := d.Put(context.Background(), testKey("a", "g"), []byte("aa")); err != nil {
			t.Fatal(err)
		}
		fmt.Println("ready")
		for {
			time.Sleep(time.Hour)
		}
	}
	root := t.TempDir()
	child := exec.Command(os.Args[0], "-test.run=^TestDiskCrashRecovery$")
	child.Env = append(os.Environ(), "MILES_CACHE_CRASH_DIR="+root)
	child.Stderr = os.Stderr
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("child readiness %q, %v", line, err)
	}
	if _, err := NewDisk(root, Limits{4, 2, 4}); err == nil {
		t.Fatal("live process lost exclusive ownership")
	}
	if blobs, _ := filepath.Glob(filepath.Join(root, "*.blob")); len(blobs) != 1 {
		t.Fatal("contender removed live cache entries")
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := child.Wait(); err == nil {
		t.Fatal("expected killed child")
	}
	d, err := NewDisk(root, Limits{4, 2, 4})
	if err != nil {
		t.Fatal("kernel did not release crashed owner", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if blobs, _ := filepath.Glob(filepath.Join(root, "*.blob")); len(blobs) != 0 {
		t.Fatal("reopen retained untrusted cache entries")
	}
}

func TestDiskPreservesLegacyOwnerAndRejectsLockSymlink(t *testing.T) {
	root := t.TempDir()
	legacy := filepath.Join(root, "owner.lock")
	if err := os.WriteFile(legacy, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewDisk(root, Limits{4, 2, 4}); !errors.Is(err, os.ErrExist) {
		t.Fatal("legacy owner was not preserved", err)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatal(err)
	}
	root = t.TempDir()
	if err := os.Symlink(filepath.Join(t.TempDir(), "foreign"), filepath.Join(root, "owner.flock")); err != nil {
		t.Fatal(err)
	}
	if _, err := NewDisk(root, Limits{4, 2, 4}); err == nil {
		t.Fatal("followed owner lock symlink")
	}
}
