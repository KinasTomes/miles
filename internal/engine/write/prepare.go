package write

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"github.com/KinasTomes/miles/internal/manifest"
	"github.com/KinasTomes/miles/internal/storage"
)

type PageStore interface {
	WriteManifestPages(context.Context, []storage.ManifestPage, storage.WriterFence) error
	GetManifestPage(context.Context, string, uint32) (storage.ManifestPage, error)
}

type PreparedObject struct {
	ManifestID string
	Domain     string
	Size       int64
	ChunkCount uint64
	PageCount  uint32
	PageIndex  []uint64
	PackCount  uint64
}

// BindHead copies the complete prepared descriptor onto a candidate head. It
// does not publish, assign revision/ETag, authorize or release any protection.
func (p PreparedObject) BindHead(h storage.ObjectHead) (storage.ObjectHead, error) {
	if h.Deleted || h.Revision != 0 || h.Domain != p.Domain {
		return storage.ObjectHead{}, storage.ErrInvalid
	}
	h.ManifestID, h.Size, h.PageCount, h.ChunkCount = p.ManifestID, p.Size, p.PageCount, p.ChunkCount
	h.PageIndex = append([]uint64(nil), p.PageIndex...)
	if err := h.ValidateManifestDescriptor(); err != nil {
		return storage.ObjectHead{}, err
	}
	return h, nil
}

func NewManifestID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// PrepareObject writes every physical dependency and immutable manifest page,
// but never publishes an object head. The caller validates the logical body
// and uses the returned descriptor in its separately fenced CommitObject.
func PrepareObject(ctx context.Context, pipeline Pipeline, pages PageStore, writer storage.WriterFence,
	body io.Reader, domain, manifestID string) (PreparedObject, error) {
	return prepare(ctx, manifest.ObjectPage, pipeline, pages, writer, body, domain, manifestID)
}

// prepare streams body into packs and immutable pages of kind (object pages,
// or part pages through a PageStore over storage.PartPageStore).
func prepare(ctx context.Context, kind byte, pipeline Pipeline, pages PageStore, writer storage.WriterFence,
	body io.Reader, domain, manifestID string) (PreparedObject, error) {
	if pages == nil || writer.LeaseID == "" {
		return PreparedObject{}, storage.ErrInvalid
	}
	pager, err := manifest.NewStreamWriter(kind, manifestID, domain,
		func(page storage.ManifestPage, digest [32]byte) error {
			return writeVerifiedPage(ctx, kind, pages, writer, page, digest)
		})
	if err != nil {
		return PreparedObject{}, err
	}
	result, err := pipeline.Run(ctx, body, domain, pager.Add)
	if err != nil {
		return PreparedObject{}, err
	}
	if err := pager.Finish(); err != nil {
		return PreparedObject{}, err
	}
	size, chunks, pageCount, index, _, err := pager.Descriptor()
	if err != nil || size != result.Size || chunks != result.ChunkCount {
		return PreparedObject{}, fmt.Errorf("%w: incomplete manifest", storage.ErrUnavailable)
	}
	return PreparedObject{ManifestID: manifestID, Domain: domain, Size: size, ChunkCount: chunks,
		PageCount: pageCount, PageIndex: index, PackCount: result.PackCount}, nil
}

