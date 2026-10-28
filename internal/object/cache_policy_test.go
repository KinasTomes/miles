package object

import (
	"bytes"
	"context"
	"testing"

	"github.com/KinasTomes/miles/internal/cache"
	"github.com/KinasTomes/miles/internal/config"
)

// Exercise policy selection through real object -> session -> publisher and
// read-through behavior. Disabling write admission must not disable read caching.
func TestObjectWriteCachePolicyAndReadThrough(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		name := "disabled"
		if enabled {
			name = "enabled"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			memory, _ := cache.NewLRU(cache.Limits{CapacityBytes: 1 << 20, MaxEntries: 128, MaxCopyBytes: 1 << 20})
			disk, err := cache.NewDisk(t.TempDir(), cache.Limits{CapacityBytes: 1 << 20, MaxEntries: 128, MaxCopyBytes: 1 << 20})
			if err != nil {
				t.Fatal(err)
			}
			node, _ := cache.NewNode("dev-node", memory, disk)
			defer node.Close()
			f := newServiceFixtureCache(t, node)
			original := f.options.BucketPolicy
			f.options.Cache = node
			f.options.BucketPolicy = func(id string) (config.BucketSnapshot, error) {
				snapshot, err := original(id)
				if err != nil {
					return snapshot, err
				}
				v, err := config.NewPolicyValidator(config.DSGPolicyLimits())
				if err != nil {
					return snapshot, err
				}
				policy := snapshot.Cache()
				policy.AdmitOnWrite = enabled
				return v.NewBucketSnapshot(id, snapshot.Revision(), policy)
			}
			f.service, err = New(f.options)
			if err != nil {
				t.Fatal(err)
			}
			data := bytes.Repeat([]byte("cache-policy-body"), 2000)
			f.put(t, ctx, "cache-policy", data)
			s := node.Stats()
			if enabled && s.Puts == 0 {
				t.Fatal("write policy not forwarded")
			}
			if !enabled && s.Puts != 0 {
				t.Fatal("disabled write admitted")
			}
			// Both reader and writer use the same node cache. Disabling write
			// admission does not disable later read-through admission.
			_, got := f.get(t, ctx, "cache-policy", nil)
			if !bytes.Equal(got, data) {
				t.Fatal("read after write")
			}
			first := node.Stats()
			if enabled && first.Misses != 0 {
				t.Fatal("write-admitted bytes missed", first)
			}
			if !enabled && (first.Misses == 0 || first.Puts == 0) {
				t.Fatal("read-through disabled", first)
			}
			_, got = f.get(t, ctx, "cache-policy", nil)
			second := node.Stats()
			if !bytes.Equal(got, data) || second.Misses != first.Misses || second.Hits <= first.Hits {
				t.Fatal("repeat read did not hit", second)
			}
		})
	}
}
