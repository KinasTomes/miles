package integration_test

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/config"
	i "github.com/KinasTomes/miles/internal/config/integration"
	"github.com/KinasTomes/miles/internal/policy"
	"github.com/KinasTomes/miles/internal/storage"
	"github.com/KinasTomes/miles/internal/storage/fake"
)

func setup(t *testing.T) (config.PolicyValidator, *fake.Metadata, storage.WriterFence, time.Time) {
	t.Helper()
	// Explicit TEST ONLY budgets, not approved deployment limits/allowlists.
	v, e := config.NewPolicyValidator(config.PolicyLimits{MaxJSONBytes: 8192, MaxDecimalDigits: 128, MaxDecimalExponent: 256, MaxColdAfterDays: 1000, MaxAutoRestoreDays: 30, MaxReadaheadChunks: 64, RestoreTiers: []config.RestoreTierRule{{Class: storage.GlacierIR, Tier: "Standard"}}})
	if e != nil {
		t.Fatal(e)
	}
	wall := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	fence := storage.WriterFence{LeaseID: "writer", ServiceIncarnation: "svc", Epoch: 7}
	store := fake.NewMetadata(fake.Seed{Now: func() time.Time { return wall }, Users: []storage.User{{ID: "admin", Active: true}}, Credentials: []storage.Credential{{ID: "key", UserID: "admin", Active: true, SystemAdmin: true}}, Writers: []storage.WriterLease{{Fence: fence, ExpiresAt: wall.Add(time.Hour)}}})
	return v, store, fence, wall
}
func event(id string, r uint64) *storage.OutboxEvent {
	return &storage.OutboxEvent{Version: storage.ModelVersion, ID: id, HistoryID: "history", Entity: "SystemConfig", EntityID: "global-policy", Revision: r, OpID: id, PayloadPointer: "payload/" + id, PayloadSHA256: strings.Repeat("a", 64)}
}
func create(t *testing.T, v config.PolicyValidator, s storage.MetadataStore, f storage.WriterFence) i.Context {
	t.Helper()
	c, e := i.CreateGlobal(context.Background(), s, v, config.DefaultGlobalPolicy(), "key", f, event("create", 1))
	if e != nil {
		t.Fatal(e)
	}
	return c
}
func proposal(t *testing.T, v config.PolicyValidator, c i.Context, patch string) config.GlobalProposal {
	t.Helper()
	p, e := v.MergeGlobal(c.Global(), []byte(patch), c.Global().Revision())
	if e != nil {
		t.Fatal(e)
	}
	candidate, e := p.Candidate()
	if e != nil || candidate.Revision != 0 {
		t.Fatal(candidate, e)
	}
	return p
}

