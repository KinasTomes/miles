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
	if pages == nil || writer.LeaseID == "" {
		return PreparedObject{}, storage.ErrInvalid
	}
	pager, err := manifest.NewStreamWriter(manifest.ObjectPage, manifestID, domain,
		func(page storage.ManifestPage, digest [32]byte) error {
			return writeVerifiedPage(ctx, pages, writer, page, digest)
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

func writeVerifiedPage(ctx context.Context, pages PageStore, writer storage.WriterFence,
	page storage.ManifestPage, expected [32]byte) error {
	for attempt := 0; attempt < 3; attempt++ {
		writeErr := pages.WriteManifestPages(ctx, []storage.ManifestPage{page}, writer)
		if writeErr != nil && !errors.Is(writeErr, storage.ErrUnavailable) {
			return writeErr
		}
		actual, readErr := pages.GetManifestPage(ctx, page.ManifestID, page.Number)
		if readErr == nil {
			body, err := manifest.Encode(manifest.ObjectPage, actual)
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