// CopyManifest writes manifestID as a same-domain copy of the committed source
// head (architecture 5.3): the same refs in the same order, no payload read.
// Before a ref is emitted its canonical row is pinned and its pack protection
// extended through Reuse, exactly like a dedup hit of PUT, so the chunk
// outlives the copy's publication. A chunk Reuse refuses (an archived pack)
// fails the copy with ErrCold; metadata copy of Cold sources is W06 work.
func CopyManifest(ctx context.Context, publisher Publisher, lookup Lookup, pages PageStore, writer storage.WriterFence,
	source storage.ObjectHead, manifestID string) (PreparedObject, error) {
	if publisher == nil || lookup == nil || pages == nil || writer.LeaseID == "" || source.Deleted || source.ValidateManifestDescriptor() != nil {
		return PreparedObject{}, storage.ErrInvalid
	}
	pager, err := manifest.NewStreamWriter(manifest.ObjectPage, manifestID, source.Domain,
		func(page storage.ManifestPage, digest [32]byte) error {
			return writeVerifiedPage(ctx, manifest.ObjectPage, pages, writer, page, digest)
		})
	if err != nil {
		return PreparedObject{}, err
	}
	for n := uint32(0); n < source.PageCount; n++ {
		page, err := pages.GetManifestPage(ctx, source.ManifestID, n)
		if err != nil {
			return PreparedObject{}, sourceRecord(err)
		}
		if page.ManifestID != source.ManifestID || page.Domain != source.Domain || page.Number != n {
			return PreparedObject{}, fmt.Errorf("%w: source manifest page", storage.ErrInvalid)
		}
		for start := 0; start < len(page.Chunks); start += maxBatchKeys {
			batch := page.Chunks[start:min(start+maxBatchKeys, len(page.Chunks))]
			if err := reuseRefs(ctx, publisher, lookup, batch); err != nil {
				return PreparedObject{}, err
			}
			for _, ref := range batch {
				if err := pager.Add(ref); err != nil {
					return PreparedObject{}, err
				}
			}
		}
	}
	if err := pager.Finish(); err != nil {
		return PreparedObject{}, err
	}
	size, chunks, pageCount, index, _, err := pager.Descriptor()
	if err != nil || size != source.Size || chunks != source.ChunkCount {
		return PreparedObject{}, fmt.Errorf("%w: copied manifest does not match its source", storage.ErrInvalid)
	}
	return PreparedObject{ManifestID: manifestID, Domain: source.Domain, Size: size, ChunkCount: chunks,
		PageCount: pageCount, PageIndex: index}, nil
}

// reuseRefs pins every distinct chunk of one batch of source refs.
func reuseRefs(ctx context.Context, publisher Publisher, lookup Lookup, refs []storage.ChunkRef) error {
	keys := make([]storage.ChunkKey, 0, len(refs))
	seen := make(map[storage.ChunkKey]bool, len(refs))
	for _, ref := range refs {
		if !seen[ref.Key] {
			seen[ref.Key] = true
			keys = append(keys, ref.Key)
		}
	}
	rows, err := lookupComplete(ctx, lookup, keys)
	if err != nil {
		return err
	}
	for _, key := range keys {
		row, ok := rows[key]
		if !ok || row.Key != key {
			return fmt.Errorf("%w: source chunk has no canonical row", storage.ErrUnavailable)
		}
		reused, err := publisher.Reuse(ctx, row)
		if err != nil {
			return sourceRecord(err)
		}
		if !reused {
			return storage.ErrCold
		}
	}
	return nil
}

// sourceRecord reports a missing page or pack of a committed source head as
// backend inconsistency, never as an absent key the client could act on.
func sourceRecord(err error) error {
	if errors.Is(err, storage.ErrNotFound) {
		return fmt.Errorf("%w: copy source record missing: %v", storage.ErrUnavailable, err)
	}
	return err
}

func writeVerifiedPage(ctx context.Context, kind byte, pages PageStore, writer storage.WriterFence,
	page storage.ManifestPage, expected [32]byte) error {
	for attempt := 0; attempt < 3; attempt++ {
		writeErr := pages.WriteManifestPages(ctx, []storage.ManifestPage{page}, writer)
		if writeErr != nil && !errors.Is(writeErr, storage.ErrUnavailable) {
			return writeErr
		}
		actual, readErr := pages.GetManifestPage(ctx, page.ManifestID, page.Number)
		if readErr == nil {
			body, err := manifest.Encode(kind, actual)
			if err != nil || sha256.Sum256(body) != expected {
				return fmt.Errorf("%w: manifest page mismatch", storage.ErrConflict)
			}
			return nil
		}
		if writeErr == nil && !errors.Is(readErr, storage.ErrUnavailable) {
			return readErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return storage.ErrUnavailable
}
