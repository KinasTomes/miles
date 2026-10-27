package config_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	c "github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/storage"
	"github.com/KinasTomes/miles/internal/storage/fake"
)

// Reference-store compatibility only, not production persistence, distributed
// atomicity, or a Q6 fencing proof. No storage port/model is changed here.
func TestPolicyCandidateSharedPortCompatibility(t *testing.T) {
	v := validator(t)
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	fence := storage.WriterFence{LeaseID: "writer", ServiceIncarnation: "incarnation", Epoch: 1}
	store := fake.NewMetadata(fake.Seed{
		Now:         func() time.Time { return now },
		Users:       []storage.User{{ID: "admin", Active: true}},
		Credentials: []storage.Credential{{ID: "key", UserID: "admin", Active: true, SystemAdmin: true}},
		Writers:     []storage.WriterLease{{Fence: fence, ExpiresAt: now.Add(time.Hour)}},
	})
	ctx := context.Background()
	event := func(id string, revision uint64) *storage.OutboxEvent {
		opID := fmt.Sprintf("%x", sha256.Sum256([]byte(id)))[:32]
		historyID := strings.Repeat("1", 32)
		eventID, _ := storage.EventID(historyID, opID, "SystemConfig", "entity_put")
		return &storage.OutboxEvent{Version: storage.ModelVersion, ID: eventID, HistoryID: historyID, Entity: "SystemConfig", Action: "entity_put", EntityID: "global-policy", Revision: revision, OpID: opID, PayloadPointer: "payload/" + id, PayloadSHA256: strings.Repeat("a", 64)}
	}
	bootstrap, err := globalSnapshot(t, v, 1).StorageSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	bootstrap.Revision = 0 // Existing INTERNAL empty-store bootstrap rule only.
	first, err := store.PutPolicy(ctx, storage.PolicyUpdate{Candidate: bootstrap, CredentialID: "key", Writer: fence, Event: event("bootstrap", 1)})
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := v.DecodeGlobal(first)
	if err != nil {
		t.Fatal(err)
	}
	noop, err := v.MergeGlobal(pinned, []byte(`{"expected_revision":1,"compression":{"min_saving_ratio":0.1000}}`), 0)
	if err != nil || noop.Changed() {
		t.Fatal(noop, err)
	}
	noopCandidate, err := noop.Candidate()
	if err != nil {
		t.Fatal(err)
	}
	same, err := store.PutPolicy(ctx, storage.PolicyUpdate{Candidate: noopCandidate, ExpectedRevision: noop.ExpectedRevision(), CredentialID: "key", Writer: fence})
	if err != nil || same.Revision != 1 {
		t.Fatal("no-op changed revision/event requirement", same, err)
	}
	change, err := v.MergeGlobal(pinned, []byte(`{"expected_revision":1,"compression":{"min_saving_ratio":0.1000000000000000001}}`), 0)
	if err != nil || !change.Changed() {
		t.Fatal("precision lost", change, err)
	}
	candidate, err := change.Candidate()
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.PutPolicy(ctx, storage.PolicyUpdate{Candidate: candidate, ExpectedRevision: change.ExpectedRevision(), CredentialID: "key", Writer: fence, Event: event("change", 2)})
	if err != nil || second.Revision != 2 {
		t.Fatal(second, err)
	}
	// A locally valid no-op is not permission to bypass the authoritative CAS.
	if _, err := store.PutPolicy(ctx, storage.PolicyUpdate{Candidate: noopCandidate, ExpectedRevision: 1, CredentialID: "key", Writer: fence}); !errors.Is(err, storage.ErrConflict) {
		t.Fatal(err)
	}
	historical, lookupErr := store.GetPolicy(ctx, 1)
	old, err := v.ResolveGlobal(1, historical, lookupErr)
	if err != nil || old.Policy() != pinned.Policy() {
		t.Fatal("history mutated", old, err)
	}
	missing, lookupErr := store.GetPolicy(ctx, 99)
	if _, err := v.ResolveGlobal(99, missing, lookupErr); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("missing history replaced by current", err)
	}
}

