package benchmark

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/KinasTomes/miles/internal/benchmarkdata"
)

func decode(data []byte, dst any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return errors.New("invalid JSON or unknown field")
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("expected one JSON document")
	}
	return nil
}

func readBounded(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil || int64(len(b)) > limit {
		return nil, errors.New("cannot read bounded input")
	}
	return b, nil
}

// Verify every local file before any request. Root prevents manifest paths or
// symlinks from escaping the dataset directory; payloads are never held in RAM.
func loadDataset(ctx context.Context, path string) (*os.Root, benchmarkdata.Manifest, []byte, error) {
	var m benchmarkdata.Manifest
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, m, nil, errors.New("cannot open dataset directory")
	}
	ok := false
	defer func() {
		if !ok {
			root.Close()
		}
	}()
	f, err := root.Open(filepath.Base(path))
	if err != nil {
		return nil, m, nil, errors.New("cannot open manifest")
	}
	b, err := readBounded(f, 1<<20)
	f.Close()
	if err != nil {
		return nil, m, nil, err
	}
	if err = decode(b, &m); err != nil {
		return nil, m, nil, err
	}
	if m.SchemaVersion != 1 || m.Generator == "" || m.GoVersion == "" || m.Seed == "" ||
		(m.Profile != "smoke" && m.Profile != "backup30") || m.Versions < 1 || m.RawBytesPerVersion < 1 || len(m.Files) == 0 || len(m.Files) > 10000 {
		return nil, m, nil, errors.New("invalid dataset manifest")
	}
	seen := map[string]bool{}
	var total int64
	for _, entry := range m.Files {
		if ctx.Err() != nil {
			return nil, m, nil, ctx.Err()
		}
		digest, e := hex.DecodeString(entry.SHA256)
		if !fs.ValidPath(entry.Path) || strings.Contains(entry.Path, "\\") || seen[entry.Path] ||
			len(digest) != sha256.Size || e != nil || entry.SHA256 != strings.ToLower(entry.SHA256) ||
			entry.Bytes < 0 || int64(entry.Bytes) > 5<<30 || entry.Version < 0 || entry.Version >= m.Versions ||
			(entry.Group != "low-entropy" && entry.Group != "compressed-identical" && entry.Group != "random-distinct") {
			return nil, m, nil, errors.New("invalid or duplicate dataset entry")
		}
		seen[entry.Path] = true
		f, e := root.Open(entry.Path)
		if e != nil {
			return nil, m, nil, errors.New("cannot open dataset file")
		}
		info, e := f.Stat()
		if e != nil || !info.Mode().IsRegular() || info.Size() != int64(entry.Bytes) {
			f.Close()
			return nil, m, nil, errors.New("dataset file type or size mismatch")
		}
		h := sha256.New()
		n, e := io.Copy(h, io.LimitReader(f, int64(entry.Bytes)+1))
		f.Close()
		if e != nil || n != int64(entry.Bytes) || hex.EncodeToString(h.Sum(nil)) != entry.SHA256 {
			return nil, m, nil, errors.New("dataset SHA-256 mismatch")
		}
		total += int64(entry.Bytes)
	}
	if total != m.TotalBytes {
		return nil, m, nil, errors.New("dataset total_bytes mismatch")
	}
	ok = true
	return root, m, b, nil
}
