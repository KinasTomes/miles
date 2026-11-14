package coldrunner

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/KinasTomes/miles/internal/benchmarkdata"
	"github.com/KinasTomes/miles/internal/benchmarknative"
	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/coordination"
	"github.com/KinasTomes/miles/internal/metadata/dynamodb"
	s "github.com/KinasTomes/miles/internal/storage"
	packs3 "github.com/KinasTomes/miles/internal/storage/s3"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Snapshot is an observational receipt, never a GC or transactional read view.
// Source packs stay included after revival even when no head points at them.
type Snapshot struct {
	Binding   benchmarknative.Config    `json:"binding"`
	Bounds    s.R1Bounds                `json:"bounds"`
	Files     map[string][]s.ChunkKey   `json:"files"`
	Heads     []s.ObjectHead            `json:"heads"`
	Chunks    []s.Chunk                 `json:"chunks"`
	Packs     []s.PackInfo              `json:"packs"`
	Physical  []s.PackInfo              `json:"physical"`
	Inventory benchmarknative.Inventory `json:"inventory"`
}

func (v Snapshot) Chunk(k s.ChunkKey) (s.Chunk, bool) {
	for _, c := range v.Chunks {
		if c.Key == k {
			return c, true
		}
	}
	return s.Chunk{}, false
}
func (v Snapshot) Pack(r s.PackRef, physical bool) (s.PackInfo, bool) {
	list := v.Packs
	if physical {
		list = v.Physical
	}
	for _, p := range list {
		if p.Ref == r {
			return p, true
		}
	}
	return s.PackInfo{}, false
}

func currentPhysical(v Snapshot) ([]s.PackInfo, error) {
	refs := map[s.PackRef]bool{}
	out := []s.PackInfo{}
	for _, chunk := range v.Chunks {
		if !refs[chunk.Location.Pack] {
			p, ok := v.Pack(chunk.Location.Pack, true)
			if !ok {
				return nil, s.ErrNotFound
			}
			out = append(out, p)
			refs[p.Ref] = true
		}
	}
	return out, nil
}

func nativeSnapshot(ctx context.Context, c benchmarknative.Config, credentials string, manifest *benchmarkdata.Manifest, retained []s.PackRef) (func(context.Context) (Snapshot, error), error) {
	cfg, err := benchmarknative.Session(ctx, c, credentials)
	if err != nil {
		return nil, err
	}
	clk := clock.NewReal()
	utc := coordination.Chrony{Clock: clk, Reference: "169.254.169.123", RestartID: c.RunID + "/" + c.Slot}
	store, err := dynamodb.New(dynamodb.Options{API: ddb.NewFromConfig(cfg), Tables: c.Tables, MaxAttempts: 1,
		Authority: &dynamodb.SharedAuthority{Time: utc, Lifetime: time.Minute}})
	if err != nil {
		return nil, err
	}
	api := s3.NewFromConfig(cfg)
	physical, err := packs3.New(api, c.Bucket, c.KMSKey)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context) (Snapshot, error) {
		out := Snapshot{Binding: c, Files: map[string][]s.ChunkKey{}}
		bounds, err := utc.Bounds(ctx)
		if err != nil {
			return out, err
		}
		out.Bounds = bounds
		bucket, err := store.GetBucket(ctx, c.LogicalBucket)
		if err != nil {
			return out, err
		}
		refs := map[s.PackRef]bool{}
		for _, r := range retained {
			refs[r] = true
		}
		chunks := map[s.ChunkKey]s.Chunk{}
		for _, file := range manifest.Files {
			head, err := store.GetObject(ctx, s.ObjectKey{Bucket: bucket.Name, BucketID: bucket.ID, Key: file.Path})
			if err != nil {
				return out, err
			}
			if head.Deleted || head.Size != int64(file.Bytes) {
				return out, errors.New("native trial head missing or size differs")
			}
			out.Heads = append(out.Heads, head)
			pages, err := store.GetManifest(ctx, head.ManifestID)
			if err != nil {
				return out, err
			}
			for _, page := range pages {
				for start := 0; start < len(page.Chunks); start += 100 {
					keys := []s.ChunkKey{}
					for _, chunk := range page.Chunks[start:min(start+100, len(page.Chunks))] {
						keys = append(keys, chunk.Key)
					}
					rows, err := store.GetChunks(ctx, keys, true)
					if err != nil {
						return out, err
					}
					for _, key := range keys {
						chunk, ok := rows[key]
						if !ok {
							return out, s.ErrNotFound
						}
						chunks[key] = chunk
						refs[chunk.Location.Pack] = true
						out.Files[file.Path] = append(out.Files[file.Path], key)
					}
				}
			}
		}
		for _, chunk := range chunks {
			out.Chunks = append(out.Chunks, chunk)
		}
		sort.Slice(out.Chunks, func(i, j int) bool {
			a, b := out.Chunks[i].Key, out.Chunks[j].Key
			if a.Domain == b.Domain {
				return a.Hash < b.Hash
			}
			return a.Domain < b.Domain
		})
		ordered := []s.PackRef{}
		for r := range refs {
			ordered = append(ordered, r)
		}
		sort.Slice(ordered, func(i, j int) bool {
			a, b := ordered[i], ordered[j]
			if a.ID == b.ID {
				return a.Generation < b.Generation
			}
			return a.ID < b.ID
		})
		for start := 0; start < len(ordered); start += 100 {
			rows, err := store.GetPacks(ctx, ordered[start:min(start+100, len(ordered))])
			if err != nil {
				return out, err
			}
			for _, r := range ordered[start:min(start+100, len(ordered))] {
				p, ok := rows[r]
				if !ok {
					return out, s.ErrNotFound
				}
				out.Packs = append(out.Packs, p)
				native, err := physical.HeadPack(ctx, r)
				if err != nil {
					return out, err
				}
				out.Physical = append(out.Physical, native)
			}
		}
		out.Inventory, err = benchmarknative.Collect(ctx, api, c)
		return out, err
	}, nil
}

