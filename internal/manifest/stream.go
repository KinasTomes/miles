package manifest

import (
	"crypto/sha256"

	"github.com/KinasTomes/miles/internal/storage"
)

// StreamWriter incrementally emits the same greedy v1 pages as Paginate. It
// retains one page of refs and compact page digests for post-write verification.
type StreamWriter struct {
	kind       byte
	owner      string
	domain     string
	capacity   int
	emit       func(storage.ManifestPage, [32]byte) error
	refs       []storage.ChunkRef
	pageCount  uint32
	chunkCount uint64
	size       int64
	index      []uint64
	digests    [][32]byte
	finished   bool
}

func NewStreamWriter(kind byte, owner, domain string, emit func(storage.ManifestPage, [32]byte) error) (*StreamWriter, error) {
	capacity, err := RefCapacity(owner, domain)
	if err != nil || emit == nil || (kind != ObjectPage && kind != PartPage) {
		return nil, storage.ErrInvalid
	}
	return &StreamWriter{kind: kind, owner: owner, domain: domain, capacity: capacity, emit: emit,
		refs: make([]storage.ChunkRef, 0, capacity)}, nil
}

func (w *StreamWriter) Add(ref storage.ChunkRef) error {
	if w == nil || w.finished || ref.Key.Validate() != nil || ref.Key.Domain != w.domain || ref.RawBytes <= 0 ||
		ref.LogicalOffset != w.size || w.size > storage.MaxObjectBytes-ref.RawBytes {
		return storage.ErrInvalid
	}
	if w.kind == PartPage && w.size+ref.RawBytes > 5<<30 {
		return storage.ErrInvalid
	}
	w.refs = append(w.refs, ref)
	w.size += ref.RawBytes
	w.chunkCount++
	if len(w.refs) == w.capacity {
		return w.flush()
	}
	return nil
}

func (w *StreamWriter) flush() error {
	if w.pageCount >= MaxPages {
		return storage.ErrInvalid
	}
	page := storage.ManifestPage{Version: storage.ModelVersion, ManifestID: w.owner, Domain: w.domain,
		Number: w.pageCount, Chunks: w.refs}
	body, err := Encode(w.kind, page)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(body)
	if err := w.emit(page, digest); err != nil {
		return err
	}
	w.index = append(w.index, uint64(w.refs[0].LogicalOffset))
	w.digests = append(w.digests, digest)
	w.pageCount++
	w.refs = make([]storage.ChunkRef, 0, w.capacity)
	return nil
}

func (w *StreamWriter) Finish() error {
	if w == nil || w.finished {
		return storage.ErrInvalid
	}
	if len(w.refs) != 0 {
		if err := w.flush(); err != nil {
			return err
		}
	}
	w.finished = true
	return nil
}

func (w *StreamWriter) Descriptor() (size int64, chunks uint64, pages uint32, index []uint64, digests [][32]byte, err error) {
	if w == nil || !w.finished {
		return 0, 0, 0, nil, nil, storage.ErrInvalid
	}
	return w.size, w.chunkCount, w.pageCount, append([]uint64(nil), w.index...), append([][32]byte(nil), w.digests...), nil
}
