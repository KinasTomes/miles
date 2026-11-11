package faults

import (
	"context"
	"io"
	"time"

	"github.com/KinasTomes/miles/internal/storage"
)

type faultPackStore struct {
	storage.PackStore
	reg    *Registry
	nodeID string
}

// WrapPackStore wraps a storage.PackStore to inject corrupt_pack faults non-destructively on read.
func WrapPackStore(p storage.PackStore, reg *Registry, nodeID string) storage.PackStore {
	if reg == nil || p == nil {
		return p
	}
	return &faultPackStore{PackStore: p, reg: reg, nodeID: nodeID}
}

func (s *faultPackStore) RangeGet(ctx context.Context, ref storage.PackRef, span storage.ByteRange) (io.ReadCloser, error) {
	rc, err := s.PackStore.RangeGet(ctx, ref, span)
	if err != nil {
		return nil, err
	}
	if s.reg != nil && s.reg.ShouldInject(time.Now(), FaultCorruptPack, s.nodeID) {
		return &corruptReader{ReadCloser: rc}, nil
	}
	return rc, nil
}

type corruptReader struct {
	io.ReadCloser
	corrupted bool
}

func (c *corruptReader) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	if n > 0 && !c.corrupted {
		p[0] ^= 0xFF
		c.corrupted = true
	}
	return n, err
}