// CFG-R01: overlapping proposal lifetimes, followed by an explicitly ordered
// A-wins CAS interleaving. This exercises the existing reference store, not a
// config-owned CAS or evidence of production/distributed atomicity.
func TestConcurrentPolicyProposalsPreserveCASAndHistory(t *testing.T) {
	v := validator(t)
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	fence := storage.WriterFence{LeaseID: "writer", ServiceIncarnation: "incarnation", Epoch: 1}
	store := fake.NewMetadata(fake.Seed{
		Now:         func() time.Time { return now },
		Users:       []storage.User{{ID: "admin", Active: true}},
		Credentials: []storage.Credential{{ID: "key", UserID: "admin", Active: true, SystemAdmin: true}},
		Writers:     []storage.WriterLease{{Fence: fence, ExpiresAt: now.Add(time.Hour)}},
	})
	ctx := context.Background()
	event := func(id string, revision uint64) *storage.OutboxEvent {
		opID := fmt.Sprintf("%x", sha256.Sum256([]byte(id)))[:32]
		historyID := strings.Repeat("1", 32)
		eventID, _ := storage.EventID(historyID, opID, "SystemConfig", "entity_put")
		return &storage.OutboxEvent{Version: storage.ModelVersion, ID: eventID, HistoryID: historyID, Entity: "SystemConfig", Action: "entity_put", EntityID: "global-policy", Revision: revision, OpID: opID, PayloadPointer: "payload/" + id, PayloadSHA256: strings.Repeat("a", 64)}
	}
	// Reach actual R3 through shared CAS: R1 level1, R2 level7, R3 W1 default3.
	// Revision0 is used only for the fake's existing empty-store bootstrap.
	for i, level := range []int64{1, 7, 3} {
		revision := uint64(i + 1)
		policy := c.DefaultGlobalPolicy()
		policy.Compression.Level = level
		snapshot, err := v.NewGlobalSnapshot(revision, policy)
		if err != nil {
			t.Fatal(err)
		}
		candidate, err := snapshot.StorageSnapshot()
		if err != nil {
			t.Fatal(err)
		}
		candidate.Revision = 0
		result, err := store.PutPolicy(ctx, storage.PolicyUpdate{Candidate: candidate, ExpectedRevision: revision - 1, CredentialID: "key", Writer: fence, Event: event(fmt.Sprintf("setup-%d", revision), revision)})
		if err != nil || result.Revision != revision {
			t.Fatal("R3 setup", result, err)
		}
	}
	readSnapshot := func(revision uint64) (storage.PolicySnapshot, c.GlobalSnapshot) {
		t.Helper()
		record, lookupErr := store.GetPolicy(ctx, revision)
		snapshot, err := v.ResolveGlobal(revision, record, lookupErr)
		if err != nil {
			t.Fatal(err)
		}
		return record, snapshot
	}
	before, clientA := readSnapshot(0)
	_, clientB := readSnapshot(0)
	if before.Revision != 3 || clientA.Revision() != 3 || clientB.Revision() != 3 {
		t.Fatal("clients did not read R3")
	}
	original := clientA.Policy()
	proposalA, err := v.MergeGlobal(clientA, []byte(`{"expected_revision":3,"compression":{"enabled":false}}`), 0)
	if err != nil {
		t.Fatal(err)
	}
	proposalB, err := v.MergeGlobal(clientB, []byte(`{"expected_revision":3,"tiering":{"cold_after_days":0}}`), 0)
	if err != nil {
		t.Fatal(err)
	}
	if !proposalA.Changed() || !proposalB.Changed() || proposalA.ExpectedRevision() != 3 || proposalB.ExpectedRevision() != 3 || proposalA.Policy() == proposalB.Policy() {
		t.Fatal("proposals are not distinct changes pinned to R3")
	}
	if clientA.Policy() != original || clientB.Policy() != original || clientA.Revision() != 3 || clientB.Revision() != 3 {
		t.Fatal("proposal preparation mutated client snapshots")
	}
	authoritative, _ := readSnapshot(0)
	if authoritative.Revision != 3 || !bytes.Equal(authoritative.Document, before.Document) {
		t.Fatal("preparation wrote authoritative state")
	}
	for _, id := range []string{"client-a", "client-b"} {
		if _, err := store.GetOutbox(ctx, id); !errors.Is(err, storage.ErrNotFound) {
			t.Fatal("preparation published an event", id, err)
		}
	}
	if _, err := store.GetPolicy(ctx, 4); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("preparation created history", err)
	}
	candidateA, err := proposalA.Candidate()
	if err != nil {
		t.Fatal(err)
	}
	candidateB, err := proposalB.Candidate()
	if err != nil {
		t.Fatal(err)
	}
	if candidateA.Revision != 0 || candidateB.Revision != 0 {
		t.Fatal("config assigned a committed revision")
	}
	eventA, eventB := event("client-a", 4), event("client-b", 4)
	winner, err := store.PutPolicy(ctx, storage.PolicyUpdate{Candidate: candidateA, ExpectedRevision: proposalA.ExpectedRevision(), CredentialID: "key", Writer: fence, Event: eventA})
	if err != nil || winner.Revision != 4 {
		t.Fatal("A did not win R4", winner, err)
	}
	if _, err := store.PutPolicy(ctx, storage.PolicyUpdate{Candidate: candidateB, ExpectedRevision: proposalB.ExpectedRevision(), CredentialID: "key", Writer: fence, Event: eventB}); !errors.Is(err, storage.ErrConflict) {
		t.Fatal("B's stale CAS was accepted", err)
	}
	current, effective := readSnapshot(0)
	if current.Revision != 4 || effective.Policy() != proposalA.Policy() || !bytes.Equal(current.Document, candidateA.Document) {
		t.Fatal("lost/partial update after stale B", current, effective)
	}
	historyR3, old := readSnapshot(3)
	if old.Policy() != original || !bytes.Equal(historyR3.Document, before.Document) {
		t.Fatal("R3 history overwritten")
	}
	historyR4, _ := readSnapshot(4)
	if !bytes.Equal(historyR4.Document, winner.Document) {
		t.Fatal("R4 history differs from winner")
	}
	if got, err := store.GetOutbox(ctx, eventA.ID); err != nil || got != *eventA {
		t.Fatal("winner event missing/partial", got, err)
	}
	if _, err := store.GetOutbox(ctx, eventB.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("loser event partially published", err)
	}
	if _, err := store.GetPolicy(ctx, 5); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("loser created extra history", err)
	}
}

