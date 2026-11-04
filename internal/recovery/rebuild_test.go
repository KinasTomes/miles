package recovery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/engine"
	"github.com/KinasTomes/miles/internal/lifecycle"
	"github.com/KinasTomes/miles/internal/manifest"
	"github.com/KinasTomes/miles/internal/metadata/operations"
	"github.com/KinasTomes/miles/internal/pack"
	"github.com/KinasTomes/miles/internal/recovery/format"
	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/KinasTomes/miles/internal/storage/fake"
)

func liveCheckpointFixture(t *testing.T) (Checkpointer, AttachmentReference, *fake.Packs) {
	t.Helper()
	w, base := checkpointFixture(t)
	now := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	key, e := engine.HashChunk("default", []byte("data"))
	if e != nil {
		t.Fatal(e)
	}
	ref := s.PackRef{ID: "pack", Generation: "generation"}
	builder, _ := pack.NewBuilder(ref)
	if e = builder.Add(pack.Blob{Key: key, RawLength: 4, Codec: "raw", Stored: []byte("data")}); e != nil {
		t.Fatal(e)
	}
	body, side, entries, e := builder.Finish()
	if e != nil {
		t.Fatal(e)
	}
	p := s.PackInfo{Version: 1, Ref: ref, Size: int64(len(body)), Class: s.Standard, State: "Warm", StateRevision: 1}
	physical := fake.NewPacks(func() time.Time { return now })
	if e = physical.PutPack(t.Context(), p, bytes.NewReader(body)); e != nil {
		t.Fatal(e)
	}
	if e = physical.PutSidecar(t.Context(), ref, int64(len(side)), bytes.NewReader(side)); e != nil {
		t.Fatal(e)
	}
	page := s.ManifestPage{Version: 1, ManifestID: "manifest", Domain: "default", Chunks: []s.ChunkRef{{Key: key, RawBytes: 4}}}
	raw, e := manifest.Encode(1, page)
	if e != nil {
		t.Fatal(e)
	}
	attachment, e := Encode(format.Attachment{Version: 1, Kind: 1, Owner: "manifest", Domain: "default", Length: 4, Pages: [][]format.PageReference{{{Length: len(raw), SHA256: digest(raw)}}}})
	if e != nil {
		t.Fatal(e)
	}
	id, _ := s.EventID(w.Catalog.History, strings.Repeat("0", 31)+"1", "Users", "entity_put")
	event := s.OutboxEvent{HistoryID: w.Catalog.History, Epoch: 0, ID: id}
	exported, e := (Exporter{Objects: w.Objects, Payloads: memoryInputs{"attachment-input": attachment, "attachment/1/manifest/0": raw}}).exportAttachment(t.Context(), event, AttachmentReference{Key: "attachment-input", Reference: refBytes(attachment)}, 0)
	if e != nil {
		t.Fatal(e)
	}
	deps, e := Encode([]AttachmentReference{exported})
	if e != nil {
		t.Fatal(e)
	}
	w.Objects.(*memoryObjects).objects["fixture/dependencies/0/0"] = deps
	head := s.ObjectHead{Version: 1, Object: s.ObjectKey{Bucket: "bucket", BucketID: strings.Repeat("4", 32), Key: "key"}, Domain: "default", ManifestID: "manifest", Size: 4, Revision: 1, OpID: strings.Repeat("5", 32), PageCount: 1, PageIndex: []uint64{0}, ChunkCount: 1, ETag: strings.Repeat("a", 32), ETagKind: "md5", Metadata: map[string]string{}}
	headID, _ := json.Marshal(head.Object)
	rows := []Effect{}
	add := func(kind, id string, rev uint64, value any) {
		r, e := operations.Projection(kind, id, rev, value, false)
		if e != nil {
			t.Fatal(e)
		}
		rows = append(rows, r)
	}
	add("Users", "owner", 1, s.User{ID: "owner", Revision: 1, Active: true, DefaultDomain: "default"})
	validator, _ := config.NewPolicyValidator(config.DSGPolicyLimits())
	snapshot, e := validator.NewGlobalSnapshot(1, config.DefaultGlobalPolicy())
	if e != nil {
		t.Fatal(e)
	}
	policy, e := snapshot.StorageSnapshot()
	if e != nil {
		t.Fatal(e)
	}
	add("SystemConfig", "global-policy", 1, policy)
	add("Uploads", "unfinished", 1, s.MultipartUpload{Version: 1, ID: "unfinished", State: "Active", Revision: 1, Writer: s.WriterFence{LeaseID: "old", ServiceIncarnation: w.Catalog.Incarnation, Epoch: 1}, Object: head.Object, Owner: "owner"})
	add("UsersQuota", "owner", 1, struct {
		Owner string
		Limit int64
	}{"owner", 100})
	add("Buckets", "bucket", 1, s.Bucket{Name: "bucket", ID: head.Object.BucketID, OwnerID: "owner", Domain: "default", Active: true, Revision: 1})
	add("Credentials", "old-key", 1, struct {
		Credential s.Credential
		Security   s.CredentialSecurity
	}{s.Credential{ID: "old-key", UserID: "owner", Active: true, Revision: 1}, s.CredentialSecurity{Version: 1, KeyID: "kms", Ciphertext: make([]byte, 16)}})
	add("AdminTokens", "old-token", 1, s.AdminToken{ID: "old-token", UserID: "owner", Scope: s.TokenScopeSystem, Active: true, Revision: 1, Hash: make([]byte, 32), IssuedAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour)})
	add("Packs", "pack/generation", 1, operations.PackStateProjection(p))
	add("Chunks", key.Domain+"#"+key.Hash, 1, s.Chunk{Version: 1, Key: key, RawBytes: 4, Location: entries[0].Location(ref), LocationRevision: 1})
	add("Objects", string(headID), 1, head)
	add("CompletedReceipt", "upload", 1, s.CompletionReceipt{Version: 1, UploadID: "upload", OpID: head.OpID, Initiator: "owner", Object: head.Object, Head: head, CommittedAt: now, ExpiresAt: now.Add(7 * 24 * time.Hour)})
	fixtureSeal(t, w, 0, w.Catalog.Genesis.SHA256, rows)
	cp, e := w.Build(t.Context(), base, 0)
	if e != nil {
		t.Fatal(e)
	}
	return w, cp, physical
}
func TestRebuildNamespaceCountersSecurityAndColdPayload(t *testing.T) {
	w, root, physical := liveCheckpointFixture(t)
	rebuilt, e := w.Rebuild(t.Context(), root)
	if e != nil {
		t.Fatal(e)
	}
	if len(rebuilt.Live) != 1 || len(rebuilt.Manifests["manifest"]) != 1 {
		t.Fatal(rebuilt)
	}
	var q s.LogicalQuota
	foundReceipt := false
	for _, r := range rebuilt.Records {
		switch r.Kind {
		case "upload-record":
			var v s.MultipartUpload
			_ = json.Unmarshal(r.Body, &v)
			if v.State != "Aborted" || v.Writer != (s.WriterFence{}) || v.Publishers != 0 {
				t.Fatal("unfinished upload resurrected", v)
			}
		case "quotas":
			if json.Unmarshal(r.Body, &q) != nil {
				t.Fatal("quota")
			}
		case "credentials":
			var v s.Credential
			_ = json.Unmarshal(r.Body, &v)
			if v.Active {
				t.Fatal("credential active")
			}
		case "admin-tokens":
			var v s.AdminToken
			_ = json.Unmarshal(r.Body, &v)
			if v.Active {
				t.Fatal("token active")
			}
		case "full-receipt":
			var v s.CompletionReceipt
			_ = json.Unmarshal(r.Body, &v)
			if !v.ExpiresAt.Equal(time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)) {
				t.Fatal("receipt TTL reset")
			}
			foundReceipt = true
		}
	}
	if q.Used != 4 || q.Reserved != 0 || q.Staged != 0 || q.ActiveUploads != 0 || !foundReceipt {
		t.Fatal(q, foundReceipt)
	}
	if e = rebuilt.VerifyPayload(t.Context(), physical); e != nil {
		t.Fatal(e)
	}
	for ref := range rebuilt.Packs {
		if e = physical.ChangeStorageClass(t.Context(), ref, s.GlacierIR); e != nil {
			t.Fatal(e)
		}
		if e = rebuilt.VerifyPayload(t.Context(), physical); e != nil {
			t.Fatal("IR unreadable", e)
		}
		if e = physical.ChangeStorageClass(t.Context(), ref, s.Glacier); e != nil {
			t.Fatal(e)
		}
		if e = rebuilt.VerifyPayload(t.Context(), physical); !errors.Is(e, s.ErrCold) {
			t.Fatal("archive admitted before restore", e)
		}
		if e = physical.Restore(t.Context(), ref, s.RestoreRequest{Days: 1, Tier: "Standard"}); e != nil {
			t.Fatal(e)
		}
		if e = rebuilt.VerifyPayload(t.Context(), physical); !errors.Is(e, s.ErrCold) {
			t.Fatal("acceptance implied readiness", e)
		}
		if e = physical.CompleteRestore(ref, time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)); e != nil {
			t.Fatal(e)
		}
		if e = rebuilt.VerifyPayload(t.Context(), physical); e != nil {
			t.Fatal(e)
		}
	}
	image, e := w.Load(t.Context(), root)
	if e != nil {
		t.Fatal(e)
	}
	for _, r := range image.Rows {
		kind, _, v, e := projection(r)
		if e != nil {
			t.Fatal(e)
		}
		if kind == "Credentials" {
			var c struct {
				Credential s.Credential
				Security   s.CredentialSecurity
			}
			_ = json.Unmarshal(v, &c)
			if !c.Credential.Active {
				t.Fatal("historical checkpoint changed")
			}
		}
	}
}

