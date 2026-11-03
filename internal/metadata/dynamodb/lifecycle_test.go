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
		out, e := w.Step(t.Context(), j, policy)
		if e != nil || !out.Settled || out.Done {
			t.Fatal(out, e)
		}
		a, _ := m.GetLifecycle(t.Context(), j.ID)
		if e = physical.CompleteRestore(a.Source.Ref, utc.Now().Add(time.Hour)); e != nil {
			t.Fatal(e)
		}
		out, e = w.Step(t.Context(), j, policy)
		if e != nil || !out.Done || physical.calls != 1 {
			t.Fatal(out, e, physical.calls)
		}
	})
}

type lifeAckAPI struct {
	API
	state  string
	before bool
	fired  bool
}

func (a *lifeAckAPI) TransactWriteItems(ctx context.Context, in *ddb.TransactWriteItemsInput, opts ...func(*ddb.Options)) (*ddb.TransactWriteItemsOutput, error) {
	match := false
	for _, item := range in.TransactItems {
		if item.Put != nil {
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
