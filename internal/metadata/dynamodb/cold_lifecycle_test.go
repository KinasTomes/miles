package dynamodb

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/jobs"
	"github.com/KinasTomes/miles/internal/lifecycle"
	s "github.com/KinasTomes/miles/internal/storage"
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
	first, e := m.QueueReadRestore(t.Context(), f, head, chunk.Location.Pack, 1, required)
	if e != nil || first.ID == "" || first.State != jobs.Queued {
		t.Fatal(first, e)
	}
	second, e := m.QueueReadRestore(t.Context(), f, head, chunk.Location.Pack, 1, required.Add(time.Second))
	if e != nil || second.ID != first.ID || second.Revision != 1 {
		t.Fatal(second, e)
	}
	params, e := lifecycle.Decode(first.Params, first.Type)
	if e != nil || params.RequiredUntil.Before(required) || params.Days != 1 || params.Tier != "Standard" {
		t.Fatal(params, e)
	}
	bad := head
	bad.ManifestID = "unbound-root"
	if _, e = m.QueueReadRestore(t.Context(), f, bad, chunk.Location.Pack, 1, required); !errors.Is(e, s.ErrFenced) {
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
	if _, e = m.QueueReadRestore(t.Context(), f, head, unpinned, 1, required); e == nil {
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
	if _, e = m.QueueReadRestore(t.Context(), f, head, chunk.Location.Pack, 1, required); !errors.Is(e, s.ErrFenced) {
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
	if _, e = m.QueueReadRestore(t.Context(), f, head, chunk.Location.Pack, 1, required); e == nil {
		t.Fatal("closed reader queued restore")
	}
}

func TestLocalReadRestoreLostACKReconcilesStableJob(t *testing.T) {
	m, _, _, _, utc, _, chunk := lifeFixture(t, "pack_restore", s.Glacier)
	f, head := restoreReader(t, m, utc, chunk.Location.Pack)
	fault := &lifeAckAPI{API: m.api, attribute: "job_id"}
	m.api = fault
	one, e := m.QueueReadRestore(t.Context(), f, head, chunk.Location.Pack, 1, utc.Now().Add(7*time.Minute))
	if e == nil || !fault.fired || one.ID == "" {
		t.Fatal("missing injected ACK loss", one, e, fault.fired)
	}
	stored, e := m.GetJob(t.Context(), one.ID)
	if e != nil || stored.State != jobs.Queued {
		t.Fatal(stored, e)
	}
	again, e := m.QueueReadRestore(t.Context(), f, head, chunk.Location.Pack, 1, utc.Now().Add(7*time.Minute))
	if e != nil || again.ID != one.ID || again.Revision != 1 {
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