func TestGlobalCASHistoryAndPin(t *testing.T) {
	v, s, f, _ := setup(t)
	ctx := context.Background()
	old := create(t, v, s, f)
	a, e := i.LoadGlobal(ctx, s, v, 0)
	if e != nil {
		t.Fatal(e)
	}
	b, e := i.LoadGlobal(ctx, s, v, 1)
	if e != nil || a.Global() != b.Global() {
		t.Fatal(e)
	}
	pa := proposal(t, v, a, `{"compression":{"enabled":false}}`)
	pb := proposal(t, v, b, `{"tiering":{"cold_after_days":0}}`)
	noop := proposal(t, v, b, `{}`)
	before, e := s.GetPolicy(ctx, 0)
	if e != nil {
		t.Fatal(e)
	}
	// Preparation cannot mutate authoritative state or snapshots.
	unchanged, _ := s.GetPolicy(ctx, 0)
	if !reflect.DeepEqual(before, unchanged) || a.Global() != old.Global() {
		t.Fatal("proposal mutated state")
	}
	type result struct {
		c    i.Context
		err  error
		name string
	}
	start := make(chan struct{})
	done := make(chan result, 2)
	for _, item := range []struct {
		name string
		p    config.GlobalProposal
	}{{"a", pa}, {"b", pb}} {
		go func(name string, p config.GlobalProposal) {
			<-start
			c, e := i.CommitGlobal(ctx, s, v, p, "key", f, event(name, 2))
			done <- result{c, e, name}
		}(item.name, item.p)
	}
	close(start)
	var winner, loser result
	for n := 0; n < 2; n++ {
		r := <-done
		if r.err == nil {
			winner = r
		} else if errors.Is(r.err, storage.ErrConflict) {
			loser = r
		} else {
			t.Fatal(r.err)
		}
	}
	if winner.name == "" || loser.name == "" || winner.c.Global().Revision() != 2 {
		t.Fatal(winner, loser)
	}
	if _, e := i.CommitGlobal(ctx, s, v, noop, "key", f, nil); !errors.Is(e, storage.ErrConflict) {
		t.Fatal("stale no-op", e)
	}
	history, e := i.LoadGlobal(ctx, s, v, 1)
	if e != nil || history.Global() != old.Global() {
		t.Fatal("history changed", e)
	}
	current, e := i.LoadGlobal(ctx, s, v, 0)
	if e != nil || current.Global() != winner.c.Global() {
		t.Fatal("partial state", e)
	}
	if _, e := s.GetOutbox(ctx, winner.name); e != nil {
		t.Fatal(e)
	}
	if _, e := s.GetOutbox(ctx, loser.name); !errors.Is(e, storage.ErrNotFound) {
		t.Fatal("loser event", e)
	}
	if _, e := s.GetPolicy(ctx, 3); !errors.Is(e, storage.ErrNotFound) {
		t.Fatal("extra history", e)
	}
	if _, e := i.LoadGlobal(ctx, s, v, 99); !errors.Is(e, storage.ErrNotFound) {
		t.Fatal("history fallback", e)
	}
	// Returned wire bytes are owned; mutation cannot alter history or pinned state.
	raw, e := s.GetPolicy(ctx, 1)
	if e != nil {
		t.Fatal(e)
	}
	raw.Document[0] = '!'
	again, e := i.LoadGlobal(ctx, s, v, 1)
	if e != nil || again.Global() != old.Global() {
		t.Fatal("snapshot alias", e)
	}
}

func TestBucketHandoffIndependentRevisions(t *testing.T) {
	v, s, f, _ := setup(t)
	global := create(t, v, s, f)
	// LOCAL constructor/decode evidence only. No bucket persistence/history port
	// exists; these snapshots MUST NOT be called authoritative store reads.
	bucket, e := v.NewBucketSnapshot("bucket", 9, config.DefaultCachePolicy())
	if e != nil {
		t.Fatal(e)
	}
	decoded, e := v.DecodeBucket("bucket", 9, []byte(`{"cache":{"admit_on_write":true,"readahead_chunks":4}}`))
	if e != nil || decoded != bucket {
		t.Fatal(decoded, e)
	}
	bound, e := global.BindBucket(decoded, "bucket", 9)
	if e != nil || bound.Global().Revision() != 1 || bound.Bucket().Revision() != 9 {
		t.Fatal(bound, e)
	}
	for _, tc := range []struct {
		id  string
		rev uint64
	}{{"bucket", 1}, {"other", 9}, {"bucket", 0}} {
		if _, e := global.BindBucket(bucket, tc.id, tc.rev); !errors.Is(e, storage.ErrInvalid) {
			t.Fatal(e)
		}
	}
	if _, e := global.BindBucket(config.BucketSnapshot{}, "bucket", 9); !errors.Is(e, storage.ErrInvalid) {
		t.Fatal(e)
	}
	p := proposal(t, v, global, `{"compression":{"enabled":false}}`)
	next, e := i.CommitGlobal(context.Background(), s, v, p, "key", f, event("change", 2))
	if e != nil {
		t.Fatal(e)
	}
	next, e = next.BindBucket(bucket, "bucket", 9)
	if e != nil || next.Bucket() != bound.Bucket() || bound.Global().Revision() != 1 {
		t.Fatal("mixed revisions", e)
	}
	if _, e := v.DecodeBucket("bucket", 9, []byte(`{"cache":{"admit_on_write":false}}`)); !errors.Is(e, storage.ErrInvalid) {
		t.Fatal("bucket default fallback", e)
	}
}

