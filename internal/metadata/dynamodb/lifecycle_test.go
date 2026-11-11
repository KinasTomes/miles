package dynamodb

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/engine"
	"github.com/KinasTomes/miles/internal/jobs"
	opstest "github.com/KinasTomes/miles/internal/jobs/contracttest"
	"github.com/KinasTomes/miles/internal/lifecycle"
	"github.com/KinasTomes/miles/internal/pack"
	"github.com/KinasTomes/miles/internal/recovery/format"
	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/KinasTomes/miles/internal/storage/fake"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"io"
	"math/big"
	"strings"
	"testing"
	"time"
)

type lifePhysical struct {
	losePut bool
	*fake.Packs
	before func()
	calls  int
	lose   bool
}

func (p *lifePhysical) PutPack(ctx context.Context, info s.PackInfo, r io.Reader) error {
	if e := s.CheckPhysicalDispatch(ctx); e != nil {
		return e
	}
	e := p.Packs.PutPack(ctx, info, r)
	if e == nil && p.losePut {
		p.losePut = false
		return s.ErrUnavailable
	}
	return e
}
func (p *lifePhysical) ChangeStorageClass(ctx context.Context, r s.PackRef, c s.StorageClass) error {
	if p.before != nil {
		p.before()
	}
	if e := s.CheckPhysicalDispatch(ctx); e != nil {
		return e
	}
	p.calls++
	e := p.Packs.ChangeStorageClass(ctx, r, c)
	if e == nil && p.lose {
		return s.ErrUnavailable
	}
	return e
}
func (p *lifePhysical) Restore(ctx context.Context, r s.PackRef, q s.RestoreRequest) error {
	if p.before != nil {
		p.before()
	}
	if e := s.CheckPhysicalDispatch(ctx); e != nil {
		return e
	}
	p.calls++
	e := p.Packs.Restore(ctx, r, q)
	if e == nil && p.lose {
		return s.ErrUnavailable
	}
	return e
}
func lifeFixture(t *testing.T, kind jobs.Type, class s.StorageClass) (*Store, *lifecycle.Worker, jobs.Job, config.GlobalSnapshot, *r1Clock, *lifePhysical, s.Chunk) {
	t.Helper()
	ctx := context.Background()
	m, _, f, utc := sharedFixture(t)
	v, _ := config.NewPolicyValidator(config.DSGPolicyLimits())
	policy := config.DefaultGlobalPolicy()
	policy.Tiering.ColdAfterDays = 0
	_ = json.Unmarshal([]byte(`0`), &policy.Tiering.MinPackLiveRatio)
	policy.Tiering.ColdStorageClass = class
	if class == s.Standard {
		policy.Tiering.ColdStorageClass = s.GlacierIR
	}
	snapshot, e := v.NewGlobalSnapshot(1, policy)
	if e != nil {
		t.Fatal(e)
	}
	record, _ := snapshot.StorageSnapshot()
	if e = m.update(ctx, func(tx *transaction) error { return put(tx, policyKey(1), record) }); e != nil {
		t.Fatal(e)
	}
	node := opstest.Node("lifecycle-node")
	node.ServiceIncarnation = "shared-service"
	node.HeartbeatAt = f.Now
	if _, e = m.PutNode(ctx, 0, node); e != nil {
		t.Fatal(e)
	}
	key, e := engine.HashChunk("default", []byte("data"))
	if e != nil {
		t.Fatal(e)
	}
	ref := s.PackRef{ID: "lifecycle-pack", Generation: "generation-1"}
	builder, _ := pack.NewBuilder(ref)
	if e = builder.Add(pack.Blob{Key: key, RawLength: 4, Codec: "raw", Stored: []byte("data")}); e != nil {
		t.Fatal(e)
	}
	if kind == "compaction" {
		deadKey, e := engine.HashChunk("default", []byte("dead"))
		if e != nil {
			t.Fatal(e)
		}
		if e = builder.Add(pack.Blob{Key: deadKey, RawLength: 4, Codec: "raw", Stored: []byte("dead")}); e != nil {
			t.Fatal(e)
		}
	}
	body, index, entries, e := builder.Finish()
	if e != nil {
		t.Fatal(e)
	}
	physical := &lifePhysical{Packs: fake.NewPacks(utc.Now)}
	info := s.PackInfo{Version: 1, Ref: ref, State: "Warm", StateRevision: 1, Class: s.Standard, Size: int64(len(body)), ProtectedUntil: f.Now.Add(-time.Hour), LastAccess: f.Now.Add(-40 * 24 * time.Hour)}
	if e = physical.Packs.PutPack(ctx, info, bytes.NewReader(body)); e != nil {
		t.Fatal(e)
	}
	if e = physical.Packs.PutSidecar(ctx, ref, int64(len(index)), bytes.NewReader(index)); e != nil {
		t.Fatal(e)
	}
	if kind != "tiering" && class != s.Standard {
		if e = physical.Packs.ChangeStorageClass(ctx, ref, class); e != nil {
			t.Fatal(e)
		}
		info.State = "Cold"
		info.Class = class
	}
	chunk := s.Chunk{Version: 1, Key: key, RawBytes: 4, Location: entries[0].Location(ref), LocationRevision: 1}
	params := lifecycle.Params{PackID: ref.ID, Generation: ref.Generation}
	if kind == "pack_restore" {
		params.Days = 1
		params.Tier = "Standard"
	}
	raw, _ := json.Marshal(params)
	svc := jobs.Service{Store: m, Policies: m, Now: utc.Now}
	j, e := svc.Create(ctx, jobs.Create{Type: kind, Params: raw, PolicyRevision: 1})
	if e != nil {
		t.Fatal(e)
	}
	sum := sha256.Sum256([]byte(j.ID))
	op := hex.EncodeToString(sum[:16])
	fence := s.WriterFence{LeaseID: j.ID, Epoch: 1, ServiceIncarnation: "shared-service"}
	if _, e = m.AcquireR1(ctx, s.R1Acquire{Fence: fence, Owner: "lifecycle-worker", Kind: "control", OpID: op, Deadline: f.Now.Add(5 * time.Minute)}); e != nil {
		t.Fatal(e)
	}
	j, e = m.ChangeJob(ctx, jobs.Change{JobID: j.ID, ExpectedRevision: j.Revision, Kind: "claim", Fence: fence, NodeID: node.ID, NodeIncarnation: node.Incarnation})
	if e != nil {
		t.Fatal(e)
	}
	j, e = m.ChangeJob(ctx, jobs.Change{JobID: j.ID, ExpectedRevision: j.Revision, Kind: "begin_step", Fence: fence, NodeID: node.ID, NodeIncarnation: node.Incarnation, StepID: strings.Repeat("1", 32)})
	if e != nil {
		t.Fatal(e)
	}
	head := f.Request("lifecycle-head", 0).Head
	head.ManifestID = strings.Repeat("c", 32)
	head.Revision = 1
	head.Size = 4
	head.PageCount = 1
	head.PageIndex = []uint64{0}
	head.ChunkCount = 1
	if e = m.WriteManifestPages(ctx, []s.ManifestPage{{Version: 1, ManifestID: head.ManifestID, Domain: "default", Chunks: []s.ChunkRef{{Key: key, RawBytes: 4}}}}, fence); e != nil {
		t.Fatal(e)
	}
	if e = m.update(ctx, func(tx *transaction) error {
		if e := put(tx, packKey(ref), info); e != nil {
			return e
		}
		if e := put(tx, chunkKey(key), chunk); e != nil {
			return e
		}
		if kind == "compaction" {
			dead := s.Chunk{Version: 1, Key: entries[1].Key, RawBytes: 4, Location: entries[1].Location(ref), LocationRevision: 1}
			if e := put(tx, chunkKey(dead.Key), dead); e != nil {
				return e
			}
		}
		return put(tx, headKey(head.Object), head)
	}); e != nil {
		t.Fatal(e)
	}
	catalog := &lifecycle.Catalog{Store: m, Now: utc.Now, MaxRecords: 1000, MaxReferences: 1000}
	return m, &lifecycle.Worker{Store: m, Packs: physical, Catalog: catalog, UTC: utc}, j, snapshot, utc, physical, chunk
}
func TestLocalLifecycleTieringRestoreAndPromotion(t *testing.T) {
	for _, target := range []s.StorageClass{s.GlacierIR, s.Glacier} {
		t.Run(string(target), func(t *testing.T) {
			m, w, j, policy, _, physical, _ := lifeFixture(t, "tiering", target)
			out, e := w.Step(t.Context(), j, policy)
			if e != nil || !out.Settled || !out.Done {
				t.Fatal(out, e)
			}
			a, e := m.GetLifecycle(t.Context(), j.ID)
			if e != nil || a.State != "Done" {
				t.Fatal(a, e)
			}
			packs, e := m.GetPacks(t.Context(), []s.PackRef{a.Source.Ref})
			p := packs[a.Source.Ref]
			if e != nil || p.Class != target || p.State != "Cold" || p.MinRetentionUntil.IsZero() || physical.calls != 1 {
				t.Fatal(p, e, physical.calls)
			}
			g, _ := m.GetR1Gate(t.Context())
			if g.State != "Open" {
				t.Fatal(g)
			}
		})
	}
	t.Run("restore acceptance then expiry proof", func(t *testing.T) {
		m, w, j, policy, utc, physical, _ := lifeFixture(t, "pack_restore", s.Glacier)
		out, e := w.Step(t.Context(), j, policy)
		if e != nil || !out.Settled || out.Done {
			t.Fatal(out, e)
		}
		a, _ := m.GetLifecycle(t.Context(), j.ID)
		if a.State != "Waiting" {
			t.Fatal(a)
		}
		if e = physical.CompleteRestore(a.Source.Ref, utc.Now().Add(24*time.Hour)); e != nil {
			t.Fatal(e)
		}
		out, e = w.Step(t.Context(), j, policy)
		if e != nil || !out.Done || physical.calls != 1 {
			t.Fatal(out, e, physical.calls)
		}
		packs, _ := m.GetPacks(t.Context(), []s.PackRef{a.Source.Ref})
		if !lifecycle.Readable(packs[a.Source.Ref], utc.Now()) {
			t.Fatal(packs)
		}
	})
	t.Run("IR is readable and does not restore", func(t *testing.T) {
		_, w, j, policy, _, physical, _ := lifeFixture(t, "pack_restore", s.GlacierIR)
		out, e := w.Step(t.Context(), j, policy)
		if e != nil || !out.Done || physical.calls != 0 {
			t.Fatal(out, e, physical.calls)
		}
	})
	t.Run("promotion publishes new warm generation", func(t *testing.T) {
		m, w, j, policy, _, _, chunk := lifeFixture(t, "pack_promote", s.GlacierIR)
		out, e := w.Step(t.Context(), j, policy)
		if e != nil || !out.Done {
			t.Fatal(out, e)
		}
		a, e := m.GetLifecycle(t.Context(), j.ID)
		if e != nil || a.State != "Done" {
			t.Fatal(a, e)
		}
		rows, _ := m.GetChunks(t.Context(), []s.ChunkKey{chunk.Key}, true)
		if rows[chunk.Key].Location.Pack != a.Destination {
			t.Fatal(rows)
		}
		packs, _ := m.GetPacks(t.Context(), []s.PackRef{chunk.Location.Pack, a.Destination})
		if packs[chunk.Location.Pack].State != "Cold" || packs[a.Destination].State != "Warm" {
			t.Fatal(packs)
		}
	})
}
func TestLocalLifecycleUnknownAndStaleDispatch(t *testing.T) {
	t.Run("mutable lost ACK stays closed and is not redispatched", func(t *testing.T) {
		m, w, j, policy, _, physical, _ := lifeFixture(t, "tiering", s.GlacierIR)
		physical.lose = true
		out, e := w.Step(t.Context(), j, policy)
		if e == nil || out.Settled {
			t.Fatal(out, e)
		}
		out, e = w.Step(t.Context(), j, policy)
		if !errors.Is(e, jobs.ErrReconciliation) || out.Settled || physical.calls != 1 {
			t.Fatal(out, e, physical.calls)
		}
		a, _ := m.GetLifecycle(t.Context(), j.ID)
		p, _ := physical.HeadPack(t.Context(), a.Source.Ref)
		if _, e = m.FinishLifecycle(t.Context(), j, p, false); !errors.Is(e, s.ErrConflict) {
			t.Fatal("HEAD alone released unknown", e)
		}
		g, _ := m.GetR1Gate(t.Context())
		if g.State != "Maintenance" {
			t.Fatal(g)
		}
	})
	for _, mode := range []string{"expiry", "drain", "cancel", "recovery", "stale_node", "closed_fence"} {
		t.Run(mode, func(t *testing.T) {
			m, w, j, policy, utc, physical, _ := lifeFixture(t, "tiering", s.GlacierIR)
			physical.before = func() {
				if mode == "stale_node" {
					utc.Advance(15 * time.Second)
					if _, e := m.RenewR1(t.Context(), j.Worker); e != nil {
						t.Fatal(e)
					}
					utc.Advance(16 * time.Second)
					return
				}
				if mode == "closed_fence" {
					if e := m.CloseR1(t.Context(), j.Worker); e != nil {
						t.Fatal(e)
					}
					return
				}
				if mode == "expiry" {
					utc.Advance(31 * time.Second)
					return
				}
				if e := m.update(t.Context(), func(tx *transaction) error {
					if mode == "recovery" {
						g, e := r1Read[s.R1Gate](tx, r1GateKey())
						if e != nil {
							return e
						}
						g.State = "Closing"
						g.Revision++
						return r1Put(tx, r1GateKey(), g)
					}
					if mode == "drain" {
						n, e := get[jobs.Node](tx, key("nodes", j.WorkerNodeID))
						if e != nil {
							return e
						}
						n.Draining = true
						n.Ready = false
						n.Revision++
						return put(tx, key("nodes", n.ID), n)
					}
					old, e := get[jobs.Job](tx, key("jobs", j.ID))
					if e != nil {
						return e
					}
					old.State = jobs.Cancelling
					old.Revision++
					return put(tx, key("jobs", j.ID), old)
				}); e != nil {
					t.Fatal(e)
				}
			}
			out, e := w.Step(t.Context(), j, policy)
			if e == nil || out.Settled || physical.calls != 0 {
				t.Fatal("late physical dispatch", out, e, physical.calls)
			}
		})
	}
}

