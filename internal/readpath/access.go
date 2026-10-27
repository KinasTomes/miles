package readpath

import (
	"context"
	"sort"
	"time"

	"github.com/KinasTomes/miles/internal/storage"
)

const accessBatchSize = 100

// persistPackAccess borrows a synchronous UpdateAccessStats method, not a new
// store/authority port. Input is one max observation per exact generation, bounded
// by the request's manifest limit. The backend owns monotonic-max persistence.
// Failure stops the flush; there is no retry, timer, goroutine or per-chunk write.
func persistPackAccess(ctx context.Context, observed map[storage.PackRef]time.Time, update func(context.Context, []storage.PackAccess) error) error {
	refs := make([]storage.PackRef, 0, len(observed))
	for ref := range observed {
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].ID != refs[j].ID {
			return refs[i].ID < refs[j].ID
		}
		return refs[i].Generation < refs[j].Generation
	})
	for start := 0; start < len(refs); start += accessBatchSize {
		if err := ctx.Err(); err != nil {
			return err
		}
		end := min(start+accessBatchSize, len(refs))
		batch := make([]storage.PackAccess, 0, end-start)
		for _, ref := range refs[start:end] {
			batch = append(batch, storage.PackAccess{Pack: ref, LastAccess: observed[ref]})
		}
		if err := update(ctx, batch); err != nil {
			return err
		}
	}
	return nil
}