type memoryCatalog struct {
	head         s.RecoveryCatalogHead
	fail, denied bool
}

func (m *memoryCatalog) CheckRecoveryWorker(context.Context, s.WriterFence) error {
	if m.denied {
		return s.ErrFenced
	}
	return nil
}

func (m *memoryCatalog) GetRecoveryCatalog(context.Context, string) (s.RecoveryCatalogHead, error) {
	if m.head.Revision == 0 {
		return m.head, s.ErrNotFound
	}
	return m.head, nil
}
func (m *memoryCatalog) CompareRecoveryCatalog(_ context.Context, old, next s.RecoveryCatalogHead, _ s.WriterFence) error {
	if m.fail {
		m.fail = false
		return s.ErrUnavailable
	}
	if m.head == next {
		return nil
	}
	if m.head != old {
		return s.ErrConflict
	}
	m.head = next
	return nil
}

func TestRetentionKeepsProtectedCutsPinsAndImmutableAnchors(t *testing.T) {
	w, root, physical := liveCheckpointFixture(t)
	store := &memoryCatalog{}
	r := Retention{Checkpointer: w, Store: store, Packs: physical}
	if e := r.Publish(t.Context(), root); e != nil {
		t.Fatal(e)
	}
	image, e := w.Load(t.Context(), root)
	if e != nil {
		t.Fatal(e)
	}
	fixtureSeal(t, w, 1, image.Snapshot.Seal.Reference.SHA256)
	next, e := w.Build(t.Context(), root, 1)
	if e != nil {
		t.Fatal(e)
	}
	if e = r.Publish(t.Context(), next); e != nil {
		t.Fatal(e)
	}
	_, catalog, e := r.Read(t.Context())
	if e != nil || len(catalog.Cuts) != 2 {
		t.Fatal(catalog, e)
	}
	start := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	edge := start.Add(ProtectedWindow + RetentionBuffer)
	b := s.R1Bounds{Lower: edge, Upper: edge, Horizon: edge.Add(time.Hour)}
	if e = r.Prune(t.Context(), b); e != nil {
		t.Fatal(e)
	}
	_, catalog, e = r.Read(t.Context())
	if e != nil || len(catalog.Cuts) != 2 {
		t.Fatal("removed at boundary", catalog, e)
	}
	if e = r.Pin(t.Context(), 0, "restore-consumer", true); e != nil {
		t.Fatal(e)
	}
	b.Lower = start.Add(26 * time.Hour)
	b.Upper = b.Lower
	b.Horizon = b.Upper.Add(time.Hour)
	if e = r.Prune(t.Context(), b); e != nil {
		t.Fatal(e)
	}
	_, catalog, e = r.Read(t.Context())
	if e != nil || len(catalog.Cuts) != 1 || catalog.Cuts[0].Epoch != 0 {
		t.Fatal(catalog, e)
	}
	if _, e = w.Load(t.Context(), next); e != nil {
		t.Fatal("removed replay anchor", e)
	}
	if e = r.Pin(t.Context(), 0, "restore-consumer", false); e != nil {
		t.Fatal(e)
	}
	if e = r.Prune(t.Context(), b); e != nil {
		t.Fatal(e)
	}
	roots, e := r.ProtectedRoots(t.Context())
	if e != nil || len(roots) != 0 {
		t.Fatal(roots, e)
	}
	if _, e = w.Load(t.Context(), root); e != nil {
		t.Fatal("deleted immutable history", e)
	}
}

