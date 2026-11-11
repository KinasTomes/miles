package dynamodb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/KinasTomes/miles/internal/cache"
	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/coordination"
	"github.com/KinasTomes/miles/internal/engine"
	"github.com/KinasTomes/miles/internal/jobs"
	"github.com/KinasTomes/miles/internal/lifecycle"
	"github.com/KinasTomes/miles/internal/object"
	"github.com/KinasTomes/miles/internal/pack"
	"github.com/KinasTomes/miles/internal/readpath"
	s "github.com/KinasTomes/miles/internal/storage"
	"math/big"
	"testing"
	"time"
)

func restoreReader(t *testing.T, m *Store, utc *r1Clock, ref s.PackRef) (s.WriterFence, s.ObjectHead) {
	t.Helper()
	p, e := m.ObservePage(t.Context(), s.ObservationRequest{Kind: "heads", Limit: 100})
	if e != nil || len(p.Heads) != 1 {
		t.Fatal(p, e)
	}
	head := p.Heads[0]
	f := s.WriterFence{LeaseID: "cold-reader", Epoch: 1, ServiceIncarnation: "shared-service"}
	if _, e = m.AcquireR1(t.Context(), s.R1Acquire{Fence: f, Owner: "reader", Kind: "reader", Object: head.Object, Deadline: utc.Now().Add(5 * time.Minute)}); e != nil {
		t.Fatal(e)
	}
	if e = m.BindR1(t.Context(), f, head); e != nil {
		t.Fatal(e)
	}
	if e = m.PinR1(t.Context(), f, []s.PackRef{ref}); e != nil {
		t.Fatal(e)
	}
	if e = m.ResolvedR1(t.Context(), f); e != nil {
		t.Fatal(e)
	}
	return f, head
}

func TestLocalReadRestoreAtomicAdmissionAndDedup(t *testing.T) {
	m, _, _, _, utc, _, chunk := lifeFixture(t, "pack_restore", s.Glacier)
	f, head := restoreReader(t, m, utc, chunk.Location.Pack)
	required := utc.Now().Add(7 * time.Minute)
	first, created, e := m.QueueReadRestore(t.Context(), f, head, chunk.Location.Pack, 1, required, nil)
	if e != nil || !created || first.ID == "" || first.State != jobs.Queued {
		t.Fatal(first, created, e)
	}
	// RestoreObject shares the generation's job: no second restore, no override.
	second, created, e := m.QueueReadRestore(t.Context(), f, head, chunk.Location.Pack, 1, required.Add(time.Second), &s.RestoreRequest{Days: 5, Tier: "Bulk"})
	if e != nil || created || second.ID != first.ID || second.Revision != 1 {
		t.Fatal(second, created, e)
	}
	params, e := lifecycle.Decode(first.Params, first.Type)
	if e != nil || params.RequiredUntil.Before(required) || params.Days != 1 || params.Tier != "Standard" {
		t.Fatal(params, e)
	}
	bad := head
	bad.ManifestID = "unbound-root"
	if _, _, e = m.QueueReadRestore(t.Context(), f, bad, chunk.Location.Pack, 1, required, nil); !errors.Is(e, s.ErrFenced) {
		t.Fatal("root bypass", e)
	}
	unpinned := s.PackRef{ID: chunk.Location.Pack.ID, Generation: "existing-unpinned"}
	if e = m.update(t.Context(), func(tx *transaction) error {
		p, e := get[s.PackInfo](tx, packKey(chunk.Location.Pack))
		if e != nil {
			return e
		}
		p.Ref = unpinned
		return put(tx, packKey(unpinned), p)
	}); e != nil {
		t.Fatal(e)
	}
	if _, _, e = m.QueueReadRestore(t.Context(), f, head, unpinned, 1, required, nil); e == nil {
		t.Fatal("unpinned restore")
	}
	if e = m.update(t.Context(), func(tx *transaction) error {
		b, e := get[s.Bucket](tx, key("buckets", head.Object.Bucket))
		if e != nil {
			return e
		}
		b.Active = false
		return put(tx, key("buckets", b.Name), b)
	}); e != nil {
		t.Fatal(e)
	}
	if _, _, e = m.QueueReadRestore(t.Context(), f, head, chunk.Location.Pack, 1, required, nil); !errors.Is(e, s.ErrFenced) {
		t.Fatal("inactive bucket queued restore", e)
	}
	if e = m.update(t.Context(), func(tx *transaction) error {
		b, e := get[s.Bucket](tx, key("buckets", head.Object.Bucket))
		if e != nil {
			return e
		}
		b.Active = true
		return put(tx, key("buckets", b.Name), b)
	}); e != nil {
		t.Fatal(e)
	}
	if e = m.CloseR1(t.Context(), f); e != nil {
		t.Fatal(e)
	}
	if _, _, e = m.QueueReadRestore(t.Context(), f, head, chunk.Location.Pack, 1, required, nil); e == nil {
		t.Fatal("closed reader queued restore")
	}
}