func TestPolicyBoundaryAdapters(t *testing.T) {
	limits := testPolicyLimits()
	limits.MaxJSONBytes = 10
	v, err := c.NewPolicyValidator(limits)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.ValidatePolicy(c.DefaultGlobalPolicy()); !errors.Is(err, c.ErrPolicyInvalid) {
		t.Fatal("complete document budget bypass", err)
	}
	if err := v.ValidateCache(c.DefaultCachePolicy()); !errors.Is(err, c.ErrPolicyInvalid) {
		t.Fatal("complete bucket document budget bypass", err)
	}
	v = validator(t)
	if _, err := v.NewBucketSnapshot("", 1, c.DefaultCachePolicy()); !errors.Is(err, c.ErrPolicyInvalid) {
		t.Fatal(err)
	}
	if _, err := v.MergeGlobal(c.GlobalSnapshot{}, []byte(`{}`), 1); !errors.Is(err, c.ErrPolicyInvalid) {
		t.Fatal(err)
	}
	if _, err := v.MergeBucket(c.BucketSnapshot{}, []byte(`{"expected_bucket_id":"bucket-one"}`), 1); !errors.Is(err, c.ErrPolicyInvalid) {
		t.Fatal(err)
	}
	if _, err := v.DecodeBucket("bucket-one", 1, []byte(`{"cache":{"admit_on_write":1,"readahead_chunks":4}}`)); !errors.Is(err, c.ErrPolicyInvalid) {
		t.Fatal(err)
	}
	if _, err := v.MergeBucket(bucketSnapshot(t, v, 1), []byte(`{"expected_bucket_id":"bucket-one","cache":{"admit_on_write":"false"}}`), 1); !errors.Is(err, c.ErrPolicyInvalid) {
		t.Fatal(err)
	}
	var invalid c.Decimal
	if _, err := invalid.MarshalJSON(); !errors.Is(err, c.ErrPolicyInvalid) {
		t.Fatal(err)
	}
	if invalid.String() != "" {
		t.Fatal("invalid Decimal masquerades as zero")
	}
}