func TestRecoveryCatalogLostPointerACKAndDatabaseLoss(t *testing.T) {
	w, root, physical := liveCheckpointFixture(t)
	store := &memoryCatalog{fail: true}
	r := Retention{Checkpointer: w, Store: store, Packs: physical}
	if e := r.Publish(t.Context(), root); !errors.Is(e, s.ErrUnavailable) {
		t.Fatal(e)
	}
	if store.head.Revision != 0 {
		t.Fatal(store.head)
	}
	offline := r
	offline.Store = nil
	_, catalog, e := offline.Read(t.Context())
	if e != nil || len(catalog.Cuts) != 1 {
		t.Fatal(catalog, e)
	}
	if _, _, e = r.Read(t.Context()); e != nil || store.head.Revision != 1 {
		t.Fatal(store.head, e)
	}
	store.denied = true
	if e = r.Pin(t.Context(), 0, "stale-worker", true); !errors.Is(e, s.ErrFenced) {
		t.Fatal(e)
	}
	if _, e = w.Objects.Read(t.Context(), supportMarker(w.Catalog.History, 2)); !errors.Is(e, s.ErrNotFound) {
		t.Fatal("fenced marker publication", e)
	}
}

func TestRecoveryRejectsPITRAfterTargetAndMissingPayload(t *testing.T) {
	w, root, physical := liveCheckpointFixture(t)
	r := Retention{Checkpointer: w, Store: &memoryCatalog{}, Packs: physical}
	if e := r.Publish(t.Context(), root); e != nil {
		t.Fatal(e)
	}
	image, e := w.Load(t.Context(), root)
	if e != nil {
		t.Fatal(e)
	}
	fixtureSeal(t, w, 1, image.Snapshot.Seal.Reference.SHA256)
	later, e := w.Build(t.Context(), root, 1)
	if e != nil {
		t.Fatal(e)
	}
	start := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	request := RecoverRequest{Target: start.Add(30 * time.Second), Bounds: s.R1Bounds{Lower: start.Add(time.Hour), Upper: start.Add(time.Hour), Horizon: start.Add(2 * time.Hour)}, History: strings.Repeat("8", 32), Incarnation: strings.Repeat("9", 32)}
	installer := &memoryInstaller{}
	_, e = (Recoverer{Retention: r, Installer: installer, PITR: &fixturePITR{root: later}}).Recover(t.Context(), request)
	if !errors.Is(e, s.ErrConflict) || installer.finished || len(installer.records) != 0 {
		t.Fatal(installer, e)
	}
	for ref := range (func() Rebuilt {
		v, e := w.Rebuild(t.Context(), root)
		if e != nil {
			t.Fatal(e)
		}
		return v
	})().Packs {
		if e = physical.DeletePack(t.Context(), ref); e != nil {
			t.Fatal(e)
		}
	}
	_, e = (Recoverer{Retention: r, Installer: installer, PITR: &fixturePITR{err: s.ErrNotFound}}).Recover(t.Context(), request)
	if e == nil || installer.finished {
		t.Fatal("missing payload installed", installer, e)
	}
}