// RestoreObject carries the caller's Days/Tier through the same pinned reader
// admission; bad parameters queue nothing.
func TestLocalExplicitRestoreUsesRequestParameters(t *testing.T) {
	m, _, _, _, utc, _, chunk := lifeFixture(t, "pack_restore", s.Glacier)
	f, head := restoreReader(t, m, utc, chunk.Location.Pack)
	required := utc.Now().Add(7 * time.Minute)
	if _, _, e := m.QueueReadRestore(t.Context(), f, head, chunk.Location.Pack, 1, required, &s.RestoreRequest{Days: 31, Tier: "Bulk"}); !errors.Is(e, s.ErrInvalid) {
		t.Fatal("31 days queued", e)
	}
	job, created, e := m.QueueReadRestore(t.Context(), f, head, chunk.Location.Pack, 1, required, &s.RestoreRequest{Days: 5, Tier: "Bulk"})
	if e != nil || !created {
		t.Fatal(job, e)
	}
	params, e := lifecycle.Decode(job.Params, job.Type)
	if e != nil || params.Days != 5 || params.Tier != "Bulk" || params.RequiredUntil.Before(required) {
		t.Fatal(params, e)
	}
}

func TestLocalReadRestoreLostACKReconcilesStableJob(t *testing.T) {
	m, _, _, _, utc, _, chunk := lifeFixture(t, "pack_restore", s.Glacier)
	f, head := restoreReader(t, m, utc, chunk.Location.Pack)
	fault := &lifeAckAPI{API: m.api, attribute: "job_id"}
	m.api = fault
	one, _, e := m.QueueReadRestore(t.Context(), f, head, chunk.Location.Pack, 1, utc.Now().Add(7*time.Minute), nil)
	if e == nil || !fault.fired || one.ID == "" {
		t.Fatal("missing injected ACK loss", one, e, fault.fired)
	}
	stored, e := m.GetJob(t.Context(), one.ID)
	if e != nil || stored.State != jobs.Queued {
		t.Fatal(stored, e)
	}
	again, created, e := m.QueueReadRestore(t.Context(), f, head, chunk.Location.Pack, 1, utc.Now().Add(7*time.Minute), nil)
	if e != nil || created || again.ID != one.ID || again.Revision != 1 {
		t.Fatal(again, e)
	}
	page, e := m.ListJobs(t.Context(), jobs.PageRequest{Limit: 100, ActiveOnly: true})
	if e != nil || len(page.Jobs) != 2 {
		t.Fatal(page, e)
	}
}

