// Package secretfile reads local credentials without following symlinks or accepting shared files.
package secretfile

import (
	"errors"
	"io"
	"os"
)

var ErrInvalid = errors.New("secret file must be a regular, non-symlink file with permission 0600")

func Read(path string, maxBytes int64) ([]byte, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, errors.New("cannot inspect secret file")
	}
	if !before.Mode().IsRegular() || before.Mode().Perm() != 0600 {
		return nil, ErrInvalid
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot open secret file")
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil || !os.SameFile(before, after) || !after.Mode().IsRegular() || after.Mode().Perm() != 0600 {
		return nil, ErrInvalid
	}
	b, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil || int64(len(b)) > maxBytes {
		return nil, errors.New("cannot read bounded secret file")
	}
	return b, nil
}