func TestRetentionRejectsChecksummedCheckpointWithMissingReceipt(t *testing.T) {
	w, root, physical := liveCheckpointFixture(t)
	image, e := w.Load(t.Context(), root)
	if e != nil {
		t.Fatal(e)
	}
	o := w.Objects.(*memoryObjects)
	cp := image.Snapshot
	for i, r := range cp.Pages {
		var page snapshotPage
		if Decode(o.objects[r.Key], &page) != nil {
			t.Fatal("page")
		}
		rows := []Effect{}
		for _, row := range page.Rows {
			if !strings.HasPrefix(row.Key, "CompletedReceipt/") {
				rows = append(rows, row)
			}
		}
		page.Rows = rows
		b, e := Encode(page)
		if e != nil {
			t.Fatal(e)
		}
		ref := AttachmentReference{Key: "recovery/checkpoints/forged/" + digest(b), Reference: ref(b)}
		o.objects[ref.Key] = b
		cp.Pages[i] = ref
	}
	body, e := Encode(cp)
	if e != nil {
		t.Fatal(e)
	}
	candidate := AttachmentReference{Key: "recovery/checkpoints/forged/state", Reference: ref(body)}
	o.objects[candidate.Key] = body
	r := Retention{Checkpointer: w, Store: &memoryCatalog{}, Packs: physical}
	if e = r.Publish(t.Context(), candidate); !errors.Is(e, s.ErrConflict) {
		t.Fatal("advertised incomplete state", e)
	}
	if _, e = o.Read(t.Context(), supportMarker(w.Catalog.History, 1)); !errors.Is(e, s.ErrNotFound) {
		t.Fatal("published a marker", e)
	}
}

