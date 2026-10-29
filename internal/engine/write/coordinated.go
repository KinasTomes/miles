package write

import (
	"context"
	"io"

	"github.com/KinasTomes/miles/internal/coordination"
	"github.com/KinasTomes/miles/internal/policy"
	"github.com/KinasTomes/miles/internal/storage"
)

// CoordinationStore is a consumer projection of existing ports, not a second
// MetadataStore or persistence implementation. Auth/quota/CAS/outbox stay there.
type CoordinationStore interface {
	CanonicalStore
	PageStore
	coordination.OutcomeReader
	ReserveLogicalQuota(context.Context, storage.Reservation) error
	CommitObject(context.Context, storage.CommitRequest) (storage.CommitResult, error)
	DeleteObject(context.Context, storage.CommitRequest) (storage.CommitResult, error)
	Authorize(context.Context, string, string) (storage.Authorization, error)
}

// CoordinatedMetadata checks the opaque registration before each prepare or
// publication mutation. Backend fences are forwarded unchanged, never installed
// or extended here. Caller retains Writer through commit/outcome reconciliation.
type CoordinatedMetadata struct {
	store  CoordinationStore
	writer *coordination.Writer
}

var (
	_ CanonicalStore    = (*CoordinatedMetadata)(nil)
	_ PageStore         = (*CoordinatedMetadata)(nil)
	_ ReuseGuard        = (*CoordinatedMetadata)(nil)
	_ storage.PackStore = coordinatedPacks{}
)

func BindCoordination(store CoordinationStore, packs storage.PackStore, w *coordination.Writer) (*CoordinatedMetadata, storage.PackStore, error) {
	if store == nil || packs == nil || w == nil {
		return nil, nil, storage.ErrInvalid
	}
	if _, alreadyBound := packs.(coordinatedPacks); alreadyBound {
		return nil, nil, storage.ErrInvalid
	}
	if err := w.Check(context.Background()); err != nil {
		return nil, nil, err
	}
	m := &CoordinatedMetadata{store, w}
	return m, coordinatedPacks{PackStore: coordination.ProtectPackStore(packs), writer: w}, nil
}
func (m *CoordinatedMetadata) checkFence(f storage.WriterFence) error {
	if m == nil || m.writer == nil || f != m.writer.Fence() {
		return storage.ErrFenced
	}
	return nil
}
func (m *CoordinatedMetadata) GetChunks(ctx context.Context, k []storage.ChunkKey, c bool) (map[storage.ChunkKey]storage.Chunk, error) {
	if err := m.writer.Check(ctx); err != nil {
		return nil, err
	}
	return m.store.GetChunks(ctx, k, c)
}
func (m *CoordinatedMetadata) GetPacks(ctx context.Context, r []storage.PackRef) (map[storage.PackRef]storage.PackInfo, error) {
	if err := m.writer.Check(ctx); err != nil {
		return nil, err
	}
	return m.store.GetPacks(ctx, r)
}
func (m *CoordinatedMetadata) GetManifestPage(ctx context.Context, id string, n uint32) (storage.ManifestPage, error) {
	return m.store.GetManifestPage(ctx, id, n)
}
func (m *CoordinatedMetadata) GetCommitOutcome(ctx context.Context, id string) (storage.CommitResult, error) {
	return m.store.GetCommitOutcome(ctx, id)
}
func (m *CoordinatedMetadata) RegisterUploadingPack(ctx context.Context, p storage.PackInfo, f storage.WriterFence) error {
	if err := m.checkFence(f); err != nil {
		return err
	}
	return m.writer.Do(ctx, "register_pack/"+p.Ref.ID+"/"+p.Ref.Generation, func(f storage.WriterFence) error { return m.store.RegisterUploadingPack(ctx, p, f) })
}
func (m *CoordinatedMetadata) MarkPackWarm(ctx context.Context, p storage.PackRef, r uint64, f storage.WriterFence) (out storage.PackInfo, err error) {
	if err = m.checkFence(f); err != nil {
		return
	}
	err = m.writer.Do(ctx, "warm/"+p.ID+"/"+p.Generation, func(f storage.WriterFence) error { out, err = m.store.MarkPackWarm(ctx, p, r, f); return err })
	return
}
func (m *CoordinatedMetadata) ExtendPackProtection(ctx context.Context, r storage.PackProtectionUpdate) (out storage.PackInfo, err error) {
	if err = m.checkFence(r.Writer); err != nil {
		return
	}
	err = m.writer.Do(ctx, "protect/"+r.Chunk.Location.Pack.ID+"/"+r.Chunk.Location.Pack.Generation, func(f storage.WriterFence) error {
		r.Writer = f
		out, err = m.store.ExtendPackProtection(ctx, r)
		return err
	})
	return
}
func (m *CoordinatedMetadata) PutChunkIfAbsent(ctx context.Context, c storage.Chunk, f storage.WriterFence) (out storage.Chunk, inserted bool, err error) {
	if err = m.checkFence(f); err != nil {
		return
	}
	err = m.writer.Do(ctx, "insert/"+c.Key.Domain+"/"+c.Key.Hash, func(f storage.WriterFence) error {
		out, inserted, err = m.store.PutChunkIfAbsent(ctx, c, f)
		return err
	})
	return
}
func (m *CoordinatedMetadata) CompareAndSwapChunkLocation(ctx context.Context, k storage.ChunkKey, r uint64, l storage.ChunkLocation, f storage.WriterFence) (out storage.Chunk, err error) {
	if err = m.checkFence(f); err != nil {
		return
	}
	err = m.writer.Do(ctx, "location/"+k.Domain+"/"+k.Hash, func(f storage.WriterFence) error {
		out, err = m.store.CompareAndSwapChunkLocation(ctx, k, r, l, f)
		return err
	})
	return
}
func (m *CoordinatedMetadata) WriteManifestPages(ctx context.Context, p []storage.ManifestPage, f storage.WriterFence) error {
	if err := m.checkFence(f); err != nil {
		return err
	}
	return m.writer.Do(ctx, "manifest", func(f storage.WriterFence) error { return m.store.WriteManifestPages(ctx, p, f) })
}
func (m *CoordinatedMetadata) ReserveLogicalQuota(ctx context.Context, r storage.Reservation) error {
	if err := m.checkFence(r.Writer); err != nil {
		return err
	}
	return m.writer.Do(ctx, "reserve/"+r.ID, func(storage.WriterFence) error { return m.store.ReserveLogicalQuota(ctx, r) })
}
func (m *CoordinatedMetadata) CommitObject(ctx context.Context, r storage.CommitRequest) (out storage.CommitResult, err error) {
	if err = m.checkFence(r.Writer); err != nil {
		return
	}
	err = m.writer.Do(ctx, "commit/"+r.Head.OpID, func(storage.WriterFence) error { out, err = m.store.CommitObject(ctx, r); return err })
	if err != nil {
		return storage.CommitResult{}, err
	}
	return
}

