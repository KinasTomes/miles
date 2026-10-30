package contracttest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/KinasTomes/miles/internal/manifest"
	shared "github.com/KinasTomes/miles/internal/storage/contracttest"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/recovery"
	s "github.com/KinasTomes/miles/internal/storage"
)

type producerSink map[string][]byte

func (m producerSink) Create(_ context.Context, key string, b []byte) error {
	if old, ok := m[key]; ok && !bytes.Equal(old, b) {
		return s.ErrConflict
	}
	m[key] = bytes.Clone(b)
	return nil
}
func (m producerSink) Read(_ context.Context, key string) ([]byte, error) {
	b, ok := m[key]
	if !ok {
		return nil, s.ErrNotFound
	}
	return bytes.Clone(b), nil
}

func RunProducers(t *testing.T, factory MultipartFactory) {
	t.Run("control-matrix-retry-and-export", func(t *testing.T) {
		ctx := context.Background()
		f := NewFixture()
		f.Quota.Limit = 1 << 20
		for i := range f.Packs {
			f.Packs[i].StateRevision = 1
		}
		h := factory(t, f)
		h.SetStrict(true)
		if _, _, err := h.Metadata.PutChunkIfAbsent(ctx, f.Chunk("default"), f.Lease.Fence); !errors.Is(err, s.ErrFenced) {
			t.Fatal("strict canonical bypass", err)
		}
		if _, _, err := h.Metadata.CreateBucket(ctx, s.BucketCreate{CredentialID: "owner-key", UserID: "owner", Name: "missing-admission"}); !errors.Is(err, s.ErrFenced) {
			t.Fatal("strict bucket bypass", err)
		}
		p := h.Metadata.(s.ProducerStore)
		makeContext := func(label string) s.MutationContext {
			op := fixtureOpID(label)
			return s.MutationContext{HistoryID: fixtureHistoryID, LogicalOpID: op, MutationID: op, Writer: f.Lease.Fence}
		}
		requests := []s.ControlMutation{
			{Entity: "Users", ID: "user-æµ·", User: s.User{ID: "user-æµ·", Active: true, DefaultDomain: "default"}, SetQuota: true, QuotaLimit: 32},
			{Entity: "Buckets", ID: "new-bucket", Bucket: s.Bucket{Name: "new-bucket", ID: "bucket-new", OwnerID: "user-æµ·", Domain: "default", Active: true}},
			{Entity: "BucketGrants", ParentID: "bucket-new", ID: "writer", Bucket: s.Bucket{Name: "new-bucket"}, Grant: s.Grant{BucketID: "bucket-new", UserID: "writer", Write: true}},
			{Entity: "Credentials", ID: "new-key", Credential: s.Credential{ID: "new-key", UserID: "user-æµ·", Active: true}, Security: s.CredentialSecurity{Version: 1, KeyID: "kms-key", Ciphertext: bytes.Repeat([]byte{42}, 64)}},
			{Entity: "AdminTokens", ID: "new-token", Token: s.AdminToken{ID: "new-token", UserID: "user-æµ·", Scope: "system-admin", Hash: bytes.Repeat([]byte{9}, 32), Active: true, ExpiresAt: f.Now.Add(time.Hour)}},
		}
		sink := producerSink{}
		for i, r := range requests {
			r.Context = makeContext(fmt.Sprint("control-", i))
			r.CredentialID = "admin-key"
			out, err := p.MutateControl(ctx, r)
			if err != nil {
				t.Fatal(r.Entity, err)
			}
			again, err := p.MutateControl(ctx, r)
			if err != nil || again.Event.ID != out.Event.ID || !bytes.Equal(again.Value, out.Value) {
				t.Fatal("retry changed event/result", again, err)
			}
			if err := (recovery.Exporter{Metadata: h.Metadata.(s.OutboxExportStore), Payloads: p, Objects: sink}).Export(ctx, out.Event.ID); err != nil {
				t.Fatal("real producer export", r.Entity, err)
			}
			if _, err := h.Metadata.(s.OutboxExportStore).GetOutboxExport(ctx, out.Event.ID); err != nil {
				t.Fatal(err)
			}
			changed := r
			changed.ID += "-different"
			if _, err := p.MutateControl(ctx, changed); !errors.Is(err, s.ErrConflict) {
				t.Fatal("mutation ID reused across identity", err)
			}
			denied := r
			denied.Context = makeContext(fmt.Sprint("denied-", i))
			denied.CredentialID = "writer-key"
			if _, err := p.MutateControl(ctx, denied); !errors.Is(err, s.ErrAccessDenied) {
				t.Fatal("non-admin mutation", err)
			}
		}
		ownerGrant := s.ControlMutation{Context: makeContext("owner-grant"), CredentialID: "admin-key", Entity: "BucketGrants", ParentID: "bucket-new", ID: "user-æµ·", Bucket: s.Bucket{Name: "new-bucket"}, Grant: s.Grant{BucketID: "bucket-new", UserID: "user-æµ·", Read: true}}
		if _, err := p.MutateControl(ctx, ownerGrant); !errors.Is(err, s.ErrInvalid) {
			t.Fatal("grant to bucket owner", err)
		}
		reissued := requests[4]
		reissued.Context, reissued.CredentialID, reissued.ExpectedRevision = makeContext("token-reissue"), "admin-key", 1
		reissued.Token.IssuedAt = f.Now
		if _, err := p.MutateControl(ctx, reissued); !errors.Is(err, s.ErrInvalid) {
			t.Fatal("token issued_at changed", err)
		}
		elevated := requests[3]
		elevated.Context, elevated.CredentialID, elevated.ExpectedRevision = makeContext("credential-elevate"), "admin-key", 1
		elevated.Credential.SystemAdmin = true
		if _, err := p.MutateControl(ctx, elevated); !errors.Is(err, s.ErrInvalid) {
			t.Fatal("credential flags changed", err)
		}
		anonymous := requests[0]
		anonymous.Context, anonymous.ExpectedRevision = makeContext("no-actor"), 1
		if _, err := p.MutateControl(ctx, anonymous); !errors.Is(err, s.ErrInvalid) {
			t.Fatal("mutation without actor", err)
		}
		for i, r := range requests {
			r.Context = makeContext(fmt.Sprint("control-delete-", i))
			r.CredentialID = "admin-key"
			r.ExpectedRevision = 1
			r.Deleted = true
			out, err := p.MutateControl(ctx, r)
			if err != nil {
				t.Fatal("delete", r.Entity, err)
			}
			if out.Event.Action != "entity_delete" || out.Event.Revision != 2 {
				t.Fatal(out)
			}
			if r.Entity == "Credentials" || r.Entity == "AdminTokens" {
				resurrect := r
				resurrect.Deleted = false
				resurrect.ExpectedRevision = 2
				resurrect.Context = makeContext(fmt.Sprint("resurrect-", i))
				if _, err := p.MutateControl(ctx, resurrect); !errors.Is(err, s.ErrInvalid) {
					t.Fatal("revoked identity resurrected", r.Entity, err)
				}
			}
			if err := (recovery.Exporter{Metadata: h.Metadata.(s.OutboxExportStore), Payloads: p, Objects: sink}).Export(ctx, out.Event.ID); err != nil {
				t.Fatal(err)
			}
		}
		c := makeContext("location-parent")
		live := s.WithMutation(ctx, c)
		chunk := f.Chunk("default")
		row, inserted, err := h.Metadata.PutChunkIfAbsent(live, chunk, c.Writer)
		if err != nil || !inserted {
			t.Fatal(row, inserted, err)
		}
		child := s.ChildMutation(c, "Chunks", chunk.Key.Domain+"#"+chunk.Key.Hash, "location_set")
		out, err := p.GetMutation(ctx, child.MutationID)
		if err != nil || out.Event.OpID == c.LogicalOpID {
			t.Fatal("child binding missing", out, err)
		}
		if err := (recovery.Exporter{Metadata: h.Metadata.(s.OutboxExportStore), Payloads: p, Objects: sink}).Export(ctx, out.Event.ID); err != nil {
			t.Fatal(err)
		}
		body := sink["journal/"+fixtureHistoryID+"/0/"+out.Event.ID+"/descriptor.json"]
		var descriptor recovery.Descriptor
		if err := recovery.Decode(body, &descriptor); err != nil || descriptor.Logical != c.LogicalOpID {
			t.Fatal("logical parent lost during export", descriptor, err)
		}
		c = makeContext("location-cas")
		live = s.WithMutation(ctx, c)
		next := chunk.Location
		next.Pack = f.Packs[1].Ref
		for i := 0; i < 2; i++ {
			row, err = h.Metadata.CompareAndSwapChunkLocation(live, chunk.Key, 1, next, c.Writer)
			if err != nil || row.LocationRevision != 2 {
				t.Fatal("CAS retry", row, err)
			}
		}
		c = makeContext("bucket-create")
		live = s.WithMutation(ctx, c)
		created, won, err := h.Metadata.CreateBucket(live, s.BucketCreate{CredentialID: "owner-key", UserID: "owner", Name: "produced-bucket"})
		if err != nil || !won {
			t.Fatal(created, won, err)
		}
		again, _, err := h.Metadata.CreateBucket(live, s.BucketCreate{CredentialID: "owner-key", UserID: "owner", Name: "produced-bucket"})
		if err != nil || again.ID != created.ID {
			t.Fatal(again, err)
		}
		if _, err := p.GetMutation(ctx, c.MutationID); err != nil {
			t.Fatal("bucket producer bypass", err)
		}
		c = makeContext("policy-put")
		live = s.WithMutation(ctx, c)
		eventID, _ := s.EventID(c.HistoryID, c.MutationID, "SystemConfig", "entity_put")
		event := s.OutboxEvent{Version: 1, ID: eventID, HistoryID: c.HistoryID, Entity: "SystemConfig", EntityID: "global-policy", Action: "entity_put", Revision: 1, OpID: c.MutationID, PayloadPointer: "backend", PayloadSHA256: fmt.Sprintf("%064x", 1)}
		for i := 0; i < 2; i++ {
			v, err := h.Metadata.PutPolicy(live, s.PolicyUpdate{Candidate: s.PolicySnapshot{Version: 1, Document: []byte(`{"enabled":true}`)}, CredentialID: "admin-key", Writer: c.Writer, Event: &event})
			if err != nil || v.Revision != 1 {
				t.Fatal("policy retry", v, err)
			}
		}
		policy, err := p.GetMutation(ctx, c.MutationID)
		if err != nil {
			t.Fatal(err)
		}
		if err := (recovery.Exporter{Metadata: h.Metadata.(s.OutboxExportStore), Payloads: p, Objects: sink}).Export(ctx, policy.Event.ID); err != nil {
			t.Fatal(err)
		}
		var decoded s.PolicySnapshot
		if json.Unmarshal(policy.Value, &decoded) != nil || decoded.Revision != 1 {
			t.Fatal("policy projection lost")
		}
		c = makeContext("policy-noop")
		live = s.WithMutation(ctx, c)
		noopRequest := s.PolicyUpdate{ExpectedRevision: 1, Candidate: s.PolicySnapshot{Version: 1, Document: []byte(`{"enabled":true}`)}, CredentialID: "admin-key", Writer: c.Writer}
		if v, err := h.Metadata.PutPolicy(live, noopRequest); err != nil || v.Revision != 1 {
			t.Fatal(v, err)
		}
		noop, err := p.GetMutation(ctx, c.MutationID)
		if err != nil || !noop.NoOp || noop.Event.ID != "" {
			t.Fatal("no-op emitted event or lost terminal binding", noop, err)
		}
		noopRequest.Candidate.Document = []byte(`{"enabled":false}`)
		if _, err := h.Metadata.PutPolicy(live, noopRequest); !errors.Is(err, s.ErrConflict) {
			t.Fatal("no-op identity reused for different input", err)
		}

		// A real object producer must serialize immutable binary dependencies,
		// including a page larger than the journal's metadata item bound.
		c = makeContext("object-producer")
		c.Object = s.ObjectKey{Bucket: created.Name, BucketID: created.ID, Key: "海/+/%//data"}
		live = s.WithMutation(ctx, c)
		const refs = 11000
		position := 0
		if err := manifest.Paginate(1, "event-large-manifest", "default", func() (s.ChunkRef, error) {
			if position == refs {
				return s.ChunkRef{}, io.EOF
			}
			r := s.ChunkRef{Key: chunk.Key, RawBytes: 4, LogicalOffset: int64(position * 4)}
			position++
			return r, nil
		}, func(page s.ManifestPage) error {
			return h.Metadata.WriteManifestPages(live, []s.ManifestPage{page}, c.Writer)
		}); err != nil {
			t.Fatal(err)
		}
		r := f.Request("object-producer", 0)
		r.CredentialID = "owner-key"
		r.UserID = "owner"
		r.Producer = &c
		r.Head.Object = c.Object
		r.Head.ManifestID = "event-large-manifest"
		r.Head.Size = refs * 4
		r.Head.Metadata = map[string]string{"x-amz-meta-title": "海<>&"}
		r.Event.Object = c.Object
		if err := h.Metadata.ReserveLogicalQuota(live, s.Reservation{Version: 1, ID: r.ReservationID, OpID: r.Head.OpID, OwnerID: "owner", Object: c.Object, Bytes: r.Head.Size, StagedBytes: r.Head.Size, Revision: 1, State: s.ReservationActive, Deadline: f.Now.Add(time.Minute), Writer: c.Writer}); err != nil {
			t.Fatal(err)
		}
		committed, err := h.Metadata.CommitObject(live, r)
		if err != nil {
			t.Fatal("atomic object producer", err)
		}
		if committed.Head.PageCount != 2 {
			t.Fatal(committed.Head)
		}
		outbox, err := h.Metadata.GetOutbox(ctx, r.Event.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := (recovery.Exporter{Metadata: h.Metadata.(s.OutboxExportStore), Payloads: p, Objects: sink}).Export(ctx, outbox.ID); err != nil {
			t.Fatal("binary dependency export", err)
		}
		body = sink["journal/"+fixtureHistoryID+"/0/"+outbox.ID+"/descriptor.json"]
		var exported recovery.Descriptor
		if err := recovery.Decode(body, &exported); err != nil || len(exported.Dependencies) != 1 || exported.Source.Object != c.Object {
			t.Fatal(exported, err)
		}
		if err := recovery.VerifyAttachment(ctx, sink, exported.Dependencies[0], outbox); err != nil {
			t.Fatal("export depends on live metadata", err)
		}
		for key, original := range sink {
			if strings.HasSuffix(key, ".bin") {
				delete(sink, key)
				if err := recovery.VerifyAttachment(ctx, sink, exported.Dependencies[0], outbox); err == nil {
					t.Fatal("missing binary dependency accepted")
				}
				sink[key] = original
				break
			}
		}
		deletion := s.ControlMutation{Context: makeContext("nonempty-delete"), CredentialID: "admin-key", Entity: "Buckets", ID: created.Name, ExpectedRevision: 1, Deleted: true, Bucket: created}
		if _, err := p.MutateControl(ctx, deletion); !errors.Is(err, s.ErrConflict) {
			t.Fatal("namespace emptiness guard bypassed", err)
		}
	})
}

type MultipartFactory = shared.MultipartFactory

func NewFixture() shared.Fixture { return shared.NewFixture() }

const fixtureHistoryID = "11111111111111111111111111111111"

func fixtureOpID(label string) string {
	sum := sha256.Sum256([]byte(label))
	return hex.EncodeToString(sum[:16])
}