type memoryInstaller struct {
	stageHook func()
	request   s.IndexInstall
	records   []s.IndexRecord
	finished  bool
}

type interruptedInstaller struct {
	memoryInstaller
	interrupted bool
}

func (m *interruptedInstaller) StageIndexRecord(ctx context.Context, r s.IndexInstall, record s.IndexRecord) error {
	if !m.interrupted {
		m.interrupted = true
		if e := m.memoryInstaller.StageIndexRecord(ctx, r, record); e != nil {
			return e
		}
		return crash
	}
	return m.memoryInstaller.StageIndexRecord(ctx, r, record)
}
func TestRecoveryRetryUsesFrozenGenesisAfterRestoreExtension(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(fmt.Sprint(changed), func(t *testing.T) {
			w, root, physical := liveCheckpointFixture(t)
			r := Retention{Checkpointer: w, Store: &memoryCatalog{}, Packs: physical}
			if e := r.Publish(t.Context(), root); e != nil {
				t.Fatal(e)
			}
			ref := s.PackRef{ID: "pack", Generation: "generation"}
			if e := physical.ChangeStorageClass(t.Context(), ref, s.Glacier); e != nil {
				t.Fatal(e)
			}
			restore := func(days int) {
				if e := physical.Restore(t.Context(), ref, s.RestoreRequest{Days: days, Tier: "Standard"}); e != nil {
					t.Fatal(e)
				}
				if e := physical.CompleteRestore(ref, time.Date(2026, 11, 4+days, 0, 0, 0, 0, time.UTC)); e != nil {
					t.Fatal(e)
				}
			}
			restore(1)
			now := time.Date(2026, 11, 4, 1, 0, 0, 0, time.UTC)
			request := RecoverRequest{Target: now, Bounds: s.R1Bounds{Lower: now, Upper: now, Horizon: now.Add(time.Hour)}, History: strings.Repeat("8", 32), Incarnation: strings.Repeat("9", 32)}
			installer := &interruptedInstaller{}
			recoverer := Recoverer{Retention: r, Installer: installer}
			if _, e := recoverer.Recover(t.Context(), request); !errors.Is(e, crash) {
				t.Fatal(e)
			}
			frozenRequest := installer.request
			stagedBefore := len(installer.records)
			original, e := w.Objects.Read(t.Context(), "recovery/active/"+request.Incarnation+".json")
			if e != nil {
				t.Fatal(e)
			}
			if changed {
				restore(2)
				request.Bounds.Lower = time.Date(2026, 11, 5, 0, 30, 0, 0, time.UTC)
				request.Bounds.Upper = request.Bounds.Lower
				request.Bounds.Horizon = request.Bounds.Upper.Add(time.Hour)
				if _, e = recoverer.Recover(t.Context(), request); !errors.Is(e, s.ErrConflict) {
					t.Fatal("opened with stale frozen restore facts", e)
				}
				if installer.finished {
					t.Fatal("stale restore facts reached readiness")
				}
				if len(installer.records) != stagedBefore {
					t.Fatal("staged stale facts after readiness mismatch")
				}
			} else {
				if _, e = recoverer.Recover(t.Context(), request); e != nil || !installer.finished {
					t.Fatal(e)
				}
				found := false
				for _, record := range installer.records {
					if record.Kind == "packs" {
						var p s.PackInfo
						if e = json.Unmarshal(record.Body, &p); e != nil {
							t.Fatal(e)
						}
						if p.Ref == ref {
							found = true
							if !p.RestoredUntil.Equal(time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)) || !lifecycle.Readable(p, request.Bounds.Upper) {
								t.Fatal("unreadable staged facts at readiness", p)
							}
						}
					}
				}
				if !found {
					t.Fatal("pack facts were not inspected at readiness")
				}
			}
			after, e := w.Objects.Read(t.Context(), "recovery/active/"+request.Incarnation+".json")
			if e != nil || !bytes.Equal(original, after) {
				t.Fatal(e)
			}
			if installer.request != frozenRequest {
				t.Fatal("installation digest changed on retry")
			}
		})
	}
}
func TestRebuildDoesNotInstallUnreferencedCanonicalWinners(t *testing.T) {
	w, root, physical := liveCheckpointFixture(t)
	image, e := w.Load(t.Context(), root)
	if e != nil {
		t.Fatal(e)
	}
	var tombstone s.ObjectHead
	for _, row := range image.Rows {
		kind, _, raw, e := projection(row)
		if e != nil {
			t.Fatal(e)
		}
		if kind == "Objects" {
			if e = json.Unmarshal(raw, &tombstone); e != nil {
				t.Fatal(e)
			}
		}
	}
	tombstone.Revision++
	tombstone.Deleted = true
	tombstone.Size = 0
	tombstone.ManifestID = ""
	tombstone.PageCount = 0
	tombstone.ChunkCount = 0
	tombstone.PageIndex = nil
	id, _ := json.Marshal(tombstone.Object)
	effect, e := operations.Projection("Objects", string(id), tombstone.Revision, tombstone, false)
	if e != nil {
		t.Fatal(e)
	}
	fixtureSeal(t, w, 1, image.Snapshot.Seal.Reference.SHA256, []Effect{effect})
	next, e := w.Build(t.Context(), root, 1)
	if e != nil {
		t.Fatal(e)
	}
	if e = physical.DeletePack(t.Context(), s.PackRef{ID: "pack", Generation: "generation"}); e != nil {
		t.Fatal(e)
	}
	rebuilt, e := w.Rebuild(t.Context(), next)
	if e != nil {
		t.Fatal(e)
	}
	if e = rebuilt.VerifyPayload(t.Context(), physical); e != nil {
		t.Fatal(e)
	}
	for _, record := range rebuilt.Records {
		if record.Kind == "chunks" {
			t.Fatal("unverified dead winner installed")
		}
	}
	historical, e := w.Load(t.Context(), next)
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for key := range historical.Rows {
		found = found || strings.HasPrefix(key, "Chunks/")
	}
	if !found {
		t.Fatal("lost audit rows")
	}
	canonical, e := w.Build(t.Context(), AttachmentReference{Key: genesisKey(w.Catalog), Reference: w.Catalog.Genesis}, 1)
	if e != nil {
		t.Fatal(e)
	}
	retention := Retention{Checkpointer: w, Store: &memoryCatalog{}, Packs: physical}
	if e = retention.Publish(t.Context(), canonical); e != nil {
		t.Fatal(e)
	}
	now := time.Date(2026, 11, 4, 1, 0, 0, 0, time.UTC)
	catalog, e := (Recoverer{Retention: retention, Installer: &memoryInstaller{}}).Recover(t.Context(), RecoverRequest{Target: now, Bounds: s.R1Bounds{Lower: now, Upper: now, Horizon: now.Add(time.Hour)}, History: strings.Repeat("8", 32), Incarnation: strings.Repeat("9", 32)})
	if e != nil {
		t.Fatal(e)
	}
	normalized, e := (Checkpointer{Objects: w.Objects, Catalog: catalog}).Load(t.Context(), AttachmentReference{Key: genesisKey(catalog), Reference: catalog.Genesis})
	if e != nil {
		t.Fatal(e)
	}
	for key := range normalized.Rows {
		if strings.HasPrefix(key, "Chunks/") {
			t.Fatal("dead winner leaked into new history baseline")
		}
	}
}

