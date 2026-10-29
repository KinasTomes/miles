package contracttest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/KinasTomes/miles/internal/config"
	i "github.com/KinasTomes/miles/internal/config/integration"
	"github.com/KinasTomes/miles/internal/recovery"
	s "github.com/KinasTomes/miles/internal/storage"
	shared "github.com/KinasTomes/miles/internal/storage/contracttest"
)

// RunPolicyPersistence uses the actual adapter transactions, never a CAS
// reimplementation. DynamoDB factories must run against explicit DDB Local.
func RunPolicyPersistence(t *testing.T, factory shared.MetadataFactory) {
	runProtectionJournal(t, factory)
	t.Run("global-rejects-bucket-event-without-publication", func(t *testing.T) {
		f := shared.NewFixture()
		h := factory(t, f)
		ctx := context.Background()
		doc, e := json.Marshal(config.DefaultGlobalPolicy())
		if e != nil {
			t.Fatal(e)
		}
		op := strings.Repeat("2", 32)
		history := strings.Repeat("1", 32)
		id, e := s.EventID(history, op, "SystemConfig", "entity_put")
		if e != nil {
			t.Fatal(e)
		}
		event := &s.OutboxEvent{Version: 1, ID: id, HistoryID: history, OpID: op, Entity: "SystemConfig", EntityID: s.BucketPolicyEntityID(f.Bucket.ID), Action: "entity_put", Revision: 1, PayloadPointer: "test-payload", PayloadSHA256: strings.Repeat("a", 64)}
		if err := event.Validate(); err != nil {
			t.Fatal("bucket event should be valid for its own operation", err)
		}
		if _, err := h.Store.PutPolicy(ctx, s.PolicyUpdate{Candidate: s.PolicySnapshot{Version: 1, Document: doc}, CredentialID: "admin-key", Writer: f.Lease.Fence, Event: event}); !errors.Is(err, s.ErrInvalid) {
			t.Fatalf("global policy accepted bucket event: %v", err)
		}
		for _, revision := range []uint64{0, 1} {
			if p, err := h.Store.GetPolicy(ctx, revision); !errors.Is(err, s.ErrNotFound) {
				t.Fatal("rejected identity published current/history", p, err)
			}
		}
		if _, err := h.Store.GetOutbox(ctx, event.ID); !errors.Is(err, s.ErrNotFound) {
			t.Fatal("rejected identity published event", err)
		}
	})
	t.Run("global-bucket-revisions-cas-history-producer", func(t *testing.T) {
		f := shared.NewFixture()
		h := factory(t, f)
		store := h.Store.(s.BucketPolicyStore)
		producer := h.Store.(s.ProducerStore)
		v, err := config.NewPolicyValidator(config.DSGPolicyLimits())
		if err != nil {
			t.Fatal(err)
		}
		mutation := func(label string) (context.Context, s.MutationContext) {
			sum := sha256.Sum256([]byte(label))
			op := hex.EncodeToString(sum[:16])
			m := s.MutationContext{HistoryID: strings.Repeat("1", 32), LogicalOpID: op, MutationID: op, Writer: f.Lease.Fence}
			return s.WithMutation(context.Background(), m), m
		}
		ctx, m := mutation("global-policy-bootstrap")
		eid, _ := s.EventID(m.HistoryID, m.MutationID, "SystemConfig", "entity_put")
		event := &s.OutboxEvent{Version: 1, ID: eid, Entity: "SystemConfig", Action: "entity_put", EntityID: "global-policy", OpID: m.MutationID, HistoryID: m.HistoryID, Revision: 1, PayloadPointer: "placeholder", PayloadSHA256: strings.Repeat("a", 64)}
		global, err := i.CreateGlobal(ctx, h.Store, v, config.DefaultGlobalPolicy(), "admin-key", f.Lease.Fence, event)
		if err != nil {
			t.Fatal("global bootstrap", err)
		}
		if _, err := global.LoadBucket(ctx, store, v, f.Bucket.Name, f.Bucket.ID, 0); !errors.Is(err, s.ErrNotFound) {
			t.Fatal("missing defaults fabricated", err)
		}
		ctx, _ = mutation("bucket-bootstrap")
		original, err := global.CreateBucketPolicy(ctx, store, v, f.Bucket.Name, f.Bucket.ID, "owner-key", f.Lease.Fence, config.DefaultCachePolicy())
		if err != nil || original.Bucket().Revision() != 1 {
			t.Fatal(original, err)
		}
		loaded, err := i.LoadGlobal(ctx, h.Store, v, 1)
		if err != nil {
			t.Fatal("reload global", err)
		}
		loaded, err = loaded.LoadBucket(ctx, store, v, f.Bucket.Name, f.Bucket.ID, 1)
		if err != nil || loaded.Global() != original.Global() || loaded.Bucket() != original.Bucket() {
			t.Fatal("native round trip", loaded, err)
		}
		checkPersistedEligibility(t, loaded, f.Now)
		pA, err := v.MergeBucket(original.Bucket(), []byte(`{"cache":{"admit_on_write":false},"expected_revision":1,"expected_bucket_id":"bucket-1"}`), 0)
		if err != nil {
			t.Fatal(err)
		}
		pB, err := v.MergeBucket(original.Bucket(), []byte(`{"cache":{"readahead_chunks":0},"expected_revision":1,"expected_bucket_id":"bucket-1"}`), 0)
		if err != nil {
			t.Fatal(err)
		}
		noOp, err := v.MergeBucket(original.Bucket(), []byte(`{"expected_revision":1,"expected_bucket_id":"bucket-1"}`), 0)
		if err != nil {
			t.Fatal(err)
		}
		type result struct {
			value    i.Context
			err      error
			mutation s.MutationContext
		}
		start := make(chan struct{})
		results := make(chan result, 2)
		var wg sync.WaitGroup
		for n, p := range []config.BucketProposal{pA, pB} {
			wg.Add(1)
			go func(n int, p config.BucketProposal) {
				defer wg.Done()
				<-start
				ctx, m := mutation(fmt.Sprint("concurrent-", n))
				value, e := original.CommitBucket(ctx, store, v, f.Bucket.Name, "owner-key", f.Lease.Fence, p)
				results <- result{value, e, m}
			}(n, p)
		}
		close(start)
		wg.Wait()
		close(results)
		var winner i.Context
		successes := 0
		for r := range results {
			if r.err == nil {
				successes++
				winner = r.value
			} else {
				if !errors.Is(r.err, s.ErrConflict) {
					t.Fatal(r.err)
				}
				if _, e := producer.GetMutation(context.Background(), r.mutation.MutationID); !errors.Is(e, s.ErrNotFound) {
					t.Fatal("loser outcome", e)
				}
			}
		}
		if successes != 1 || winner.Bucket().Revision() != 2 {
			t.Fatal("CAS winners", successes, winner)
		}
		ctx, _ = mutation("stale-noop")
		if _, e := original.CommitBucket(ctx, store, v, f.Bucket.Name, "owner-key", f.Lease.Fence, noOp); !errors.Is(e, s.ErrConflict) {
			t.Fatal("stale noop", e)
		}
		history, err := global.LoadBucket(ctx, store, v, f.Bucket.Name, f.Bucket.ID, 1)
		if err != nil || history.Bucket() != original.Bucket() {
			t.Fatal("history mutated", history, err)
		}
		if current, e := h.Store.GetPolicy(ctx, 0); e != nil || current.Revision != 1 {
			t.Fatal("bucket bumped global", current, e)
		}
		if _, e := global.LoadBucket(ctx, store, v, f.Bucket.Name, f.Bucket.ID, 99); !errors.Is(e, s.ErrNotFound) {
			t.Fatal("fallback history", e)
		}
		doc, _ := store.GetBucketPolicy(ctx, f.Bucket.Name, f.Bucket.ID, 1)
		expected := bytes.Clone(doc.Document)
		doc.Document[0] = '!'
		again, _ := store.GetBucketPolicy(ctx, f.Bucket.Name, f.Bucket.ID, 1)
		if !bytes.Equal(again.Document, expected) {
			t.Fatal("mutable alias")
		}
		patch := fmt.Sprintf(`{"expected_revision":2,"expected_bucket_id":%q}`, f.Bucket.ID)
		p, err := v.MergeBucket(winner.Bucket(), []byte(patch), 0)
		if err != nil {
			t.Fatal(err)
		}
		ctx, m = mutation("current-noop")
		unchanged, err := winner.CommitBucket(ctx, store, v, f.Bucket.Name, "admin-key", f.Lease.Fence, p)
		if err != nil || unchanged.Bucket() != winner.Bucket() {
			t.Fatal("noop", err)
		}
		outcome, err := producer.GetMutation(ctx, m.MutationID)
		if err != nil || !outcome.NoOp || outcome.Event.ID != "" {
			t.Fatal("noop event", outcome, err)
		}
		if retry, err := winner.CommitBucket(ctx, store, v, f.Bucket.Name, "admin-key", f.Lease.Fence, p); err != nil || retry.Bucket() != unchanged.Bucket() {
			t.Fatal("noop retry", retry, err)
		}
		// A content change with lost ACK is committed, retained and replayable.
		change := winner.Bucket().Cache()
		change.ReadaheadChunks = 7
		patch = fmt.Sprintf(`{"expected_revision":2,"expected_bucket_id":%q,"cache":{"readahead_chunks":7}}`, f.Bucket.ID)
		p, err = v.MergeBucket(winner.Bucket(), []byte(patch), 0)
		if err != nil {
			t.Fatal(err)
		}
		ctx, m = mutation("publish-fault")
		h.FailCommit(true, false)
		if _, e := winner.CommitBucket(ctx, store, v, f.Bucket.Name, "owner-key", f.Lease.Fence, p); !errors.Is(e, s.ErrUnavailable) {
			t.Fatal("before fault", e)
		}
		current, _ := store.GetBucketPolicy(ctx, f.Bucket.Name, f.Bucket.ID, 0)
		if current.Revision != 2 {
			t.Fatal("partial current", current)
		}
		if _, e := store.GetBucketPolicy(ctx, f.Bucket.Name, f.Bucket.ID, 3); !errors.Is(e, s.ErrNotFound) {
			t.Fatal("partial history", e)
		}
		if _, e := producer.GetMutation(ctx, m.MutationID); !errors.Is(e, s.ErrNotFound) {
			t.Fatal("partial outbox", e)
		}
		h.FailCommit(false, true)
		if _, e := winner.CommitBucket(ctx, store, v, f.Bucket.Name, "owner-key", f.Lease.Fence, p); !errors.Is(e, s.ErrUnavailable) {
			t.Fatal("lost ACK", e)
		}
		h.FailCommit(false, false)
		outcome, err = producer.GetMutation(ctx, m.MutationID)
		if err != nil || outcome.Event.Revision != 3 {
			t.Fatal("retained mutation", outcome, err)
		}
		retried, err := winner.CommitBucket(ctx, store, v, f.Bucket.Name, "owner-key", f.Lease.Fence, p)
		if err != nil || retried.Bucket().Revision() != 3 || retried.Bucket().Cache() != change {
			t.Fatal("retry original", retried, err)
		}
		sink := producerSink{}
		if err := (recovery.Exporter{Metadata: h.Store.(s.OutboxExportStore), Payloads: producer, Objects: sink}).Export(ctx, outcome.Event.ID); err != nil {
			t.Fatal("export policy", err)
		}
		if len(sink) == 0 {
			t.Fatal("no durable export")
		}
		beforeValidation, _ := store.GetBucketPolicy(ctx, f.Bucket.Name, f.Bucket.ID, 0)
		for _, tc := range []struct {
			name        string
			edit        func(*s.BucketPolicyUpdate)
			want        error
			noAdmission bool
		}{
			{"candidate revision", func(r *s.BucketPolicyUpdate) { r.Candidate.Revision = 3 }, s.ErrInvalid, false},
			{"candidate version", func(r *s.BucketPolicyUpdate) { r.Candidate.Version = 0 }, s.ErrInvalid, false},
			{"stale writer", func(r *s.BucketPolicyUpdate) { r.Writer.Epoch++ }, s.ErrFenced, false},
			{"wrong incarnation", func(r *s.BucketPolicyUpdate) { r.BucketID = "old-incarnation" }, s.ErrConflict, false},
			{"unadmitted noop", func(*s.BucketPolicyUpdate) {}, s.ErrFenced, true},
		} {
			ctx, m := mutation(tc.name)
			if tc.noAdmission {
				ctx = context.Background()
			}
			r := s.BucketPolicyUpdate{Bucket: f.Bucket.Name, BucketID: f.Bucket.ID, CredentialID: "owner-key", Writer: f.Lease.Fence, ExpectedRevision: 3, Candidate: s.PolicySnapshot{Version: 1, Document: bytes.Clone(beforeValidation.Document)}}
			tc.edit(&r)
			if _, err := store.PutBucketPolicy(ctx, r); !errors.Is(err, tc.want) {
				t.Fatal(tc.name, err)
			}
			if _, err := producer.GetMutation(ctx, m.MutationID); !errors.Is(err, s.ErrNotFound) {
				t.Fatal("rejected update published", tc.name, err)
			}
		}
		if after, _ := store.GetBucketPolicy(context.Background(), f.Bucket.Name, f.Bucket.ID, 0); !reflect.DeepEqual(beforeValidation, after) {
			t.Fatal("rejection mutated policy")
		}
		// Backend validation and scope are mandatory even without config helpers.
		for _, bad := range []string{`{"cache":{"admit_on_write":true}}`, `{"cache":{"admit_on_write":true,"readahead_chunks":65}}`, `{"cache":{"admit_on_write":true,"admit_on_write":false,"readahead_chunks":4}}`, `{"cache":{"admit_on_write":true,"readahead_chunks":4},"tiering":{}}`} {
			ctx, m = mutation("invalid-" + bad)
			_, e := store.PutBucketPolicy(ctx, s.BucketPolicyUpdate{Bucket: f.Bucket.Name, BucketID: f.Bucket.ID, CredentialID: "owner-key", Writer: f.Lease.Fence, ExpectedRevision: 3, Candidate: s.PolicySnapshot{Version: 1, Document: []byte(bad)}})
			if !errors.Is(e, s.ErrInvalid) {
				t.Fatal("backend accepted", bad, e)
			}
			if _, e := producer.GetMutation(ctx, m.MutationID); !errors.Is(e, s.ErrNotFound) {
				t.Fatal("invalid mutation published", e)
			}
		}
		ctx, _ = mutation("denied-owner-policy")
		if _, e := retried.CommitBucket(ctx, store, v, f.Bucket.Name, "writer-key", f.Lease.Fence, p); !errors.Is(e, s.ErrAccessDenied) {
			t.Fatal("grantee changed policy", e)
		}
		// Bucket identity/domain is immutable under cache updates.
		b, err := h.Store.GetBucket(ctx, f.Bucket.Name)
		if err != nil || !reflect.DeepEqual(b, f.Bucket) {
			t.Fatal("bucket identity changed", b, err)
		}
		b.ID = "recreated-bucket"
		h.SetBucket(b)
		if _, e := store.GetBucketPolicy(ctx, b.Name, f.Bucket.ID, 1); !errors.Is(e, s.ErrConflict) {
			t.Fatal("old incarnation read", e)
		}
		if _, e := store.GetBucketPolicy(ctx, b.Name, b.ID, 0); !errors.Is(e, s.ErrNotFound) {
			t.Fatal("policy inherited", e)
		}
	})
}