// SelectProbes refuses overlapping chunk sets: the first PUT must not warm the
// second UploadPart's input before that independent Cold/Restored test starts.
func SelectProbes(m *benchmarkdata.Manifest, v Snapshot) ([]string, error) {
	selected := []string{}
	seen := map[s.ChunkKey]bool{}
	for _, f := range m.Files {
		keys := v.Files[f.Path]
		if f.Group != "random-distinct" || len(keys) == 0 {
			continue
		}
		overlap := false
		for _, k := range keys {
			if seen[k] {
				overlap = true
			}
		}
		if overlap {
			continue
		}
		selected = append(selected, f.Path)
		for _, k := range keys {
			seen[k] = true
		}
		if len(selected) == 4 {
			return selected, nil
		}
	}
	return nil, errors.New("Cold lifecycle requires four independent random-distinct chunk sets")
}

func SameBinding(a, b benchmarknative.Config) bool {
	// stop_at and credentials can be refreshed in the SAME paid window; identity,
	// data scope, source and history cannot move when resuming a trial.
	a.StopAt = time.Time{}
	b.StopAt = time.Time{}
	return a.Account == b.Account && a.RoleARN == b.RoleARN && a.Region == b.Region && a.Bucket == b.Bucket && a.KMSKey == b.KMSKey &&
		a.RunID == b.RunID && a.Slot == b.Slot && a.Source == b.Source && a.History == b.History && a.Incarnation == b.Incarnation && a.LogicalBucket == b.LogicalBucket && sameMap(a.Tables, b.Tables) && sameMap(a.Tags, b.Tags)
}
func sameMap(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// VerifyReplacement proves native CAS, durable Warm publication, protection and
// retention of every old physical payload version. It never uses a debug count
// or a logical storage-class header as a substitute for these observations.
func VerifyReplacement(before, after Snapshot, file string, ackLower time.Time, reuseIR bool) error {
	if !SameBinding(before.Binding, after.Binding) || len(before.Files[file]) == 0 {
		return errors.New("revival binding/input changed")
	}
	oldPacks := map[s.PackRef]bool{}
	for _, key := range before.Files[file] {
		old, ok := before.Chunk(key)
		if !ok {
			return s.ErrNotFound
		}
		next, ok := after.Chunk(key)
		if !ok {
			return s.ErrNotFound
		}
		p, ok := after.Pack(next.Location.Pack, false)
		if !ok {
			return s.ErrNotFound
		}
		native, ok := after.Pack(next.Location.Pack, true)
		if !ok {
			return s.ErrNotFound
		}
		if p.ProtectedUntil.Before(ackLower.Add(15 * time.Minute)) {
			return errors.New("published location lacks R3 protection")
		}
		if reuseIR {
			if old.Location != next.Location || old.LocationRevision != next.LocationRevision || p.Class != s.GlacierIR || native.Class != s.GlacierIR {
				return errors.New("IR readable dedup unexpectedly moved location")
			}
		} else {
			if old.Location.Pack == next.Location.Pack || next.LocationRevision <= old.LocationRevision || p.State != "Warm" || p.Class != s.Standard || native.Class != s.Standard {
				return errors.New("revival did not CAS into durable Warm generation")
			}
			oldPacks[old.Location.Pack] = true
		}
	}
	for r := range oldPacks {
		old, _ := before.Pack(r, false)
		retained, ok := after.Pack(r, false)
		if !ok || retained.Class != s.Glacier || retained.MinRetentionUntil.Before(old.MinRetentionUntil) || !after.Bounds.Upper.Before(retained.MinRetentionUntil) {
			return errors.New("old Cold retention protection missing")
		}
		native, ok := after.Pack(r, true)
		if !ok || native.Class != s.Glacier {
			return errors.New("old Cold generation was changed or removed")
		}
		sidecar := false
		versions := 0
		for _, v := range before.Inventory.Native.Objects {
			if v.Pack == r && v.Category == "pack" {
				versions++
				found := false
				for _, w := range after.Inventory.Native.Objects {
					if v.Key == w.Key && v.VersionID == w.VersionID && v.Size == w.Size {
						found = true
					}
				}
				if !found {
					return errors.New("retained Cold payload version disappeared")
				}
			}
		}
		if versions == 0 {
			return errors.New("source Cold version evidence missing")
		}
		for _, v := range after.Inventory.Native.Objects {
			if v.Pack == r && v.Category == "sidecar" {
				sidecar = true
			}
		}
		if !sidecar {
			return errors.New("retained Cold sidecar disappeared")
		}
	}
	return nil
}