func TestCorruptHistoricalSnapshotNoDefaults(t *testing.T) {
	for _, doc := range []string{`{}`, `{"tiering":{}}`, `{"tiering":null}`, `not-json`} {
		v, s, f, _ := setup(t)
		// Fake envelope accepts JSON objects without full field validation; deliberately
		// exercise existing fake behavior, never add a fake history corruption port.
		if doc == "not-json" {
			if _, e := v.ResolveGlobal(1, storage.PolicySnapshot{Version: 1, Revision: 1, Document: []byte(doc)}, nil); !errors.Is(e, storage.ErrInvalid) {
				t.Fatal(e)
			}
			continue
		}
		_, e := s.PutPolicy(context.Background(), storage.PolicyUpdate{Candidate: storage.PolicySnapshot{Version: 1, Document: []byte(doc)}, CredentialID: "key", Writer: f, Event: event("bad", 1)})
		if e != nil {
			t.Fatal(e)
		}
		for _, r := range []uint64{0, 1} {
			if _, e := i.LoadGlobal(context.Background(), s, v, r); !errors.Is(e, storage.ErrInvalid) {
				t.Fatal("repaired historical policy", e)
			}
		}
	}
}

func TestMutationAuthorityAndUnknownOutcome(t *testing.T) {
	for _, name := range []string{"auth", "fence", "outbox", "noop", "before", "after"} {
		t.Run(name, func(t *testing.T) {
			v, s, f, _ := setup(t)
			c := create(t, v, s, f)
			ctx := context.Background()
			p := proposal(t, v, c, `{"compression":{"enabled":false}}`)
			ev := event("mutation", 2)
			credential := "key"
			writer := f
			want := storage.ErrInvalid
			switch name {
			case "auth":
				credential = "unknown"
				want = storage.ErrAccessDenied
			case "fence":
				writer.ServiceIncarnation = "old"
				want = storage.ErrFenced
			case "outbox":
				ev = nil
			case "noop":
				p = proposal(t, v, c, `{}`)
				ev = nil
				want = nil
			case "before":
				s.FailCommit(true, false)
				want = storage.ErrUnavailable
			case "after":
				s.FailCommit(false, true)
				want = storage.ErrUnavailable
			}
			result, e := i.CommitGlobal(ctx, s, v, p, credential, writer, ev)
			if !errors.Is(e, want) {
				t.Fatal(result, e)
			}
			raw, e := s.GetPolicy(ctx, 0)
			if e != nil {
				t.Fatal(e)
			}
			if name == "after" {
				if raw.Revision != 2 {
					t.Fatal("lost ACK lost commit")
				}
				if _, e := s.GetOutbox(ctx, "mutation"); e != nil {
					t.Fatal(e)
				}
			} else {
				if raw.Revision != 1 {
					t.Fatal("partial write")
				}
				if _, e := s.GetOutbox(ctx, "mutation"); !errors.Is(e, storage.ErrNotFound) {
					t.Fatal(e)
				}
			}
		})
	}
}