func TestLocalRestoreExtensionWaitsForRequiredExpiry(t *testing.T) {
	m, w, j, snapshot, utc, physical, chunk := lifeFixture(t, "pack_restore", s.Glacier)
	// Model a still-readable temporary copy whose expiry is too short for GET.
	if e := physical.Packs.Restore(t.Context(), chunk.Location.Pack, s.RestoreRequest{Days: 1, Tier: "Standard"}); e != nil {
		t.Fatal(e)
	}
	if e := physical.CompleteRestore(chunk.Location.Pack, utc.Now().Add(time.Minute)); e != nil {
		t.Fatal(e)
	}
	if e := m.update(t.Context(), func(tx *transaction) error {
		p, e := get[s.PackInfo](tx, packKey(chunk.Location.Pack))
		if e != nil {
			return e
		}
		p.State = "Restored"
		p.RestoredUntil = utc.Now().Add(time.Minute)
		return put(tx, packKey(p.Ref), p)
	}); e != nil {
		t.Fatal(e)
	}
	params, _ := lifecycle.Decode(j.Params, j.Type)
	params.RequiredUntil = utc.Now().Add(7 * time.Minute)
	j.Params, _ = json.Marshal(params)
	if e := m.update(t.Context(), func(tx *transaction) error { return put(tx, key("jobs", j.ID), j) }); e != nil {
		t.Fatal(e)
	}
	out, e := w.Step(t.Context(), j, snapshot)
	if e != nil || out.Done || !out.Settled || physical.calls != 1 {
		t.Fatal(out, e, physical.calls)
	}
	if e = physical.CompleteRestore(chunk.Location.Pack, params.RequiredUntil.Add(-time.Nanosecond)); e != nil {
		t.Fatal(e)
	}
	out, e = w.Step(t.Context(), j, snapshot)
	if e != nil || out.Done {
		t.Fatal("premature restore readiness", out, e)
	}
	// A later backend observation extends expiry; it is not computed from Days.
	if e = physical.Packs.Restore(t.Context(), chunk.Location.Pack, s.RestoreRequest{Days: 1, Tier: "Standard"}); e != nil {
		t.Fatal(e)
	}
	if e = physical.CompleteRestore(chunk.Location.Pack, params.RequiredUntil); e != nil {
		t.Fatal(e)
	}
	out, e = w.Step(t.Context(), j, snapshot)
	if e != nil || !out.Done || physical.calls != 1 {
		t.Fatal(out, e, physical.calls)
	}
}

func TestLocalColdGCRetentionAndRoots(t *testing.T) {
	for _, class := range []s.StorageClass{s.GlacierIR, s.Glacier} {
		for _, mode := range []string{"before-retention", "missing-retention", "root-protected", "eligible"} {
			t.Run(string(class)+"/"+mode, func(t *testing.T) {
				m, w, j, snapshot, utc, physical, chunk := lifeFixture(t, "gc", class)
				if e := m.update(t.Context(), func(tx *transaction) error {
					p, e := get[s.PackInfo](tx, packKey(chunk.Location.Pack))
					if e != nil {
						return e
					}
					p.MinRetentionUntil = utc.Now()
					if mode == "before-retention" {
						p.MinRetentionUntil = utc.Now().Add(time.Nanosecond)
					}
					if mode == "missing-retention" {
						p.MinRetentionUntil = time.Time{}
					}
					if e = put(tx, packKey(p.Ref), p); e != nil {
						return e
					}
					page, e := m.ObservePage(t.Context(), s.ObservationRequest{Kind: "heads", Limit: 100})
					if e != nil {
						return e
					}
					h := page.Heads[0]
					return put(tx, headKey(h.Object), s.ObjectHead{Version: 1, Object: h.Object, Domain: h.Domain, Revision: h.Revision + 1, OpID: h.OpID, Deleted: true})
				}); e != nil {
					t.Fatal(e)
				}
				w.Marker = &lifecycle.Marker{Store: m, Pinned: m.ObservePackPins, MaxRecords: 1000, MaxReferences: 1000, RecoveryRoots: func(context.Context) ([]s.PackRef, error) {
					if mode == "root-protected" {
						return []s.PackRef{chunk.Location.Pack}, nil
					}
					return nil, nil
				}}
				w.Sweep = &sweepPhysicalFixture{}
				out, e := w.Step(t.Context(), j, snapshot)
				if mode != "eligible" {
					if e == nil && out.Done {
						t.Fatal("unsafe cold retirement", out)
					}
				} else {
					if e != nil || !out.Done {
						t.Fatal(out, e)
					}
					packs, e := m.GetPacks(t.Context(), []s.PackRef{chunk.Location.Pack})
					if e != nil || packs[chunk.Location.Pack].State != "Retiring" {
						t.Fatal(packs, e)
					}
					if _, e = physical.HeadPack(t.Context(), chunk.Location.Pack); e != nil {
						t.Fatal("retire deleted source", e)
					}
				}
			})
		}
	}
}