type lifeTerminalProof struct {
	ID                          string
	Fence                       s.WriterFence
	Target                      s.StorageClass
	Terminal, Isolated, Applied bool
}
type lifeTerminalGuard struct{}

func (lifeTerminalGuard) Terminal(_ context.Context, a lifecycle.Action, observe Observe) (bool, error) {
	proof, e := r1Observed[lifeTerminalProof](observe, stringKey("SystemConfig", "config_id", "fixture/lifecycle/terminal"))
	if e != nil {
		return false, e
	}
	if proof.ID != a.ID || proof.Fence != a.Fence || proof.Target != a.Target || !proof.Terminal || !proof.Isolated {
		return false, s.ErrR1Busy
	}
	return proof.Applied, nil
}
func TestLocalLifecycleTrustedReconcileAndImmutableRetry(t *testing.T) {
	t.Run("transition needs exact durable terminal and isolation proof", func(t *testing.T) {
		m, w, j, policy, _, physical, _ := lifeFixture(t, "tiering", s.GlacierIR)
		physical.lose = true
		if out, e := w.Step(t.Context(), j, policy); e == nil || out.Settled {
			t.Fatal(out, e)
		}
		m.authority.(*SharedAuthority).LifecycleTerminal = lifeTerminalGuard{}
		if out, e := w.Step(t.Context(), j, policy); e == nil || out.Settled {
			t.Fatal("missing proof accepted", out, e)
		}
		proof := lifeTerminalProof{ID: j.ID, Fence: j.Worker, Target: s.GlacierIR, Terminal: true, Applied: true}
		putProof := func() {
			t.Helper()
			if e := m.update(t.Context(), func(tx *transaction) error {
				return r1Put(tx, stringKey("SystemConfig", "config_id", "fixture/lifecycle/terminal"), proof)
			}); e != nil {
				t.Fatal(e)
			}
		}
		putProof()
		if out, e := w.Step(t.Context(), j, policy); e == nil || out.Settled {
			t.Fatal("terminal without isolation accepted", out, e)
		}
		proof.Isolated = true
		putProof()
		out, e := w.Step(t.Context(), j, policy)
		if e != nil || !out.Done || !out.Settled || physical.calls != 1 {
			t.Fatal(out, e, physical.calls)
		}
	})
	t.Run("lost immutable PUT ACK resumes exact plan without deleting source", func(t *testing.T) {
		m, w, j, policy, _, physical, chunk := lifeFixture(t, "pack_promote", s.GlacierIR)
		physical.losePut = true
		if out, e := w.Step(t.Context(), j, policy); e == nil || out.Settled {
			t.Fatal(out, e)
		}
		out, e := w.Step(t.Context(), j, policy)
		if e != nil || !out.Done || !out.Settled {
			t.Fatal(out, e)
		}
		a, _ := m.GetLifecycle(t.Context(), j.ID)
		rows, _ := m.GetChunks(t.Context(), []s.ChunkKey{chunk.Key}, true)
		if rows[chunk.Key].Location.Pack != a.Destination {
			t.Fatal(rows)
		}
		if _, e = physical.HeadPack(t.Context(), chunk.Location.Pack); e != nil {
			t.Fatal("old generation was removed", e)
		}
	})
	t.Run("unknown restore is not ready until positive backend expiry", func(t *testing.T) {
		m, w, j, policy, utc, physical, _ := lifeFixture(t, "pack_restore", s.Glacier)
		physical.lose = true
		if out, e := w.Step(t.Context(), j, policy); e == nil || out.Settled {
			t.Fatal(out, e)
		}
		// Unknown must go through the trusted terminal hook. A restore already
		// accepted by S3 is terminal even while its asynchronous retrieval runs;
		// the mutable HEAD result alone is not the proof.
		m.authority.(*SharedAuthority).LifecycleTerminal = lifeTerminalGuard{}
		a, e := m.GetLifecycle(t.Context(), j.ID)
		if e != nil || a.State != "Unknown" {
			t.Fatal(a, e)
		}
		proof := lifeTerminalProof{ID: a.ID, Fence: a.Fence, Target: a.Target, Terminal: true, Isolated: true, Applied: true}
		if e = m.update(t.Context(), func(tx *transaction) error {
			return r1Put(tx, stringKey("SystemConfig", "config_id", "fixture/lifecycle/terminal"), proof)
		}); e != nil {
			t.Fatal(e)
		}
		out, e := w.Step(t.Context(), j, policy)
		if e != nil || !out.Settled || out.Done {
			t.Fatal(out, e)
		}
		a, _ = m.GetLifecycle(t.Context(), j.ID)
		if e = physical.CompleteRestore(a.Source.Ref, utc.Now().Add(time.Hour)); e != nil {
			t.Fatal(e)
		}
		out, e = w.Step(t.Context(), j, policy)
		if e != nil || !out.Done || physical.calls != 1 {
			t.Fatal(out, e, physical.calls)
		}
	})
}