func TestPinnedEvaluatorsParityAndEvidence(t *testing.T) {
	v, s, f, wall := setup(t)
	c := create(t, v, s, f)
	ctx := context.Background()
	before, e := s.GetPolicy(ctx, 0)
	if e != nil {
		t.Fatal(e)
	}
	// Advance current, retain operation's admitted R1 snapshot.
	next, e := i.CommitGlobal(ctx, s, v, proposal(t, v, c, `{"compression":{"enabled":false}}`), "key", f, event("update", 2))
	if e != nil {
		t.Fatal(e)
	}
	virtual, e := clock.NewVirtual(wall, 10)
	if e != nil {
		t.Fatal(e)
	}
	var readExpected, writeExpected policy.Decision
	var protectionExpected policy.ProtectionDecision
	for n, source := range []clock.Clock{clock.NewReal(), virtual} {
		now := source.Now().Instant
		deadline, e := now.Add(time.Minute)
		if e != nil {
			t.Fatal(e)
		}
		request := policy.Request{Now: now, Deadline: deadline}
		makeUTC := func(upper time.Time) policy.UTCProof {
			p, e := policy.ValidateUTCSample(policy.UTCSource{ID: "test", Provenance: "modeled", RestartID: "run", Verified: policy.Valid}, policy.UTCSample{SourceID: "test", Provenance: "modeled", RestartID: "run", Observed: now, ValidThrough: deadline, Lower: upper.Add(-time.Second), Upper: upper, Precision: policy.DurationBound{Known: true}, MeasurementError: policy.DurationBound{Known: true}, NodeSkew: policy.DurationBound{Known: true}, Drift: policy.DriftBound{Known: true}, Sync: policy.Valid}, request)
			if e != nil {
				t.Fatal(e)
			}
			return p
		}
		utc := makeUTC(wall)
		pack := storage.PackInfo{Version: 1, Ref: storage.PackRef{ID: "p", Generation: "g"}, Size: 100, State: "Restored", Class: storage.Glacier, StateRevision: 1, RestoredUntil: wall.Add(3 * time.Minute)}
		read := policy.ReadInput{Request: request, UTC: utc, Pack: pack, Facts: policy.ReadFacts{Logical: policy.Valid, Metadata: policy.Valid, Authority: policy.Valid, PinCoverage: policy.Valid, Physical: policy.Valid}, RestoreVerified: policy.Valid}
		write := policy.WriteInput{Request: request, Operation: policy.PUT, Authority: policy.Valid, Metadata: policy.Valid, ReusePins: policy.Valid, Packs: []storage.PackInfo{pack}}
		protect := policy.ProtectionInput{Request: request, UTC: utc, Pack: pack, OperationID: "op", ServiceIncarnation: "svc", Writer: f, Binding: policy.Valid, Authority: policy.Valid, Precision: time.Nanosecond}
		r, e := c.EvaluateRead(read)
		if e != nil || r.Verdict != policy.Eligible || r.PolicyRevision != 1 {
			t.Fatal(r, e)
		}
		w, e := c.EvaluateWrite(write)
		if e != nil || w.Verdict != policy.Eligible || w.PolicyRevision != 1 || len(w.Actions) != 1 || w.Actions[0].Kind != policy.ReviveFromBody {
			t.Fatal(w, e)
		}
		p, e := c.EvaluateProtection(protect)
		if e != nil || p.Verdict != policy.Eligible || p.PolicyRevision != 1 {
			t.Fatal(p, e)
		}
		if n == 0 {
			readExpected, writeExpected, protectionExpected = r, w, p
		} else if !reflect.DeepEqual(r, readExpected) || !reflect.DeepEqual(w, writeExpected) || !reflect.DeepEqual(p, protectionExpected) {
			t.Fatal("parity mismatch")
		}
		// Request supplied a different pin: never replace it silently with R1.
		wrong := policy.PinnedPolicy{Snapshot: next.Global(), RequiredRevision: 2}
		read.Policy = wrong
		write.Policy = wrong
		protect.Policy = wrong
		if d, e := c.EvaluateRead(read); !errors.Is(e, storage.ErrConflict) || d.Verdict != policy.Indeterminate {
			t.Fatal(d, e)
		}
		if d, e := c.EvaluateWrite(write); !errors.Is(e, storage.ErrConflict) || len(d.Actions) != 0 {
			t.Fatal(d, e)
		}
		if d, e := c.EvaluateProtection(protect); !errors.Is(e, storage.ErrConflict) || !d.Target.IsZero() {
			t.Fatal(d, e)
		}
		read.Policy = policy.PinnedPolicy{}
		protect.Policy = policy.PinnedPolicy{}
		for _, proof := range []policy.UTCProof{{}, {Lower: wall, Upper: wall, Horizon: deadline}} {
			read.UTC = proof
			protect.UTC = proof
			if d, e := c.EvaluateRead(read); e != nil || d.Verdict != policy.Indeterminate {
				t.Fatal(d, e)
			}
			if d, e := c.EvaluateProtection(protect); e != nil || d.Verdict != policy.Indeterminate {
				t.Fatal(d, e)
			}
		}
		protect.UTC = utc
		protect.Writer.ServiceIncarnation = "old"
		if d, e := c.EvaluateProtection(protect); !errors.Is(e, policy.ErrEvidence) || d.Verdict != policy.Indeterminate {
			t.Fatal(d, e)
		}
		protect.Writer = f
		protect.Binding = policy.Unknown
		if d, e := c.EvaluateProtection(protect); e != nil || d.Verdict != policy.Indeterminate {
			t.Fatal(d, e)
		}
		protect.Binding = policy.Valid
		// Concurrent extension PROPOSALS only: no atomic protection store is tested.
		a, e := c.EvaluateProtection(protect)
		if e != nil {
			t.Fatal(e)
		}
		protect.UTC.Upper = wall.Add(time.Hour)
		protect.UTC = makeUTC(wall.Add(time.Hour))
		b, e := c.EvaluateProtection(protect)
		if e != nil || !b.Target.After(a.Target) {
			t.Fatal(a, b, e)
		}
	}
	after, e := s.GetPolicy(ctx, 0)
	if e != nil || after.Revision != 2 {
		t.Fatal(e)
	}
	old, e := s.GetPolicy(ctx, 1)
	if e != nil || !bytes.Equal(old.Document, before.Document) {
		t.Fatal("evaluator mutated history")
	}
	expected, e := next.Global().StorageSnapshot()
	if e != nil || !bytes.Equal(after.Document, expected.Document) {
		t.Fatal("evaluator mutated current")
	}
}