func TestLocalTieringDiscoveryQueuesOnlyEligibleChildren(t *testing.T) {
	m, w, j, snapshot, utc, physical, _ := lifeFixture(t, "tiering", s.GlacierIR)
	j.Params = []byte(`{}`)
	if e := m.update(t.Context(), func(tx *transaction) error { return put(tx, key("jobs", j.ID), j) }); e != nil {
		t.Fatal(e)
	}
	svc := jobs.Service{Store: m, Policies: m, Now: utc.Now}
	w.Jobs = &svc
	for range 2 {
		out, e := w.Step(t.Context(), j, snapshot)
		if e != nil || !out.Done || !out.Settled || physical.calls != 0 {
			t.Fatal(out, e, physical.calls)
		}
	}
	page, e := m.ListJobs(t.Context(), jobs.PageRequest{Limit: 100, ActiveOnly: true})
	if e != nil || len(page.Jobs) != 2 {
		t.Fatal(page, e)
	}
	for _, child := range page.Jobs {
		if child.ID != j.ID && (child.PolicyRevision != snapshot.Revision() || child.Type != "tiering" || child.State != jobs.Queued) {
			t.Fatal(child)
		}
	}
	// Discovery cannot send S3 work. Its old native creation fence must close
	// before another worker may repeat the stable child enqueue.
	if e = m.LifecycleQuiescent(t.Context(), j.ID, j.Worker); e != nil {
		t.Fatal(e)
	}
	oldFence := j.Worker
	if e = m.CloseR1(t.Context(), oldFence); e != nil {
		t.Fatal(e)
	}
	if e = m.ReleaseR1(t.Context(), oldFence); e != nil {
		t.Fatal(e)
	}
	newFence := oldFence
	newFence.Epoch++
	if _, e = m.AcquireR1(t.Context(), s.R1Acquire{Fence: newFence, Owner: "new-discovery-worker", Kind: "control", OpID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Deadline: utc.Now().Add(5 * time.Minute)}); e != nil {
		t.Fatal(e)
	}
	j, e = m.ChangeJob(t.Context(), jobs.Change{JobID: j.ID, ExpectedRevision: j.Revision, Kind: "claim", Fence: newFence, NodeID: j.WorkerNodeID, NodeIncarnation: j.WorkerNodeIncarnation})
	if e != nil || j.PendingStep != "" {
		t.Fatal(j, e)
	}
	if out, e := w.Step(t.Context(), j, snapshot); e != nil || !out.Done {
		t.Fatal(out, e)
	}
	if _, e = svc.CreateNamed(jobs.WithCreationFence(t.Context(), oldFence), jobs.Create{Type: "tiering", Params: []byte(`{}`), PolicyRevision: 1}, "child_stale_discovery"); e == nil {
		t.Fatal("old discovery created a child after closure")
	}
}

func TestLocalColdCompactionNeverReadsOrMutatesPayload(t *testing.T) {
	for _, class := range []s.StorageClass{s.GlacierIR, s.Glacier} {
		t.Run(string(class), func(t *testing.T) {
			_, w, j, policy, _, physical, _ := lifeFixture(t, "compaction", class)
			out, e := w.Step(t.Context(), j, policy)
			if e == nil || !out.Settled || physical.calls != 0 {
				t.Fatal(out, e, physical.calls)
			}
		})
	}
}

