package benchmarkdata

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestReproducibilityAndIntegrity(t *testing.T) {
	root := t.TempDir()
	a, err := Generate(filepath.Join(root, "a"), "smoke", "test-seed")
	if err != nil {
		t.Fatal(err)
	}
	b, err := Generate(filepath.Join(root, "b"), "smoke", "test-seed")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatal("same inputs changed manifest")
	}
	var total int64
	groups := map[string][][]byte{}
	for _, f := range a.Files {
		body, err := os.ReadFile(filepath.Join(root, "a", f.Path))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(body)
		if f.Bytes != len(body) || f.SHA256 != hex.EncodeToString(sum[:]) {
			t.Fatal("manifest mismatch", f.Path)
		}
		total += int64(len(body))
		groups[f.Group] = append(groups[f.Group], body)
	}
	if len(a.Files) != 9 || total != a.TotalBytes {
		t.Fatal("missing files or wrong size")
	}
	if !bytes.Equal(groups["compressed-identical"][0], groups["compressed-identical"][2]) {
		t.Fatal("gzip copies differ")
	}
	reader, err := gzip.NewReader(bytes.NewReader(groups["compressed-identical"][0]))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if err = reader.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, randomBytes("test-seed", "compressed-source", a.RawBytesPerVersion)) {
		t.Fatal("gzip content not original")
	}
	low := groups["low-entropy"]
	if bytes.Equal(low[0], low[1]) || !bytes.Equal(low[0][a.RawBytesPerVersion/100:], low[1][a.RawBytesPerVersion/100:]) {
		t.Fatal("low entropy change window incorrect")
	}
	if bytes.Equal(groups["random-distinct"][0], groups["random-distinct"][1]) {
		t.Fatal("random versions repeated")
	}
	c, err := Generate(filepath.Join(root, "c"), "smoke", "different-seed")
	if err != nil {
		t.Fatal(err)
	}
	if reflect.DeepEqual(a.Files, c.Files) {
		t.Fatal("seed does not affect content")
	}
}
func TestRefusesOverwriteAndInvalidProfile(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "keep")
	if err := os.WriteFile(file, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Generate(root, "smoke", "seed"); err == nil {
		t.Fatal("overwrote existing directory")
	}
	if b, err := os.ReadFile(file); err != nil || string(b) != "keep" {
		t.Fatal("existing data damaged")
	}
	out := filepath.Join(root, "invalid")
	if _, err := Generate(out, "invalid", "seed"); err == nil {
		t.Fatal("accepted unknown profile")
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("invalid input created output")
	}
}

func TestPinnedSmokeManifest(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "benchmarks", "datasets", "smoke-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var expected Manifest
	if err = json.Unmarshal(data, &expected); err != nil {
		t.Fatal(err)
	}
	actual, err := Generate(filepath.Join(t.TempDir(), "smoke"), "smoke", expected.Seed)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(actual, expected) {
		t.Fatal("smoke fixture changed; review generator version, Go version and hashes before regenerating")
	}
}