func TestLocalLifecycleTerminalGuardBindsOriginalFenceAndPendingStep(t *testing.T) {
	m, w, j, policy, _, physical, _ := lifeFixture(t, "tiering", s.GlacierIR)
	physical.lose = true
	if out, err := w.Step(t.Context(), j, policy); err == nil || out.Settled {
		t.Fatal(out, err)
	}
	a, err := m.GetLifecycle(t.Context(), j.ID)
	if err != nil || a.State != "Unknown" {
		t.Fatal(a, err)
	}
	observe := func() error {
		return m.view(t.Context(), func(tx *transaction) error { return VerifyLifecycleIsolation(a, r1Observe(tx)) })
	}
	if err = observe(); err != nil {
		t.Fatal("exact active action was not bound to its pending step", err)
	}
	if err = m.update(t.Context(), func(tx *transaction) error {
		lease, e := r1Read[s.R1Lease](tx, r1LeaseKey(j.ID))
		if e != nil {
			return e
		}
		lease.Pending = nil
		lease.Revision++
		return r1Put(tx, r1LeaseKey(j.ID), lease)
	}); err != nil {
		t.Fatal(err)
	}
	if err = observe(); !errors.Is(err, s.ErrR1Busy) {
		t.Fatal("missing action protection was accepted", err)
	}
	if err = m.update(t.Context(), func(tx *transaction) error {
		lease, e := r1Read[s.R1Lease](tx, r1LeaseKey(j.ID))
		if e != nil {
			return e
		}
		lease.Pending = []string{"lifecycle/" + j.ID}
		lease.Revision++
		if e = r1Put(tx, r1LeaseKey(j.ID), lease); e != nil {
			return e
		}
		job, e := get[jobs.Job](tx, key("jobs", j.ID))
		if e != nil {
			return e
		}
		job.PendingStep = ""
		job.Revision++
		return put(tx, key("jobs", j.ID), job)
	}); err != nil {
		t.Fatal(err)
	}
	if err = observe(); !errors.Is(err, s.ErrFenced) {
		t.Fatal("a different job step inherited the action proof", err)
	}
}