func (m *memoryInstaller) BeginIndexInstall(_ context.Context, r s.IndexInstall) error {
	m.request = r
	return nil
}
func (m *memoryInstaller) StageIndexRecord(_ context.Context, _ s.IndexInstall, r s.IndexRecord) error {
	m.records = append(m.records, r)
	if m.stageHook != nil {
		m.stageHook()
	}
	return nil
}
func (m *memoryInstaller) FinishIndexInstall(context.Context, s.IndexInstall) error {
	m.finished = true
	return nil
}

type fixturePITR struct {
	root  AttachmentReference
	err   error
	calls int
}

func (p *fixturePITR) AtCut(context.Context, SupportedCut) (AttachmentReference, error) {
	p.calls++
	return p.root, p.err
}
func TestRecoverySupportedCutPITRAndNewHistory(t *testing.T) {
	w, root, physical := liveCheckpointFixture(t)
	r := Retention{Checkpointer: w, Store: &memoryCatalog{}, Packs: physical}
	if e := r.Publish(t.Context(), root); e != nil {
		t.Fatal(e)
	}
	oldHead := r.Store.(*memoryCatalog).head
	if e := r.Publish(t.Context(), root); e != nil || r.Store.(*memoryCatalog).head != oldHead {
		t.Fatal(e)
	}
	start := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	bounds := s.R1Bounds{Lower: start.Add(time.Hour), Upper: start.Add(time.Hour), Horizon: start.Add(2 * time.Hour)}
	if _, e := r.Select(t.Context(), start.Add(-25*time.Hour), bounds); !errors.Is(e, s.ErrInvalid) {
		t.Fatal(e)
	}
	if _, e := r.Select(t.Context(), start.Add(2*time.Hour), bounds); !errors.Is(e, s.ErrInvalid) {
		t.Fatal(e)
	}
	pitr := &fixturePITR{root: root}
	installer := &memoryInstaller{}
	recoverer := Recoverer{Retention: r, Installer: installer, PITR: pitr}
	catalog, e := recoverer.Recover(t.Context(), RecoverRequest{Target: start.Add(30 * time.Minute), Bounds: bounds, History: strings.Repeat("8", 32), Incarnation: strings.Repeat("9", 32)})
	if e != nil {
		t.Fatal(e)
	}
	if !installer.finished || pitr.calls != 1 || installer.request.SourceHistory != w.Catalog.History || installer.request.Incarnation != catalog.Incarnation {
		t.Fatal(installer, pitr)
	}
	selected, e := LoadSelectedCatalog(t.Context(), w.Objects, catalog.History, catalog.Incarnation)
	if e != nil || selected != catalog {
		t.Fatal(selected, e)
	}
	newImage, e := (Checkpointer{Objects: w.Objects, Catalog: catalog}).Load(t.Context(), AttachmentReference{Key: genesisKey(catalog), Reference: catalog.Genesis})
	if e != nil || newImage.Snapshot.Mode != "normalized-genesis" {
		t.Fatal(newImage, e)
	}
	old, e := LoadCatalog(t.Context(), w.Objects)
	if e != nil || !reflect.DeepEqual(old, w.Catalog) {
		t.Fatal("old discovery rewritten", old, e)
	}
	roots, e := r.ProtectedRoots(t.Context())
	if e != nil || len(roots) != 1 {
		t.Fatal(roots, e)
	}
	inherited, e := (Retention{Checkpointer: Checkpointer{Objects: w.Objects, Catalog: catalog}}).ProtectedRoots(t.Context())
	if e != nil || !reflect.DeepEqual(inherited, roots) {
		t.Fatal("lost source-history roots", inherited, e)
	}
}