func (m *CoordinatedMetadata) DeleteObject(ctx context.Context, r storage.CommitRequest) (out storage.CommitResult, err error) {
	if err = m.checkFence(r.Writer); err != nil {
		return
	}
	err = m.writer.Do(ctx, "commit/"+r.Head.OpID, func(storage.WriterFence) error { out, err = m.store.DeleteObject(ctx, r); return err })
	if err != nil {
		return storage.CommitResult{}, err
	}
	return
}

// Pin implements ReuseGuard for Warm/IR candidates. Protection attaches to the exact
// generation and is retained by Writer. Re-reading checks location revision;
// this is a local no-maintenance handoff, not a distributed pin/CAS transaction.
func (m *CoordinatedMetadata) Pin(ctx context.Context, row storage.Chunk) (bool, error) {
	if row.Validate() != nil {
		return false, storage.ErrInvalid
	}
	rows, err := m.GetChunks(ctx, []storage.ChunkKey{row.Key}, true)
	if err != nil {
		return false, err
	}
	if rows[row.Key] != row {
		return false, storage.ErrConflict
	}
	infos, err := m.GetPacks(ctx, []storage.PackRef{row.Location.Pack})
	if err != nil {
		return false, err
	}
	info, ok := infos[row.Location.Pack]
	if !ok {
		return false, storage.ErrNotFound
	}
	reusable, err := policy.WriteReuseCandidate(info.State, info.Class)
	if err != nil || !reusable {
		return false, err
	}
	if info.Ref != row.Location.Pack || row.Location.Range.Validate(info.Size) != nil {
		return false, storage.ErrInvalid
	}
	if err := m.writer.Pin(ctx, row.Location.Pack); err != nil {
		return false, err
	}
	rows, err = m.GetChunks(ctx, []storage.ChunkKey{row.Key}, true)
	if err != nil {
		return false, err
	}
	if rows[row.Key] != row {
		return false, storage.ErrConflict
	}
	return true, nil
}

type coordinatedPacks struct {
	storage.PackStore
	writer *coordination.Writer
}

func (p coordinatedPacks) ReadSidecar(ctx context.Context, ref storage.PackRef) ([]byte, error) {
	if err := p.writer.Check(ctx); err != nil {
		return nil, err
	}
	return p.PackStore.(interface {
		ReadSidecar(context.Context, storage.PackRef) ([]byte, error)
	}).ReadSidecar(ctx, ref)
}

func (p coordinatedPacks) PutPack(ctx context.Context, info storage.PackInfo, body io.Reader) error {
	if info.Writer != p.writer.Fence() {
		return storage.ErrFenced
	}
	return p.writer.Do(ctx, "pack/"+info.Ref.ID+"/"+info.Ref.Generation, func(storage.WriterFence) error { return p.PackStore.PutPack(ctx, info, body) })
}
func (p coordinatedPacks) PutSidecar(ctx context.Context, ref storage.PackRef, size int64, body io.Reader) error {
	return p.writer.Do(ctx, "sidecar/"+ref.ID+"/"+ref.Generation, func(storage.WriterFence) error { return p.PackStore.PutSidecar(ctx, ref, size, body) })
}