type lifeAckAPI struct {
	API
	state     string
	attribute string
	before    bool
	fired     bool
}

func (a *lifeAckAPI) TransactWriteItems(ctx context.Context, in *ddb.TransactWriteItemsInput, opts ...func(*ddb.Options)) (*ddb.TransactWriteItemsOutput, error) {
	match := false
	for _, item := range in.TransactItems {
		if item.Put != nil {
			if a.attribute != "" && item.Put.Item[a.attribute] != nil {
				match = true
			}
			if state, ok := item.Put.Item["state"].(*types.AttributeValueMemberS); ok && state.Value == a.state {
				match = true
			}
		}
	}
	if match && !a.fired {
		a.fired = true
		if a.before {
			return nil, s.ErrUnavailable
		}
		out, e := a.API.TransactWriteItems(ctx, in, opts...)
		if e != nil {
			return out, e
		}
		return nil, s.ErrUnavailable
	}
	return a.API.TransactWriteItems(ctx, in, opts...)
}
func TestLocalLifecycleOneShotMetadataACKs(t *testing.T) {
	for _, mode := range []string{"claim_ack", "unknown_marker"} {
		t.Run(mode, func(t *testing.T) {
			m, w, j, policy, _, physical, _ := lifeFixture(t, "tiering", s.GlacierIR)
			seam := &lifeAckAPI{API: m.api, state: "Dispatched"}
			if mode == "unknown_marker" {
				seam.state = "Unknown"
				seam.before = true
				physical.lose = true
			}
			m.api = seam
			out, e := w.Step(t.Context(), j, policy)
			if e == nil || out.Settled {
				t.Fatal(out, e)
			}
			calls := physical.calls
			out, e = w.Step(t.Context(), j, policy)
			if !errors.Is(e, jobs.ErrReconciliation) || out.Settled || physical.calls != calls {
				t.Fatal("lost metadata ACK granted another mutable send", out, e, physical.calls, calls)
			}
			if mode == "claim_ack" && calls != 0 {
				t.Fatal("claim ACK was not known successful", calls)
			}
		})
	}
	t.Run("proven terminal no-effect rolls back without success claim", func(t *testing.T) {
		m, w, j, policy, _, physical, _ := lifeFixture(t, "tiering", s.GlacierIR)
		m.api = &lifeAckAPI{API: m.api, state: "Dispatched"}
		if out, e := w.Step(t.Context(), j, policy); e == nil || out.Settled {
			t.Fatal(out, e)
		}
		if _, e := w.Step(t.Context(), j, policy); !errors.Is(e, jobs.ErrReconciliation) {
			t.Fatal(e)
		}
		m.authority.(*SharedAuthority).LifecycleTerminal = lifeTerminalGuard{}
		proof := lifeTerminalProof{ID: j.ID, Fence: j.Worker, Target: s.GlacierIR, Terminal: true, Isolated: true, Applied: false}
		if e := m.update(t.Context(), func(tx *transaction) error {
			return r1Put(tx, stringKey("SystemConfig", "config_id", "fixture/lifecycle/terminal"), proof)
		}); e != nil {
			t.Fatal(e)
		}
		out, e := w.Step(t.Context(), j, policy)
		if e != nil || !out.Settled || !out.Done || out.ErrorCode != "transition_not_applied" || physical.calls != 0 {
			t.Fatal(out, e, physical.calls)
		}
		g, _ := m.GetR1Gate(t.Context())
		if g.State != "Open" {
			t.Fatal(g)
		}
	})
}
func TestLocalLifecyclePagedPromotionPlan(t *testing.T) {
	m, w, j, policy, _, physical, _ := lifeFixture(t, "pack_promote", s.GlacierIR)
	ctx := t.Context()
	ref := s.PackRef{ID: "lifecycle-pack", Generation: "multi-page"}
	builder, _ := pack.NewBuilder(ref)
	refs := []s.ChunkRef{}
	offset := int64(0)
	compressor, e := engine.NewCompressor(engine.CompressionPolicy{Enabled: true, Codec: "zstd", Level: 3, MinSavingRatio: big.NewRat(1, 10)})
	if e != nil {
		t.Fatal(e)
	}
	defer compressor.Close()
	for n := range 80 {
		raw := []byte(fmt.Sprintf("chunk-%03d", n) + strings.Repeat("compress-me", 500))
		key, e := engine.HashChunk("default", raw)
		if e != nil {
			t.Fatal(e)
		}
		encoded, e := compressor.Encode(engine.Chunk{Key: key, Bytes: raw})
		if e != nil {
			t.Fatal(e)
		}
		if e = builder.Add(pack.Blob{Key: key, RawLength: uint32(len(raw)), Codec: encoded.Codec, Stored: encoded.Bytes}); e != nil {
			t.Fatal(e)
		}
		refs = append(refs, s.ChunkRef{Key: key, RawBytes: int64(len(raw)), LogicalOffset: offset})
		offset += int64(len(raw))
	}
	body, index, entries, e := builder.Finish()
	if e != nil {
		t.Fatal(e)
	}
	info := s.PackInfo{Version: 1, Ref: ref, State: "Warm", Class: s.Standard, StateRevision: 1, Size: int64(len(body))}
	if e = physical.Packs.PutPack(ctx, info, bytes.NewReader(body)); e != nil {
		t.Fatal(e)
	}
	if e = physical.Packs.PutSidecar(ctx, ref, int64(len(index)), bytes.NewReader(index)); e != nil {
		t.Fatal(e)
	}
	mid := strings.Repeat("d", 32)
	if e = m.WriteManifestPages(ctx, []s.ManifestPage{{Version: 1, ManifestID: mid, Domain: "default", Chunks: refs}}, j.Worker); e != nil {
		t.Fatal(e)
	}
	params := lifecycle.Params{PackID: ref.ID, Generation: ref.Generation}
	j.Params, _ = json.Marshal(params)
	if e = m.update(ctx, func(tx *transaction) error {
		if e := put(tx, packKey(ref), info); e != nil {
			return e
		}
		if e := put(tx, key("jobs", j.ID), j); e != nil {
			return e
		}
		for _, entry := range entries {
			c := s.Chunk{Version: 1, Key: entry.Key, RawBytes: int64(entry.RawLength), Location: entry.Location(ref), LocationRevision: 1}
			if e := put(tx, chunkKey(c.Key), c); e != nil {
				return e
			}
		}
		// Replace the fixture's one live head, retaining only the new complete plan.
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	// Observe the actual fixture head key instead of guessing bucket identities.
	page, e := m.ObservePage(ctx, s.ObservationRequest{Kind: "heads", Limit: 100})
	if e != nil || len(page.Heads) != 1 {
		t.Fatal(page, e)
	}
	head := page.Heads[0]
	head.ManifestID = mid
	head.Size = offset
	head.ChunkCount = 80
	if e = m.update(ctx, func(tx *transaction) error { return put(tx, headKey(head.Object), head) }); e != nil {
		t.Fatal(e)
	}
	out, e := w.Step(ctx, j, policy)
	if e != nil || !out.Done {
		t.Fatal(out, e)
	}
	a, e := m.GetLifecycle(ctx, j.ID)
	if e != nil || a.PlanPages != 3 || len(a.Chunks) != 80 {
		t.Fatal(a.PlanPages, len(a.Chunks), e)
	}
	for _, chunk := range a.Chunks {
		rows, e := m.GetChunks(ctx, []s.ChunkKey{chunk.Key}, true)
		if e != nil || rows[chunk.Key].Location.Pack != a.Destination || rows[chunk.Key].Location.Codec != "zstd" {
			t.Fatal("plan was truncated", e)
		}
	}
}

func TestLocalLifecycleCancelledAcceptedRestoreCanResume(t *testing.T) {
	m, w, j, policy, utc, physical, _ := lifeFixture(t, "pack_restore", s.Glacier)
	ctx := t.Context()
	out, e := w.Step(ctx, j, policy)
	if e != nil || !out.Settled || out.Done {
		t.Fatal(out, e)
	}
	a, _ := m.GetLifecycle(ctx, j.ID)
	j, e = m.ChangeJob(ctx, jobs.Change{JobID: j.ID, ExpectedRevision: j.Revision, Kind: "cancel"})
	if e != nil {
		t.Fatal(e)
	}
	out, e = w.Step(ctx, j, policy)
	if e != nil || !out.Settled {
		t.Fatal(out, e)
	}
	j, e = m.ChangeJob(ctx, jobs.Change{JobID: j.ID, ExpectedRevision: j.Revision, Kind: "checkpoint", Fence: j.Worker, NodeID: j.WorkerNodeID, NodeIncarnation: j.WorkerNodeIncarnation, StepID: j.PendingStep, Checkpoint: json.RawMessage(`{}`)})
	if e != nil || j.State != jobs.Cancelled {
		t.Fatal(j, e)
	}
	if e = m.CloseR1(ctx, j.Worker); e != nil {
		t.Fatal(e)
	}
	if e = m.ReleaseR1(ctx, j.Worker); e != nil {
		t.Fatal(e)
	}
	svc := jobs.Service{Store: m, Policies: m, Now: utc.Now}
	next, e := svc.Create(ctx, jobs.Create{Type: j.Type, Params: j.Params, PolicyRevision: 1})
	if e != nil {
		t.Fatal(e)
	}
	sum := sha256.Sum256([]byte(next.ID))
	f := s.WriterFence{LeaseID: next.ID, Epoch: 1, ServiceIncarnation: "shared-service"}
	if _, e = m.AcquireR1(ctx, s.R1Acquire{Fence: f, Owner: "resumer", Kind: "control", OpID: hex.EncodeToString(sum[:16]), Deadline: utc.Now().Add(5 * time.Minute)}); e != nil {
		t.Fatal(e)
	}
	next, e = m.ChangeJob(ctx, jobs.Change{JobID: next.ID, ExpectedRevision: next.Revision, Kind: "claim", Fence: f, NodeID: j.WorkerNodeID, NodeIncarnation: j.WorkerNodeIncarnation})
	if e != nil {
		t.Fatal(e)
	}
	next, e = m.ChangeJob(ctx, jobs.Change{JobID: next.ID, ExpectedRevision: next.Revision, Kind: "begin_step", Fence: f, NodeID: j.WorkerNodeID, NodeIncarnation: j.WorkerNodeIncarnation, StepID: strings.Repeat("2", 32)})
	if e != nil {
		t.Fatal(e)
	}
	if e = physical.CompleteRestore(a.Source.Ref, utc.Now().Add(time.Hour)); e != nil {
		t.Fatal(e)
	}
	out, e = w.Step(ctx, next, policy)
	if e != nil || !out.Done || physical.calls != 1 {
		t.Fatal("accepted restore was reissued or stranded", out, e, physical.calls)
	}
}

func TestLocalLifecycleProtectionAndOwnedMaintenance(t *testing.T) {
	t.Run("retained generations defer with no state or physical change", func(t *testing.T) {
		m, w, j, policy, utc, physical, _ := lifeFixture(t, "tiering", s.GlacierIR)
		if e := m.update(t.Context(), func(tx *transaction) error {
			p, e := get[s.PackInfo](tx, packKey(s.PackRef{ID: "lifecycle-pack", Generation: "generation-1"}))
			if e != nil {
				return e
			}
			p.ProtectedUntil = utc.Now().Add(time.Hour)
			return put(tx, packKey(p.Ref), p)
		}); e != nil {
			t.Fatal(e)
		}
		out, e := w.Step(t.Context(), j, policy)
		if e != nil || !out.Settled || out.Done || physical.calls != 0 {
			t.Fatal(out, e, physical.calls)
		}
		g, _ := m.GetR1Gate(t.Context())
		if g.State != "Open" {
			t.Fatal(g)
		}
	})
	t.Run("own maintenance can settle while readiness is suppressed", func(t *testing.T) {
		m, w, j, policy, _, physical, _ := lifeFixture(t, "tiering", s.GlacierIR)
		physical.before = func() {
			if e := m.update(t.Context(), func(tx *transaction) error {
				n, e := get[jobs.Node](tx, key("nodes", j.WorkerNodeID))
				if e != nil {
					return e
				}
				n.Ready = false
				n.Revision++
				return put(tx, key("nodes", n.ID), n)
			}); e != nil {
				t.Fatal(e)
			}
		}
		out, e := w.Step(t.Context(), j, policy)
		if e != nil || !out.Done || physical.calls != 1 {
			t.Fatal(out, e, physical.calls)
		}
	})
}

func TestLocalLifecycleTerminalOutcomeAllowsJobTakeover(t *testing.T) {
	m, w, j, policy, utc, physical, _ := lifeFixture(t, "tiering", s.GlacierIR)
	ctx := t.Context()
	out, e := w.Step(ctx, j, policy)
	if e != nil || !out.Done {
		t.Fatal(out, e)
	}
	if e = m.LifecycleQuiescent(ctx, j.ID, j.Worker); e != nil {
		t.Fatal(e)
	}
	// Crash after durable lifecycle outcome, before the Jobs checkpoint ACK.
	lease, e := m.GetR1Lease(ctx, j.ID)
	if e != nil {
		t.Fatal(e)
	}
	if e = m.CloseR1(ctx, j.Worker); e != nil {
		t.Fatal(e)
	}
	if e = m.ReleaseR1(ctx, j.Worker); e != nil {
		t.Fatal(e)
	}
	next := j.Worker
	next.Epoch++
	if _, e = m.AcquireR1(ctx, s.R1Acquire{Fence: next, Owner: "takeover", Kind: "control", OpID: lease.OpID, Deadline: utc.Now().Add(5 * time.Minute)}); e != nil {
		t.Fatal(e)
	}
	j, e = m.ChangeJob(ctx, jobs.Change{JobID: j.ID, ExpectedRevision: j.Revision, Kind: "claim", Fence: next, NodeID: j.WorkerNodeID, NodeIncarnation: j.WorkerNodeIncarnation})
	if e != nil {
		t.Fatal(e)
	}
	j, e = m.ChangeJob(ctx, jobs.Change{JobID: j.ID, ExpectedRevision: j.Revision, Kind: "begin_step", Fence: next, NodeID: j.WorkerNodeID, NodeIncarnation: j.WorkerNodeIncarnation, StepID: strings.Repeat("3", 32)})
	if e != nil {
		t.Fatal(e)
	}
	out, e = w.Step(ctx, j, policy)
	if e != nil || !out.Settled || !out.Done || physical.calls != 1 {
		t.Fatal("retained outcome redispatched", out, e, physical.calls)
	}
}

func TestLocalLifecycleMissingIntentDoesNotProveQuiescence(t *testing.T) {
	m, _, j, _, _, _, _ := lifeFixture(t, "pack_promote", s.GlacierIR)
	if e := m.LifecycleQuiescent(t.Context(), j.ID, j.Worker); !errors.Is(e, jobs.ErrReconciliation) {
		t.Fatal("absence granted expiry cleanup", e)
	}
	lease, e := m.GetR1Lease(t.Context(), j.ID)
	if e != nil || lease.State != "Active" {
		t.Fatal(lease, e)
	}
}

type lifeLateCASAPI struct {
	API
	table  string
	before func()
	fired  bool
}

func (a *lifeLateCASAPI) TransactWriteItems(ctx context.Context, in *ddb.TransactWriteItemsInput, opts ...func(*ddb.Options)) (*ddb.TransactWriteItemsOutput, error) {
	for _, item := range in.TransactItems {
		if !a.fired && item.Put != nil && item.Put.TableName != nil && *item.Put.TableName == a.table {
			a.fired = true
			a.before()
			break
		}
	}
	return a.API.TransactWriteItems(ctx, in, opts...)
}
func TestLocalLifecycleNodeClosureWinsLateCanonicalCAS(t *testing.T) {
	m, w, j, policy, utc, _, chunk := lifeFixture(t, "pack_promote", s.GlacierIR)
	peer, e := New(Options{API: m.api, Tables: m.tables, Authority: m.authority, Now: utc.Now})
	if e != nil {
		t.Fatal(e)
	}
	seam := &lifeLateCASAPI{API: m.api, table: m.tables["Chunks"], before: func() {
		if e := peer.update(t.Context(), func(tx *transaction) error {
			n, e := get[jobs.Node](tx, key("nodes", j.WorkerNodeID))
			if e != nil {
				return e
			}
			n.Draining = true
			n.Ready = false
			n.Revision++
			return put(tx, key("nodes", n.ID), n)
		}); e != nil {
			t.Fatal(e)
		}
	}}
	m.api = seam
	out, e := w.Step(t.Context(), j, policy)
	if e == nil || out.Settled || !seam.fired {
		t.Fatal(out, e, seam.fired)
	}
	rows, e := peer.GetChunks(t.Context(), []s.ChunkKey{chunk.Key}, true)
	if e != nil || rows[chunk.Key] != chunk {
		t.Fatal("drained worker published a late canonical location", rows, e)
	}
}

// A body revival that wins the canonical race after promotion read the row is
// kept; promotion finishes and its destination is only protected garbage.
func TestLocalPromotionLosingRaceKeepsRevivedWinner(t *testing.T) {
	m, w, j, policy, utc, _, chunk := lifeFixture(t, "pack_promote", s.GlacierIR)
	peer, e := New(Options{API: m.api, Tables: m.tables, Authority: m.authority, Now: utc.Now})
	if e != nil {
		t.Fatal(e)
	}
	revived := chunk
	revived.Location.Pack = s.PackRef{ID: "revived-pack", Generation: "generation-r"}
	revived.LocationRevision++
	seam := &lifeLateCASAPI{API: m.api, table: m.tables["Chunks"], before: func() {
		if e := peer.update(t.Context(), func(tx *transaction) error { return put(tx, chunkKey(chunk.Key), revived) }); e != nil {
			t.Fatal(e)
		}
	}}
	m.api = seam
	out, e := w.Step(t.Context(), j, policy)
	if e != nil || !out.Done || !seam.fired {
		t.Fatal(out, e, seam.fired)
	}
	a, e := peer.GetLifecycle(t.Context(), j.ID)
	if e != nil || a.State != "Done" {
		t.Fatal(a, e)
	}
	rows, e := peer.GetChunks(t.Context(), []s.ChunkKey{chunk.Key}, true)
	if e != nil || rows[chunk.Key] != revived {
		t.Fatal("promotion rebased over the revived winner", rows, e)
	}
	packs, e := peer.GetPacks(t.Context(), []s.PackRef{chunk.Location.Pack, a.Destination})
	if e != nil || packs[chunk.Location.Pack].State != "Cold" || packs[a.Destination].State != "Warm" || !packs[a.Destination].ProtectedUntil.After(utc.Now()) {
		t.Fatal("source/destination after lost race", packs, e)
	}
}

func TestLocalWarmCompactionRetainsPinnedSource(t *testing.T) {
	m, w, j, policy, utc, physical, old := lifeFixture(t, "compaction", s.Standard)
	ctx := t.Context()
	w.Marker = &lifecycle.Marker{Store: m, Pinned: m.ObservePackPins, MaxRecords: 1000, MaxReferences: 1000, RecoveryRoots: func(context.Context) ([]s.PackRef, error) { return nil, nil }}
	headPage, e := m.ObservePage(ctx, s.ObservationRequest{Kind: "heads", Limit: 100})
	if e != nil || len(headPage.Heads) != 1 {
		t.Fatal(headPage, e)
	}
	reader := s.WriterFence{LeaseID: "compaction-old-reader", Epoch: 1, ServiceIncarnation: j.Worker.ServiceIncarnation}
	if _, e = m.AcquireR1(ctx, s.R1Acquire{Fence: reader, Owner: "old-reader", Kind: "reader", Object: headPage.Heads[0].Object, Deadline: utc.Now().Add(time.Minute)}); e != nil {
		t.Fatal(e)
	}
	if e = m.BindR1(ctx, reader, headPage.Heads[0]); e != nil {
		t.Fatal(e)
	}
	if e = m.PinR1(ctx, reader, []s.PackRef{old.Location.Pack}); e != nil {
		t.Fatal(e)
	}
	if e = m.ResolvedR1(ctx, reader); e != nil {
		t.Fatal(e)
	}
	step, e := w.Step(ctx, j, policy)
	if e != nil || !step.Done || !step.Settled {
		t.Fatal(step, e)
	}
	rows, e := m.GetChunks(ctx, []s.ChunkKey{old.Key}, true)
	now := rows[old.Key]
	if e != nil || now.LocationRevision != old.LocationRevision+1 || now.Location.Pack == old.Location.Pack {
		t.Fatal(now, e)
	}
	for _, c := range []s.Chunk{old, now} {
		r, e := physical.RangeGet(ctx, c.Location.Pack, c.Location.Range)
		if e != nil {
			t.Fatal(e)
		}
		data, e := io.ReadAll(r)
		closeErr := r.Close()
		if e != nil || closeErr != nil || string(data) != "data" {
			t.Fatal(string(data), e, closeErr)
		}
	}
	index, e := physical.ReadSidecar(ctx, now.Location.Pack)
	if e != nil {
		t.Fatal(e)
	}
	expected, e := pack.NewBuilder(now.Location.Pack)
	if e != nil {
		t.Fatal(e)
	}
	if e = expected.Add(pack.Blob{Key: old.Key, RawLength: 4, Codec: "raw", Stored: []byte("data")}); e != nil {
		t.Fatal(e)
	}
	_, wantIndex, _, e := expected.Finish()
	if e != nil || !bytes.Equal(index, wantIndex) {
		t.Fatal("destination contains dead payload", e)
	}
	if pinned, e := m.ObservePackPins(ctx, old.Location.Pack); e != nil || !pinned {
		t.Fatal(pinned, e)
	}
}

type sweepPhysicalFixture struct {
	calls   []s.SweepVersion
	unknown bool
}

func (p *sweepPhysicalFixture) ListSweepVersions(context.Context, s.PackRef) ([]s.SweepVersion, error) {
	return []s.SweepVersion{{Category: "pack", VersionID: "pack-version"}, {Category: "sidecar", VersionID: "index-version"}}, nil
}
func (p *sweepPhysicalFixture) DeleteSweepVersion(ctx context.Context, _ s.PackRef, v s.SweepVersion) error {
	if e := s.CheckPhysicalDispatch(ctx); e != nil {
		return e
	}
	p.calls = append(p.calls, v)
	if p.unknown && v.Category == "sidecar" {
		return s.ErrUnavailable
	}
	return nil
}

func TestLocalGCSweepDoesNotReuseUnknownDispatch(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		name := "known"
		if unknown {
			name = "unknown_after_pack_ack"
		}
		t.Run(name, func(t *testing.T) {
			m, w, j, policy, utc, _, old := lifeFixture(t, "gc", s.Standard)
			ctx := t.Context()
			// Model a source retired by a prior stable mark, with the live object
			// still referencing its new generation. The retired pack has no winners.
			rows, e := m.GetPacks(ctx, []s.PackRef{old.Location.Pack})
			if e != nil {
				t.Fatal(e)
			}
			p := rows[old.Location.Pack]
			dead := p
			dead.Ref = s.PackRef{ID: "gc-pack", Generation: "retiring"}
			dead.State = "Retiring"
			when := utc.Now().Add(-26 * time.Hour)
			if e = m.update(ctx, func(tx *transaction) error {
				if e := put(tx, packKey(dead.Ref), dead); e != nil {
					return e
				}
				if e := r1Put(tx, sweepRetirementKey(dead.Ref), sweepRetirement{Version: 1, Ref: dead.Ref, At: when}); e != nil {
					return e
				}
				current, e := get[jobs.Job](tx, key("jobs", j.ID))
				if e != nil {
					return e
				}
				current.Params, _ = json.Marshal(lifecycle.Params{PackID: dead.Ref.ID, Generation: dead.Ref.Generation})
				j = current
				return put(tx, key("jobs", j.ID), current)
			}); e != nil {
				t.Fatal(e)
			}
			w.Marker = &lifecycle.Marker{Store: m, Pinned: m.ObservePackPins, MaxRecords: 1000, MaxReferences: 1000, RecoveryRoots: func(context.Context) ([]s.PackRef, error) { return nil, nil }}
			w.VerifyCutAfter = func(_ context.Context, at time.Time) error {
				if !at.Equal(when) {
					return s.ErrInvalid
				}
				return nil
			}
			physical := &sweepPhysicalFixture{unknown: unknown}
			w.Sweep = physical
			step, e := w.Step(ctx, j, policy)
			action, readErr := m.GetLifecycle(ctx, j.ID)
			if readErr != nil {
				t.Fatal(readErr)
			}
			gate, readErr := m.GetR1Gate(ctx)
			if readErr != nil {
				t.Fatal(readErr)
			}
			rows, readErr = m.GetPacks(ctx, []s.PackRef{dead.Ref})
			if readErr != nil {
				t.Fatal(readErr)
			}
			if unknown {
				if e == nil || step.Settled || action.State != "Unknown" || gate.State != "Maintenance" || rows[dead.Ref].State != "Deleting" {
					t.Fatal(step, e, action.State, gate.State, rows[dead.Ref].State)
				}
				if again, e := w.Step(ctx, j, policy); e == nil || again.Settled {
					t.Fatal(again, e)
				}
			} else if e != nil || !step.Done || !step.Settled || action.State != "Done" || gate.State != "Open" || rows[dead.Ref].State != "Retired" {
				t.Fatal(step, e, action.State, gate.State, rows[dead.Ref].State)
			}
			if len(physical.calls) != 2 {
				t.Fatal("dispatched again", physical.calls)
			}
		})
	}
}

func TestLocalGCProtectsLiveGeneration(t *testing.T) {
	m, w, j, policy, _, _, old := lifeFixture(t, "gc", s.Standard)
	ctx := t.Context()
	physical := &sweepPhysicalFixture{}
	w.Sweep = physical
	w.Marker = &lifecycle.Marker{Store: m, Pinned: m.ObservePackPins, MaxRecords: 1000, MaxReferences: 1000, RecoveryRoots: func(context.Context) ([]s.PackRef, error) { return nil, nil }}
	step, e := w.Step(ctx, j, policy)
	if !errors.Is(e, s.ErrR1Busy) || !step.Settled || len(physical.calls) != 0 {
		t.Fatal(step, e, physical.calls)
	}
	rows, e := m.GetPacks(ctx, []s.PackRef{old.Location.Pack})
	gate, gateErr := m.GetR1Gate(ctx)
	if e != nil || gateErr != nil || rows[old.Location.Pack].State != "Warm" || gate.State != "Open" {
		t.Fatal(rows, gate, e, gateErr)
	}
}

func TestLocalGCRetiresLastCanonicalAndAllowsReinsertion(t *testing.T) {
	m, w, j, policy, utc, _, old := lifeFixture(t, "gc", s.Standard)
	ctx := t.Context()
	headPage, e := m.ObservePage(ctx, s.ObservationRequest{Kind: "heads", Limit: 100})
	if e != nil || len(headPage.Heads) != 1 {
		t.Fatal(headPage, e)
	}
	head := headPage.Heads[0]
	tombstone := s.ObjectHead{Version: 1, Revision: head.Revision + 1, Object: head.Object, Domain: head.Domain, OpID: head.OpID, Deleted: true}
	if e = m.update(ctx, func(tx *transaction) error { return put(tx, headKey(head.Object), tombstone) }); e != nil {
		t.Fatal(e)
	}
	w.Marker = &lifecycle.Marker{Store: m, Pinned: m.ObservePackPins, MaxRecords: 1000, MaxReferences: 1000, RecoveryRoots: func(context.Context) ([]s.PackRef, error) { return nil, nil }}
	physical := &sweepPhysicalFixture{}
	w.Sweep = physical
	blocker := s.WriterFence{LeaseID: "before-gc", Epoch: 1, ServiceIncarnation: j.Worker.ServiceIncarnation}
	if _, e = m.AcquireR1(ctx, s.R1Acquire{Fence: blocker, Owner: "writer", Kind: "writer", Object: head.Object, Deadline: utc.Now().Add(time.Minute)}); e != nil {
		t.Fatal(e)
	}
	if step, e := w.Step(ctx, j, policy); e != nil || step.Done {
		t.Fatal("GC did not defer", step, e)
	}
	if e = m.CloseR1(ctx, blocker); e != nil {
		t.Fatal(e)
	}
	if e = m.ReleaseR1(ctx, blocker); e != nil {
		t.Fatal(e)
	}
	fault := &lifeAckAPI{API: m.api, attribute: chunkRetirementAttribute}
	m.api = fault
	if step, e := w.Step(ctx, j, policy); e == nil || step.Settled || !fault.fired {
		t.Fatal("retirement ACK loss", step, e, fault.fired)
	}
	gate, e := m.GetR1Gate(ctx)
	if e != nil || gate.State != "Maintenance" {
		t.Fatal(gate, e)
	}
	step, e := w.Step(ctx, j, policy)
	if e != nil || !step.Done || !step.Settled {
		t.Fatal(step, e)
	}
	rows, e := m.GetChunks(ctx, []s.ChunkKey{old.Key}, true)
	if e != nil || len(rows) != 0 {
		t.Fatal(rows, e)
	}
	packs, e := m.GetPacks(ctx, []s.PackRef{old.Location.Pack})
	if e != nil || packs[old.Location.Pack].State != "Retiring" {
		t.Fatal(packs, e)
	}
	producer, e := m.LifecycleProducer(ctx, j)
	if e != nil {
		t.Fatal(e)
	}
	identity, e := old.Key.PartitionKey()
	if e != nil {
		t.Fatal(e)
	}
	retirement := s.ChildMutation(producer, "Chunks", identity, "location_retire")
	id, e := s.EventID(producer.HistoryID, retirement.MutationID, "Chunks", "location_retire")
	if e != nil {
		t.Fatal(e)
	}
	event, e := read[s.OutboxEvent](m, ctx, key("events", id))
	if e != nil || event.Revision != old.LocationRevision+1 {
		t.Fatal(event, e)
	}
	body, e := m.Open(ctx, event.PayloadPointer)
	if e != nil {
		t.Fatal(e)
	}
	defer body.Close()
	var input format.Input
	if e = json.NewDecoder(body).Decode(&input); e != nil {
		t.Fatal(e)
	}
	found := false
	for _, page := range input.Pages {
		for _, effect := range page.Effects {
			if effect.Key == "Chunks/"+hex.EncodeToString([]byte(identity)) {
				found = effect.Deleted && effect.Revision == old.LocationRevision+1 && effect.Value == ""
			}
		}
	}
	if !found {
		t.Fatal("retirement event has no chunk tombstone", input)
	}
	if _, _, e = m.PutChunkIfAbsent(s.WithMutation(ctx, producer), old, j.Worker); !errors.Is(e, s.ErrConflict) {
		t.Fatal("retired pack accepted as a destination", e)
	}
	nextPack := packs[old.Location.Pack]
	nextPack.Ref.Generation = "new-location"
	nextPack.State = "Warm"
	if e = m.update(ctx, func(tx *transaction) error { return put(tx, packKey(nextPack.Ref), nextPack) }); e != nil {
		t.Fatal(e)
	}
	candidate := old
	candidate.Location.Pack = nextPack.Ref
	got, won, e := m.PutChunkIfAbsent(s.WithMutation(ctx, producer), candidate, j.Worker)
	if e != nil || !won || got.LocationRevision != old.LocationRevision+2 || got.Location.Pack != nextPack.Ref {
		t.Fatal(got, won, e)
	}

	if e = m.CloseR1(ctx, j.Worker); e != nil {
		t.Fatal(e)
	}
	if e = m.ReleaseR1(ctx, j.Worker); e != nil {
		t.Fatal(e)
	}
	utc.Advance(25 * time.Hour)
	node, e := m.GetNode(ctx, j.WorkerNodeID)
	if e != nil {
		t.Fatal(e)
	}
	node.HeartbeatAt = utc.Now()
	if _, e = m.PutNode(ctx, node.Revision, node); e != nil {
		t.Fatal(e)
	}
	svc := jobs.Service{Store: m, Policies: m, Now: utc.Now}
	sweep, e := svc.Create(ctx, jobs.Create{Type: "gc", Params: j.Params, PolicyRevision: 1})
	if e != nil {
		t.Fatal(e)
	}
	fence := s.WriterFence{LeaseID: sweep.ID, Epoch: 1, ServiceIncarnation: j.Worker.ServiceIncarnation}
	sum := sha256.Sum256([]byte(sweep.ID))
	if _, e = m.AcquireR1(ctx, s.R1Acquire{Fence: fence, Owner: "sweep-worker", Kind: "control", OpID: hex.EncodeToString(sum[:16]), Deadline: utc.Now().Add(time.Minute)}); e != nil {
		t.Fatal(e)
	}
	sweep, e = m.ChangeJob(ctx, jobs.Change{JobID: sweep.ID, ExpectedRevision: sweep.Revision, Kind: "claim", Fence: fence, NodeID: node.ID, NodeIncarnation: node.Incarnation})
	if e != nil {
		t.Fatal(e)
	}
	sweep, e = m.ChangeJob(ctx, jobs.Change{JobID: sweep.ID, ExpectedRevision: sweep.Revision, Kind: "begin_step", Fence: fence, NodeID: node.ID, NodeIncarnation: node.Incarnation, StepID: strings.Repeat("2", 32)})
	if e != nil {
		t.Fatal(e)
	}
	checkedCut := false
	w.VerifyCutAfter = func(context.Context, time.Time) error { checkedCut = true; return nil }
	step, e = w.Step(ctx, sweep, policy)
	if e != nil || !step.Done || !step.Settled || !checkedCut || len(physical.calls) != 2 {
		t.Fatal(step, e, checkedCut, physical.calls)
	}
	rows, e = m.GetChunks(ctx, []s.ChunkKey{old.Key}, true)
	if e != nil || rows[old.Key] != got {
		t.Fatal(rows, e)
	}
}
