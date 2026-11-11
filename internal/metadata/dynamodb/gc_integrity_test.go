package dynamodb

import (
	"bytes"
	"context"
	"errors"
	"hash/crc32"
	"io"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/lifecycle"
	"github.com/KinasTomes/miles/internal/pack"
	s "github.com/KinasTomes/miles/internal/storage"
)

// Fault only the source read. Destination writes and every metadata operation
// still use the worker and native DynamoDB adapter under test.
type gcSourceFault struct {
	*lifePhysical
	source s.PackRef
	read   func([]byte) ([]byte, error)
}

func (p gcSourceFault) RangeGet(ctx context.Context, ref s.PackRef, r s.ByteRange) (io.ReadCloser, error) {
	body, err := p.lifePhysical.RangeGet(ctx, ref, r)
	if err != nil || ref != p.source {
		return body, err
	}
	b, err := io.ReadAll(body)
	closeErr := body.Close()
	if err != nil || closeErr != nil {
		return nil, errors.Join(err, closeErr)
	}
	b, err = p.read(b)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func TestLocalCompactionRejectsCorruptionAndExpiredWriter(t *testing.T) {
	for _, mode := range []string{"crc", "hash", "short", "oversized", "io", "expired-writer"} {
		t.Run(mode, func(t *testing.T) {
			m, w, j, policy, utc, physical, chunk := lifeFixture(t, "compaction", s.Standard)
			ctx := t.Context()
			if mode == "hash" {
				// A valid stored CRC cannot substitute for the raw content hash.
				chunk.Location.CRC32C = crc32.Checksum([]byte("evil"), crc32.MakeTable(crc32.Castagnoli))
				if err := m.update(ctx, func(tx *transaction) error { return put(tx, chunkKey(chunk.Key), chunk) }); err != nil {
					t.Fatal(err)
				}
			}
			w.Marker = &lifecycle.Marker{Store: m, Pinned: m.ObservePackPins, MaxRecords: 1000, MaxReferences: 1000,
				RecoveryRoots: func(context.Context) ([]s.PackRef, error) { return nil, nil }}
			w.Packs = gcSourceFault{lifePhysical: physical, source: chunk.Location.Pack, read: func(b []byte) ([]byte, error) {
				switch mode {
				case "crc", "hash":
					return []byte("evil"), nil
				case "short":
					return b[:len(b)-1], nil
				case "oversized":
					return append(b, 0), nil
				case "io":
					return nil, s.ErrUnavailable
				case "expired-writer":
					utc.Advance(31 * time.Second)
				}
				return b, nil
			}}
			out, err := w.Step(ctx, j, policy)
			if err == nil || out.Done {
				t.Fatal("invalid source published", out, err)
			}
			var formatErr *pack.FormatError
			switch mode {
			case "io":
				if !errors.Is(err, s.ErrUnavailable) || errors.As(err, &formatErr) {
					t.Fatal("I/O misclassified as corruption", err)
				}
			case "expired-writer":
				if !errors.Is(err, s.ErrFenced) {
					t.Fatal("expired writer not fenced", err)
				}
			default:
				if !errors.As(err, &formatErr) {
					t.Fatal("missing integrity error", err)
				}
			}
			rows, err := m.GetChunks(ctx, []s.ChunkKey{chunk.Key}, true)
			if err != nil || rows[chunk.Key] != chunk {
				t.Fatal("failed compaction changed canonical location", rows, err)
			}
			dest := lifecycle.Destination(j.Type, j.ID, chunk.Location.Pack)
			packs, err := m.GetPacks(ctx, []s.PackRef{dest})
			if err != nil || len(packs) != 0 {
				t.Fatal("failed preflight registered destination", packs, err)
			}
			if _, err := physical.HeadPack(ctx, chunk.Location.Pack); err != nil {
				t.Fatal("lost source", err)
			}
			if _, err := m.GetLifecycle(ctx, j.ID); !errors.Is(err, s.ErrNotFound) {
				t.Fatal("failed preflight prepared intent", err)
			}
		})
	}
}
