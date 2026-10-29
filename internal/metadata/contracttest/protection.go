package contracttest

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/recovery"
	"github.com/KinasTomes/miles/internal/recovery/format"
	s "github.com/KinasTomes/miles/internal/storage"
	shared "github.com/KinasTomes/miles/internal/storage/contracttest"
)

func runProtectionJournal(t *testing.T, factory shared.MetadataFactory) {
	t.Run("protection-atomic-outbox-lost-ack-and-replay", func(t *testing.T) {
		f := shared.NewFixture()
		f.Packs[0].StateRevision = 1
		f.Packs[0].ProtectedUntil = f.Now.Add(time.Minute)
		h := factory(t, f)
		store := h.Store.(s.PackProtectionStore)
		producer := h.Store.(s.ProducerStore)
		row := f.Chunk("default")
		base := context.Background()
		if _, _, err := h.Store.PutChunkIfAbsent(base, row, f.Lease.Fence); err != nil {
			t.Fatal(err)
		}
		r := s.PackProtectionUpdate{Chunk: row, ExpectedStateRevision: 1, RequiredUntil: f.Now.Add(2 * time.Minute), Writer: f.Lease.Fence}
		root := s.MutationContext{HistoryID: strings.Repeat("1", 32), LogicalOpID: strings.Repeat("2", 32), MutationID: strings.Repeat("2", 32), Writer: f.Lease.Fence}
		ctx := s.WithMutation(base, root)
		identity := row.Location.Pack.ID + "/" + row.Location.Pack.Generation
		input, _ := json.Marshal(r)
		sum := sha256.Sum256(input)
		child := s.ChildMutation(root, "PackProtection", identity, "protect/"+hex.EncodeToString(sum[:]))
		eventID, _ := s.EventID(child.HistoryID, child.MutationID, "PackProtection", "entity_put")
		h.FailCommit(true, false)
		if _, err := store.ExtendPackProtection(ctx, r); !errors.Is(err, s.ErrUnavailable) {
			t.Fatal("before commit", err)
		}
		packs, _ := h.Store.GetPacks(base, []s.PackRef{row.Location.Pack})
		if !s.SamePackInfo(packs[row.Location.Pack], f.Packs[0]) {
			t.Fatal("partial protection")
		}
		if _, err := producer.GetMutation(base, child.MutationID); !errors.Is(err, s.ErrNotFound) {
			t.Fatal("partial outcome", err)
		}
		if _, err := h.Store.GetOutbox(base, eventID); !errors.Is(err, s.ErrNotFound) {
			t.Fatal("partial event", err)
		}
		h.FailCommit(false, true)
		if _, err := store.ExtendPackProtection(ctx, r); !errors.Is(err, s.ErrUnavailable) {
			t.Fatal("lost ACK", err)
		}
		h.FailCommit(false, false)
		packs, _ = h.Store.GetPacks(base, []s.PackRef{row.Location.Pack})
		committed := packs[row.Location.Pack]
		if committed.StateRevision != 1 || !committed.ProtectedUntil.Equal(r.RequiredUntil) {
			t.Fatal(committed)
		}
		outcome, err := producer.GetMutation(base, child.MutationID)
		if err != nil || outcome.Event.ID != eventID || outcome.Event.Entity != "PackProtection" || outcome.Event.Revision != 1 {
			t.Fatal(outcome, err)
		}
		reader, err := producer.Open(base, outcome.Event.PayloadPointer)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(reader)
		_ = reader.Close()
		if err != nil {
			t.Fatal(err)
		}
		var bundle format.Input
		if err := json.Unmarshal(body, &bundle); err != nil {
			t.Fatal(err)
		}
		foundState, foundProtection := false, false
		for _, page := range bundle.Pages {
			for _, effect := range page.Effects {
				if effect.Key == "Packs/"+hex.EncodeToString([]byte(identity)) {
					foundState = true
					if effect.Revision != 1 {
						t.Fatal("state revision", effect)
					}
				}
				if effect.Key != "PackProtection/"+hex.EncodeToString([]byte(identity)) {
					continue
				}
				foundProtection = true
				raw, err := base64.StdEncoding.Strict().DecodeString(effect.Value)
				if err != nil {
					t.Fatal(err)
				}
				var projection format.Projection
				if json.Unmarshal(raw, &projection) != nil || projection.Entity != "PackProtection" {
					t.Fatal("protection projection")
				}
				var fact struct {
					Version        uint32
					Ref            s.PackRef
					ProtectedUntil time.Time
				}
				if json.Unmarshal(projection.Value, &fact) != nil || fact.Version != 1 || fact.Ref != row.Location.Pack || !fact.ProtectedUntil.Equal(r.RequiredUntil) {
					t.Fatal("journal lost protection", fact)
				}
			}
		}
		if !foundState || !foundProtection {
			t.Fatal("incomplete atomic bundle")
		}
		sink := producerSink{}
		if err := (recovery.Exporter{Metadata: h.Store.(s.OutboxExportStore), Payloads: producer, Objects: sink}).Export(base, eventID); err != nil {
			t.Fatal("protection export", err)
		}
		again, err := store.ExtendPackProtection(ctx, r)
		if err != nil || !s.SamePackInfo(again, committed) {
			t.Fatal("same-operation retry", again, err)
		}
		rows, _ := h.Store.GetChunks(base, []s.ChunkKey{row.Key}, true)
		if rows[row.Key] != row {
			t.Fatal("protection changed location")
		}
		// A full PackInfo rewrite (e.g. batched access stats) must retain the
		// adapter's protection counter, otherwise replay revisions restart at1.
		if err := h.Store.UpdateAccessStats(base, []s.PackAccess{{Pack: row.Location.Pack, LastAccess: f.Now.Add(time.Second)}}); err != nil {
			t.Fatal(err)
		}
		root.LogicalOpID = strings.Repeat("3", 32)
		root.MutationID = root.LogicalOpID
		next := r
		next.RequiredUntil = f.Now.Add(3 * time.Minute)
		nextBody, _ := json.Marshal(next)
		nextSum := sha256.Sum256(nextBody)
		nextChild := s.ChildMutation(root, "PackProtection", identity, "protect/"+hex.EncodeToString(nextSum[:]))
		if _, err := store.ExtendPackProtection(s.WithMutation(base, root), next); err != nil {
			t.Fatal("extension after stats", err)
		}
		nextOutcome, err := producer.GetMutation(base, nextChild.MutationID)
		if err != nil || nextOutcome.Event.Revision != 2 {
			t.Fatal("protection counter dropped by PackInfo rewrite", nextOutcome, err)
		}
	})
}