func TestLocalRetiredColdSweepRequiresEveryProtection(t *testing.T) {
	for _, class := range []s.StorageClass{s.GlacierIR, s.Glacier} {
		for _, mode := range []string{"min-retention", "recovery-root", "cut-unverified", "eligible", "unknown-delete"} {
			t.Run(string(class)+"/"+mode, func(t *testing.T) {
				m, w, j, policy, utc, _, row := lifeFixture(t, "gc", s.Standard)
				rows, e := m.GetPacks(t.Context(), []s.PackRef{row.Location.Pack})
				if e != nil {
					t.Fatal(e)
				}
				dead := rows[row.Location.Pack]
				dead.Ref = s.PackRef{ID: "cold-retired", Generation: "generation-1"}
				dead.Class = class
				dead.State = "Retiring"
				dead.MinRetentionUntil = utc.Now()
				if mode == "min-retention" {
					dead.MinRetentionUntil = utc.Now().Add(time.Nanosecond)
				}
				retired := utc.Now().Add(-24*time.Hour - 10*time.Minute)
				j.Params, _ = json.Marshal(lifecycle.Params{PackID: dead.Ref.ID, Generation: dead.Ref.Generation})
				if e = m.update(t.Context(), func(tx *transaction) error {
					if e := put(tx, packKey(dead.Ref), dead); e != nil {
						return e
					}
					if e := r1Put(tx, sweepRetirementKey(dead.Ref), sweepRetirement{Version: 1, Ref: dead.Ref, At: retired}); e != nil {
						return e
					}
					return put(tx, key("jobs", j.ID), j)
				}); e != nil {
					t.Fatal(e)
				}
				w.Marker = &lifecycle.Marker{Store: m, Pinned: m.ObservePackPins, MaxRecords: 1000, MaxReferences: 1000, RecoveryRoots: func(context.Context) ([]s.PackRef, error) {
					if mode == "recovery-root" {
						return []s.PackRef{dead.Ref}, nil
					}
					return nil, nil
				}}
				w.VerifyCutAfter = func(_ context.Context, at time.Time) error {
					if !at.Equal(retired) {
						return s.ErrInvalid
					}
					if mode == "cut-unverified" {
						return s.ErrUnavailable
					}
					return nil
				}
				physical := &sweepPhysicalFixture{unknown: mode == "unknown-delete"}
				w.Sweep = physical
				out, e := w.Step(t.Context(), j, policy)
				if mode == "eligible" {
					if e != nil || !out.Done || len(physical.calls) != 2 {
						t.Fatal(out, e, physical.calls)
					}
				} else if mode == "unknown-delete" {
					if e == nil || out.Settled {
						t.Fatal(out, e)
					}
					before := len(physical.calls)
					if _, e = w.Step(t.Context(), j, policy); e == nil || len(physical.calls) != before {
						t.Fatal("unknown delete retried", e, physical.calls)
					}
				} else if len(physical.calls) != 0 || e == nil && out.Done {
					t.Fatal("cold protection bypass", out, e, physical.calls)
				}
			})
		}
	}
}

