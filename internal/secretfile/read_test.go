package secretfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPrivateBoundedRegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if b, err := Read(path, 6); err != nil || string(b) != "secret" {
		t.Fatal("private file rejected")
	}
	if _, err := Read(path, 5); err == nil {
		t.Fatal("unbounded read")
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(path, 10); err == nil {
		t.Fatal("accepted public secret")
	}
	os.Chmod(path, 0600)
	link := path + "-link"
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(link, 10); err == nil {
		t.Fatal("followed symlink")
	}
}
