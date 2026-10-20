// Package benchmarkdata creates synthetic, reproducible input data, not benchmark results.
package benchmarkdata

import (
	"bytes"
	"compress/gzip"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

type File struct {
	Path    string `json:"path"`
	Group   string `json:"group"`
	Version int    `json:"version"`
	Bytes   int    `json:"bytes"`
	SHA256  string `json:"sha256"`
}
type Manifest struct {
	SchemaVersion      int    `json:"schema_version"`
	Generator          string `json:"generator"`
	GoVersion          string `json:"go_version"`
	Profile            string `json:"profile"`
	Seed               string `json:"seed"`
	Versions           int    `json:"versions"`
	RawBytesPerVersion int    `json:"raw_bytes_per_version"`
	TotalBytes         int64  `json:"total_bytes"`
	Files              []File `json:"files"`
}

// Generate refuses an existing output path so it cannot overwrite user data.
// backup30 must be generated on a dedicated test machine, not the developer host.
func Generate(out, profile, seed string) (manifest Manifest, err error) {
	versions, size := 3, 64*1024
	switch profile {
	case "smoke":
	case "backup30":
		versions, size = 30, 8*1024*1024
	default:
		return manifest, fmt.Errorf("unknown dataset profile %q", profile)
	}
	if out == "" || seed == "" {
		return manifest, fmt.Errorf("out and seed are required")
	}
	if err = os.Mkdir(out, 0700); err != nil {
		return manifest, err
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.RemoveAll(out)
		}
	}()
	manifest = Manifest{SchemaVersion: 1, Generator: "miles-aesctr-gzip-v1", GoVersion: runtime.Version(), Profile: profile, Seed: seed, Versions: versions, RawBytesPerVersion: size, Files: []File{}}
	// Pseudorandom, not production key material. Gzip's bytes are reused unchanged.
	original := randomBytes(seed, "compressed-source", size)
	var packed bytes.Buffer
	compressor, err := gzip.NewWriterLevel(&packed, gzip.BestSpeed)
	if err != nil {
		return manifest, err
	}
	compressor.Header.OS = 255
	if _, err = compressor.Write(original); err != nil {
		return manifest, err
	}
	if err = compressor.Close(); err != nil {
		return manifest, err
	}
	for version := 0; version < versions; version++ {
		line := []byte(fmt.Sprintf("seed=%q service=miles event=backup status=ok record=repeatable\n", seed))
		low := bytes.Repeat(line, size/len(line)+1)[:size]
		// Replace a fixed 1% window; the rest stays identical across versions.
		for i := 0; i < size/100; i++ {
			low[i] = byte('A' + version)
		}
		for _, group := range []string{"low-entropy", "compressed-identical", "random-distinct"} {
			body := low
			extension := ".bin"
			if group == "compressed-identical" {
				body = packed.Bytes()
				extension = ".gz"
			}
			if group == "random-distinct" {
				body = randomBytes(seed, fmt.Sprintf("random-%d", version), size)
			}
			name := fmt.Sprintf("%s-v%02d%s", group, version, extension)
			if err = os.WriteFile(filepath.Join(out, name), body, 0600); err != nil {
				return manifest, err
			}
			sum := sha256.Sum256(body)
			manifest.Files = append(manifest.Files, File{Path: name, Group: group, Version: version, Bytes: len(body), SHA256: hex.EncodeToString(sum[:])})
			manifest.TotalBytes += int64(len(body))
		}
	}
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return manifest, err
	}
	if err = os.WriteFile(filepath.Join(out, "manifest.json"), append(encoded, '\n'), 0600); err != nil {
		return manifest, err
	}
	complete = true
	return manifest, nil
}

func randomBytes(seed, label string, size int) []byte {
	key := sha256.Sum256([]byte(seed + "\x00" + label))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		panic(err)
	}
	data := make([]byte, size)
	cipher.NewCTR(block, make([]byte, aes.BlockSize)).XORKeyStream(data, data)
	return data
}