func TestCrossDomainCopyColdSourceAutoRestoreDynamoDB(t *testing.T) {
	m, _, _, snapshot, utc, physical, chunk := lifeFixture(t, "pack_restore", s.Glacier)
	ctx := t.Context()

	// 1. Get source object head
	p, e := m.ObservePage(ctx, s.ObservationRequest{Kind: "heads", Limit: 100})
	if e != nil || len(p.Heads) != 1 {
		t.Fatal(p, e)
	}
	srcHead := p.Heads[0]

	// 2. Create destination bucket in other-domain with owner quota
	b2 := s.Bucket{Name: "dst-bkt", ID: "bucket-dst", OwnerID: "owner", Domain: "other-domain", Revision: 1, Active: true}
	if e := m.update(ctx, func(tx *transaction) error {
		if err := put(tx, key("buckets", b2.Name), b2); err != nil {
			return err
		}
		return put(tx, key("quotas", b2.OwnerID), s.LogicalQuota{OwnerID: b2.OwnerID, Limit: 1 << 30})
	}); e != nil {
		t.Fatal(e)
	}

	// 3. Set up object.Service on DynamoDB
	local, err := clock.NewVirtual(utc.Now(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	lru, err := cache.NewLRU(cache.Limits{CapacityBytes: 1 << 20, MaxEntries: 16, MaxCopyBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	gate, err := coordination.NewSharedR1Gate(local, "shared-service", "reader", coordination.LeaseLimits{Lifetime: 30 * time.Second, Heartbeat: 5 * time.Second}, m, utc)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := coordination.NewWriterRegistry(gate, coordination.LeaseLimits{Lifetime: 30 * time.Second, Heartbeat: 5 * time.Second}, coordination.Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	admission, err := readpath.NewAdmission(readpath.Limits{
		Node:   readpath.ScopeLimits{ActiveRequests: 100, Budget: warmBudget(64 << 20)},
		Bucket: readpath.ScopeLimits{ActiveRequests: 100, Budget: warmBudget(64 << 20)},
	})
	if err != nil {
		t.Fatal(err)
	}
	reader, err := readpath.NewWarmReader(readpath.WarmOptions{
		Metadata: m, Packs: physical, Cache: lru, Admission: admission, Gate: gate, Clock: local, UTC: utc,
		Global: snapshot, Metrics: &readpath.WarmMetrics{}, TransportBytes: 4096,
		MaxManifestPages: 4, MaxManifestChunks: 20000,
	})
	if err != nil {
		t.Fatal(err)
	}
	validator, err := config.NewPolicyValidator(config.DSGPolicyLimits())
	if err != nil {
		t.Fatal(err)
	}
	cachePolicy := config.DefaultCachePolicy()
	svc, err := object.New(object.Options{
		Metadata: m, Packs: physical, Cache: lru, Registry: registry, Clock: local, ReadUTC: utc,
		Restore: func(ctx context.Context, view *coordination.WarmView, head s.ObjectHead, ref s.PackRef, revision uint64, required time.Time, explicit *s.RestoreRequest) (bool, error) {
			f, err := view.SharedFence(ctx)
			if err != nil {
				return false, err
			}
			_, created, err := m.QueueReadRestore(ctx, f, head, ref, revision, required, explicit)
			return created, err
		},
		AdmitWriter: func(ctx context.Context, r object.WriterRequest) (object.AdmittedWriter, error) {
			mutation, w, e := registry.AdmitProducer(ctx, r.Object, r.OpID, r.Deadline)
			if e != nil {
				return object.AdmittedWriter{}, e
			}
			_, stop, e := w.KeepAlive(ctx)
			if e != nil {
				_ = w.Release()
				return object.AdmittedWriter{}, e
			}
			protected := r.WallDeadline.Add(15 * time.Minute)
			return object.AdmittedWriter{Producer: &mutation, Fence: mutation.Writer, ProtectedUntil: protected, Finish: func() { stop(); _ = w.Release() }}, nil
		},
		Chunking:    engine.ChunkingParams{Min: 16 << 10, Avg: 16 << 10, Max: 16 << 10},
		Compression: engine.CompressionPolicy{Enabled: true, Codec: "zstd", Level: 3, MinSavingRatio: big.NewRat(1, 10)},
		Events:      object.MetadataEvents{}, Reader: reader,
		BucketPolicy: func(id string) (config.BucketSnapshot, error) {
			return validator.NewBucketSnapshot(id, 1, cachePolicy)
		},
		Budget:         warmBudget(64 << 20),
		MaxOutstanding: 3,
		RequestTimeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Jobs before copy
	jobsBefore, e := m.ListJobs(ctx, jobs.PageRequest{Limit: 100, ActiveOnly: true})
	if e != nil {
		t.Fatal(e)
	}

	// 4. Case 1: Missing chunk in destination domain: CopyObject fails with ErrCold and ENQUEUES a restore job
	_, err = svc.CopyObject(ctx, object.CopyRequest{
		Source:    s.Bucket{Name: srcHead.Object.Bucket, ID: srcHead.Object.BucketID, Domain: srcHead.Domain},
		SourceKey: srcHead.Object.Key,
		Bucket:    b2, Key: "dst-missing",
		UserID: b2.OwnerID, CredentialID: "owner-key",
	})
	if !errors.Is(err, s.ErrCold) {
		t.Fatalf("expected ErrCold, got %v", err)
	}

	// Verify the restore job was enqueued in DynamoDB
	jobsAfter, e := m.ListJobs(ctx, jobs.PageRequest{Limit: 100, ActiveOnly: true})
	if e != nil || len(jobsAfter.Jobs) != len(jobsBefore.Jobs)+1 {
		t.Fatalf("expected 1 new restore job enqueued, before=%d after=%d: %v", len(jobsBefore.Jobs), len(jobsAfter.Jobs), e)
	}
	var latestJob *jobs.Job
	beforeIDs := map[string]bool{}
	for _, j := range jobsBefore.Jobs {
		beforeIDs[j.ID] = true
	}
	for i := range jobsAfter.Jobs {
		if !beforeIDs[jobsAfter.Jobs[i].ID] {
			latestJob = &jobsAfter.Jobs[i]
			break
		}
	}
	if latestJob == nil {
		t.Fatalf("no new restore job found among %+v", jobsAfter.Jobs)
	}
	if latestJob.Type != "pack_restore" || latestJob.State != jobs.Queued {
		t.Fatalf("unexpected job: %+v", *latestJob)
	}
	params, e := lifecycle.Decode(latestJob.Params, latestJob.Type)
	if e != nil || params.PackID != chunk.Location.Pack.ID {
		t.Fatalf("expected restore for pack %q, got params: %+v, err: %v", chunk.Location.Pack.ID, params, e)
	}

	// 5. Case 2: Destination domain already has the chunk: CopyObject does NOT enqueue a restore job
	warmRef := s.PackRef{ID: "dest-warm-pack", Generation: "generation-1"}
	warmBuilder, _ := pack.NewBuilder(warmRef)
	destKey := s.ChunkKey{Domain: "other-domain", Hash: chunk.Key.Hash}
	_ = warmBuilder.Add(pack.Blob{Key: destKey, RawLength: 4, Codec: "raw", Stored: []byte("data")})
	warmData, warmIndex, warmEntries, _ := warmBuilder.Finish()
	warmInfo := s.PackInfo{Version: s.ModelVersion, Ref: warmRef, Size: int64(len(warmData)), State: "Warm", Class: s.Standard, StateRevision: 1}
	if e = physical.Packs.PutPack(ctx, warmInfo, bytes.NewReader(warmData)); e != nil {
		t.Fatal(e)
	}
	if e = physical.Packs.PutSidecar(ctx, warmRef, int64(len(warmIndex)), bytes.NewReader(warmIndex)); e != nil {
		t.Fatal(e)
	}
	destChunk := s.Chunk{Version: 1, Key: destKey, RawBytes: 4, Location: warmEntries[0].Location(warmRef), LocationRevision: 1}
	if e = m.update(ctx, func(tx *transaction) error {
		if e := put(tx, packKey(warmRef), warmInfo); e != nil {
			return e
		}
		return put(tx, chunkKey(destKey), destChunk)
	}); e != nil {
		t.Fatal(e)
	}

	jobsBeforeSecond, e := m.ListJobs(ctx, jobs.PageRequest{Limit: 100, ActiveOnly: true})
	if e != nil {
		t.Fatal(e)
	}

	outSuccess, err := svc.CopyObject(ctx, object.CopyRequest{
		Source:    s.Bucket{Name: srcHead.Object.Bucket, ID: srcHead.Object.BucketID, Domain: srcHead.Domain},
		SourceKey: srcHead.Object.Key,
		Bucket:    b2, Key: "dst-has-chunk",
		UserID: b2.OwnerID, CredentialID: "owner-key",
	})
	if err != nil {
		t.Fatalf("copy with existing destination chunks failed: %v", err)
	}
	if outSuccess.ETag != srcHead.ETag {
		t.Fatalf("ETag mismatch: got %q want %q", outSuccess.ETag, srcHead.ETag)
	}

	jobsAfterSecond, e := m.ListJobs(ctx, jobs.PageRequest{Limit: 100, ActiveOnly: true})
	if e != nil || len(jobsAfterSecond.Jobs) != len(jobsBeforeSecond.Jobs) {
		t.Fatalf("expected no new jobs when chunks already exist in destination, before=%d after=%d", len(jobsBeforeSecond.Jobs), len(jobsAfterSecond.Jobs))
	}
}
