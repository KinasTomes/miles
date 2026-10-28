package readpath

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/KinasTomes/miles/internal/cache"
	"github.com/KinasTomes/miles/internal/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
)

func twoTierFixture(t *testing.T, w int64) (*warmFixture, *cache.Node, *cache.LRU) {
	t.Helper()
	f := newWarmFixture(t, w)
	memory, _ := cache.NewLRU(cache.Limits{CapacityBytes: 1 << 20, MaxEntries: 16, MaxCopyBytes: 1 << 20})
	disk, err := cache.NewDisk(t.TempDir(), cache.Limits{CapacityBytes: 1 << 20, MaxEntries: 16, MaxCopyBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	node, _ := cache.NewNode("test-node", memory, disk)
	t.Cleanup(func() { _ = node.Close() })
	f.reader.options.Cache = node
	return f, node, memory
}
func TestTwoTierCacheDiskHitAndRevokedAuthorization(t *testing.T) {
	f, node, memory := twoTierFixture(t, 0)
	ctx := context.Background()
	if _, out, err := f.read(ctx, f.request); err != nil || !bytes.Equal(out, f.body) {
		t.Fatal(err)
	}
	if err := memory.Purge(ctx); err != nil {
		t.Fatal(err)
	}
	calls := f.backend.count()
	_, out, err := f.read(ctx, f.request)
	if err != nil || !bytes.Equal(out, f.body) || f.backend.count() != calls || node.Stats().Hits == 0 {
		t.Fatal("disk hit fetched origin", err)
	}
	mutateWarmRecord[storage.Credential](t, f, "credentials", []string{f.request.CredentialID}, func(c *storage.Credential) { c.Active = false; c.Revision++ })
	before := node.Stats()
	_, out, err = f.read(ctx, f.request)
	if !errors.Is(err, storage.ErrAccessDenied) || len(out) != 0 || node.Stats().Lookups != before.Lookups || f.backend.count() != calls {
		t.Fatal("revoked cache hit", err)
	}
}
func TestTwoTierRangeAmplificationBoundedByIntersectingChunks(t *testing.T) {
	for _, ahead := range []int64{0, 4} {
		t.Run(fmt.Sprint(ahead), func(t *testing.T) {
			f, node, memory := twoTierFixture(t, ahead)
			ctx := context.Background()
			var fetched int64
			f.backend.beforeRange = func(_ context.Context, in *awss3.GetObjectInput) error {
				var lo, hi int64
				if _, err := fmt.Sscanf(aws.ToString(in.Range), "bytes=%d-%d", &lo, &hi); err != nil {
					return err
				}
				f.backend.mu.Lock()
				fetched += hi - lo + 1
				f.backend.mu.Unlock()
				return nil
			}
			q := f.request
			q.Range = &storage.ByteRange{Offset: int64(len(f.body) - 1), Length: 1}
			_, out, err := f.read(ctx, q)
			if err != nil || !bytes.Equal(out, f.body[len(f.body)-1:]) {
				t.Fatal(err)
			}
			// One logical byte intersects only the last compressed chunk. Read-ahead
			// must not fetch other chunks or the complete pack outside the plan.
			want := int64(f.entries[len(f.entries)-1].StoredLen)
			s := f.reader.options.Metrics.Snapshot()
			if fetched != want || s.BackendBytes != uint64(want) || s.EmittedBytes != 1 || f.backend.count() != 1 {
				t.Fatalf("amplification: fetched=%d counters=%+v want=%d", fetched, s, want)
			}
			_ = memory.Purge(ctx)
			before := node.Stats()
			if _, _, err = f.read(ctx, q); err != nil || f.backend.count() != 1 || node.Stats().Hits != before.Hits+1 {
				t.Fatal("disk hit amplified", err)
			}
			if err = node.PurgeNode(ctx, "test-node"); err != nil {
				t.Fatal(err)
			}
			if _, _, err = f.read(ctx, q); err != nil || f.backend.count() != 2 {
				t.Fatal("purge bypassed origin", err)
			}
		})
	}
}