func TestHelperBoundaries(t *testing.T) {
	v, s, f, _ := setup(t)
	ctx := context.Background()
	if _, e := i.LoadGlobal(ctx, nil, v, 0); !errors.Is(e, storage.ErrInvalid) {
		t.Fatal(e)
	}
	if _, e := i.LoadGlobal(ctx, s, v, storage.MaxPolicyRevision+1); !errors.Is(e, storage.ErrInvalid) {
		t.Fatal(e)
	}
	if _, e := i.CreateGlobal(ctx, nil, v, config.DefaultGlobalPolicy(), "key", f, event("create", 1)); !errors.Is(e, storage.ErrInvalid) {
		t.Fatal(e)
	}
	invalid := config.DefaultGlobalPolicy()
	invalid.Chunking.MinBytes = -1
	if _, e := i.CreateGlobal(ctx, s, v, invalid, "key", f, event("create", 1)); !errors.Is(e, storage.ErrInvalid) {
		t.Fatal(e)
	}
	c := create(t, v, s, f)
	if _, e := i.CreateGlobal(ctx, s, v, config.DefaultGlobalPolicy(), "key", f, event("duplicate", 1)); !errors.Is(e, storage.ErrConflict) {
		t.Fatal(e)
	}
	if _, e := i.CommitGlobal(ctx, nil, v, config.GlobalProposal{}, "key", f, nil); !errors.Is(e, storage.ErrInvalid) {
		t.Fatal(e)
	}
	if _, e := i.CommitGlobal(ctx, s, v, config.GlobalProposal{}, "key", f, nil); !errors.Is(e, storage.ErrInvalid) {
		t.Fatal(e)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, e := i.LoadGlobal(cancelled, s, v, 1); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	// Same revision but DIFFERENT complete content is also a conflicting pin.
	forgedPolicy := c.Global().Policy()
	forgedPolicy.Compression.Enabled = false
	forged, e := v.NewGlobalSnapshot(1, forgedPolicy)
	if e != nil {
		t.Fatal(e)
	}
	pin := policy.PinnedPolicy{Snapshot: forged, RequiredRevision: 1}
	if d, e := c.EvaluateRead(policy.ReadInput{Policy: pin}); !errors.Is(e, storage.ErrConflict) || d.Verdict != policy.Indeterminate {
		t.Fatal(d, e)
	}
	if d, e := c.EvaluateWrite(policy.WriteInput{Policy: pin}); !errors.Is(e, storage.ErrConflict) || d.Verdict != policy.Indeterminate || d.Actions != nil {
		t.Fatal(d, e)
	}
	if d, e := c.EvaluateProtection(policy.ProtectionInput{Policy: pin}); !errors.Is(e, storage.ErrConflict) || !d.Target.IsZero() {
		t.Fatal(d, e)
	}
	// Zero Context is unusable; no default policy or selector-current lookup.
	var zero i.Context
	if d, e := zero.EvaluateRead(policy.ReadInput{}); !errors.Is(e, storage.ErrInvalid) || d.Verdict != policy.Indeterminate {
		t.Fatal(d, e)
	}
	if _, e := zero.BindBucket(config.BucketSnapshot{}, "bucket", 1); !errors.Is(e, storage.ErrInvalid) {
		t.Fatal(e)
	}
	// Explicit matching pin succeeds through normal evaluator evidence checks.
	if d, e := c.EvaluateRead(policy.ReadInput{Policy: policy.PinnedPolicy{Snapshot: c.Global(), RequiredRevision: 1}}); !errors.Is(e, clock.ErrDomain) || d.Verdict != policy.Indeterminate {
		t.Fatal(d, e)
	}
	raw, e := s.GetPolicy(ctx, 0)
	if e != nil || raw.Revision != 1 {
		t.Fatal("boundary wrote state", raw, e)
	}
}

// Publication spy delegates ALL persistence/CAS/auth/fence/outbox behavior to
// the existing shared fake. It counts calls and can corrupt returned bytes only;
// it is not another metadata implementation or invented CAS behavior.
type publicationSpy struct {
	storage.MetadataStore
	calls             int
	candidateRevision uint64
	response          func(storage.PolicySnapshot) storage.PolicySnapshot
}

func (s *publicationSpy) PutPolicy(ctx context.Context, r storage.PolicyUpdate) (storage.PolicySnapshot, error) {
	s.calls++
	s.candidateRevision = r.Candidate.Revision
	result, err := s.MetadataStore.PutPolicy(ctx, r)
	if err == nil && s.response != nil {
		result = s.response(result)
	}
	return result, err
}

func TestINT_R01ValidateBeforePublication(t *testing.T) {
	for _, name := range []string{"A-unconfigured", "B-stricter", "C-accepted", "D-stale-change", "E-stale-noop", "F-unknown-before", "F-unknown-after"} {
		t.Run(name, func(t *testing.T) {
			a, store, f, _ := setup(t)
			ctx := context.Background()
			original := create(t, a, store, f)
			p := proposal(t, a, original, `{"tiering":{"cold_after_days":500}}`)
			b := a
			wantErr := error(nil)
			wantRevision := uint64(2)
			wantCalls := 1
			switch name {
			case "A-unconfigured":
				b = config.PolicyValidator{}
				wantErr = config.ErrPolicyDeferred
				wantRevision = 1
				wantCalls = 0
			case "B-stricter":
				var err error
				b, err = config.NewPolicyValidator(config.PolicyLimits{MaxJSONBytes: 8192, MaxDecimalDigits: 128, MaxDecimalExponent: 256, MaxColdAfterDays: 100, MaxAutoRestoreDays: 30, MaxReadaheadChunks: 64, RestoreTiers: []config.RestoreTierRule{{Class: storage.GlacierIR, Tier: "Standard"}}})
				if err != nil {
					t.Fatal(err)
				}
				wantErr = storage.ErrInvalid
				wantRevision = 1
				wantCalls = 0
			case "D-stale-change", "E-stale-noop":
				if name == "E-stale-noop" {
					p = proposal(t, a, original, `{}`)
				}
				_, err := i.CommitGlobal(ctx, store, a, proposal(t, a, original, `{"compression":{"enabled":false}}`), "key", f, event("winner", 2))
				if err != nil {
					t.Fatal(err)
				}
				wantErr = storage.ErrConflict
			case "F-unknown-before":
				store.FailCommit(true, false)
				wantErr = storage.ErrUnavailable
				wantRevision = 1
			case "F-unknown-after":
				store.FailCommit(false, true)
				wantErr = storage.ErrUnavailable
			}
			before, err := store.GetPolicy(ctx, 0)
			if err != nil {
				t.Fatal(err)
			}
			spy := &publicationSpy{MetadataStore: store}
			candidateBefore, err := p.Candidate()
			if err != nil {
				t.Fatal(err)
			}
			result, err := i.CommitGlobal(ctx, spy, b, p, "key", f, event("attempt", 2))
			if !errors.Is(err, wantErr) || (err != nil) != (wantErr != nil) || spy.calls != wantCalls || spy.candidateRevision != 0 {
				t.Fatal(result, err, spy.calls)
			}
			candidateAfter, e := p.Candidate()
			if e != nil || !reflect.DeepEqual(candidateBefore, candidateAfter) {
				t.Fatal("candidate repaired/mutated", e)
			}
			current, e := store.GetPolicy(ctx, 0)
			if e != nil || current.Revision != wantRevision {
				t.Fatal(current, e)
			}
			history, e := store.GetPolicy(ctx, 1)
			originalBytes, encodeErr := original.Global().StorageSnapshot()
			if e != nil || encodeErr != nil || !bytes.Equal(history.Document, originalBytes.Document) {
				t.Fatal("history R1 mutated", e)
			}
			committed := name == "C-accepted" || name == "F-unknown-after"
			if committed {
				h, e := store.GetPolicy(ctx, 2)
				if e != nil || !reflect.DeepEqual(h, current) {
					t.Fatal("current/history divergence", e)
				}
				ev, e := store.GetOutbox(ctx, "attempt")
				if e != nil || ev != *event("attempt", 2) {
					t.Fatal("outbox missing", e)
				}
				decoded, e := a.DecodeGlobal(current)
				if e != nil || decoded.Policy() != p.Policy() {
					t.Fatal("wrong committed content", e)
				}
			} else {
				if !reflect.DeepEqual(before, current) {
					t.Fatal("publication despite failure")
				}
				if _, e := store.GetOutbox(ctx, "attempt"); !errors.Is(e, storage.ErrNotFound) {
					t.Fatal("partial event", e)
				}
				if wantRevision == 1 {
					if _, e := store.GetPolicy(ctx, 2); !errors.Is(e, storage.ErrNotFound) {
						t.Fatal("partial history", e)
					}
				}
			}
			if _, e := store.GetPolicy(ctx, 3); !errors.Is(e, storage.ErrNotFound) {
				t.Fatal("implicit retry/rebase", e)
			}
			if wantErr == nil && result.Global().Revision() != 2 {
				t.Fatal(result)
			}
			if wantErr != nil && result.Global().Revision() != 0 {
				t.Fatal("unverified context escaped", result)
			}
		})
	}
}

func TestINT_R01PostPublicationVerification(t *testing.T) {
	for _, tc := range []struct {
		name  string
		alter func(storage.PolicySnapshot) storage.PolicySnapshot
	}{
		{"revision", func(s storage.PolicySnapshot) storage.PolicySnapshot { s.Revision = 99; return s }},
		{"incomplete", func(s storage.PolicySnapshot) storage.PolicySnapshot { s.Document = []byte(`{}`); return s }},
		{"version", func(s storage.PolicySnapshot) storage.PolicySnapshot { s.Version = 0; return s }},
		{"different valid content", func(s storage.PolicySnapshot) storage.PolicySnapshot {
			s.Document = bytes.Replace(s.Document, []byte(`"cold_after_days":500`), []byte(`"cold_after_days":30`), 1)
			return s
		}},
	} {
		t.Run("G-"+tc.name, func(t *testing.T) {
			v, store, f, _ := setup(t)
			ctx := context.Background()
			c := create(t, v, store, f)
			p := proposal(t, v, c, `{"tiering":{"cold_after_days":500}}`)
			spy := &publicationSpy{MetadataStore: store, response: tc.alter}
			got, err := i.CommitGlobal(ctx, spy, v, p, "key", f, event("committed", 2))
			if !errors.Is(err, storage.ErrUnavailable) || errors.Is(err, storage.ErrInvalid) || errors.Is(err, config.ErrPolicyDeferred) || got.Global().Revision() != 0 || spy.calls != 1 || !strings.Contains(err.Error(), "reconcile") {
				t.Fatal(got, err, spy.calls)
			}
			raw, e := store.GetPolicy(ctx, 0)
			if e != nil || raw.Revision != 2 {
				t.Fatal("rollback/unknown lost", raw, e)
			}
			snapshot, e := v.DecodeGlobal(raw)
			if e != nil || snapshot.Policy() != p.Policy() {
				t.Fatal("response corruption mutated backend", e)
			}
			h, e := store.GetPolicy(ctx, 2)
			if e != nil || !reflect.DeepEqual(h, raw) {
				t.Fatal(e)
			}
			if _, e := store.GetOutbox(ctx, "committed"); e != nil {
				t.Fatal(e)
			}
			if _, e := store.GetPolicy(ctx, 3); !errors.Is(e, storage.ErrNotFound) {
				t.Fatal(e)
			}
		})
	}
	// Bootstrap uses the same conservative outcome classification.
	v, store, f, _ := setup(t)
	spy := &publicationSpy{MetadataStore: store, response: func(s storage.PolicySnapshot) storage.PolicySnapshot { s.Revision = 9; return s }}
	_, err := i.CreateGlobal(context.Background(), spy, v, config.DefaultGlobalPolicy(), "key", f, event("bootstrap", 1))
	if !errors.Is(err, storage.ErrUnavailable) || spy.calls != 1 {
		t.Fatal(err)
	}
	if raw, e := store.GetPolicy(context.Background(), 0); e != nil || raw.Revision != 1 {
		t.Fatal(raw, e)
	}
}
