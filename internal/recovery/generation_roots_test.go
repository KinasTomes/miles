package recovery

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/KinasTomes/miles/internal/metadata/operations"
	"github.com/KinasTomes/miles/internal/pack"
	s "github.com/KinasTomes/miles/internal/storage"
)

// Replay real projection bundles through sealed checkpoints. Each historical
// cut must retain its original location even after the current winner moves.
func generationCuts(t *testing.T) (Retention, []AttachmentReference, []s.Chunk) {
	t.Helper()
	w, first, physical := liveCheckpointFixture(t)
	r := Retention{Checkpointer: w, Store: &memoryCatalog{}, Packs: physical}
	if err := r.Publish(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	rebuilt, err := w.Rebuild(t.Context(), first)
	if err != nil {
		t.Fatal(err)
	}
	var chunk s.Chunk
	for _, c := range rebuilt.Chunks {
		chunk = c
	}
	cuts, locations := []AttachmentReference{first}, []s.Chunk{chunk}
	appendCut := func(epoch uint64, effects []Effect) {
		t.Helper()
		base := cuts[len(cuts)-1]
		image, err := w.Load(t.Context(), base)
		if err != nil {
			t.Fatal(err)
		}
		fixtureSeal(t, w, epoch, image.Snapshot.Seal.Reference.SHA256, effects)
		next, err := w.Build(t.Context(), base, epoch)
		if err != nil {
			t.Fatal(err)
		}
		if err = r.Publish(t.Context(), next); err != nil {
			t.Fatal(err)
		}
		cuts = append(cuts, next)
	}
	project := func(kind, id string, revision uint64, value any) Effect {
		t.Helper()
		e, err := operations.Projection(kind, id, revision, value, false)
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	for epoch := uint64(1); epoch <= 2; epoch++ {
		ref := s.PackRef{ID: chunk.Location.Pack.ID, Generation: fmt.Sprintf("generation-%d", epoch+1)}
		builder, err := pack.NewBuilder(ref)
		if err != nil {
			t.Fatal(err)
		}
		if err = builder.Add(pack.Blob{Key: chunk.Key, RawLength: 4, Codec: "raw", Stored: []byte("data")}); err != nil {
			t.Fatal(err)
		}
		body, sidecar, entries, err := builder.Finish()
		if err != nil {
			t.Fatal(err)
		}
		p := s.PackInfo{Version: 1, Ref: ref, Size: int64(len(body)), Class: s.Standard, State: "Warm", StateRevision: 1}
		if err = physical.PutPack(t.Context(), p, bytes.NewReader(body)); err != nil {
			t.Fatal(err)
		}
		if err = physical.PutSidecar(t.Context(), ref, int64(len(sidecar)), bytes.NewReader(sidecar)); err != nil {
			t.Fatal(err)
		}
		chunk.Location, chunk.LocationRevision = entries[0].Location(ref), epoch+1
		appendCut(epoch, []Effect{
			project("Packs", ref.ID+"/"+ref.Generation, 1, operations.PackStateProjection(p)),
			project("Chunks", chunk.Key.Domain+"#"+chunk.Key.Hash, chunk.LocationRevision, chunk),
		})
		locations = append(locations, chunk)
	}
	head := rebuilt.Heads[0]
	headID, _ := json.Marshal(head.Object)
	tombstone := s.ObjectHead{Version: 1, Revision: head.Revision + 1, Object: head.Object, Domain: head.Domain, OpID: head.OpID, Deleted: true}
	appendCut(3, []Effect{project("Objects", string(headID), tombstone.Revision, tombstone)})
	// Recreating the same bucket name has a fresh identity; the old tombstone
	// cannot become the new namespace's object. Only the explicit new head is live.
	head.Object.BucketID = "recreated-bucket"
	headID, _ = json.Marshal(head.Object)
	appendCut(4, []Effect{
		project("Buckets", head.Object.Bucket, 2, s.Bucket{Name: head.Object.Bucket, ID: head.Object.BucketID, OwnerID: "owner", Domain: head.Domain, Active: true, Revision: 2}),
		project("Objects", string(headID), head.Revision, head),
	})
	return r, cuts, locations
}

func TestRecoveryMissingHistoricalGenerationFailsWholeEnumeration(t *testing.T) {
	r, _, locations := generationCuts(t)
	// The current head and its newest generation remain intact. Losing an
	// intermediate historical generation must invalidate the complete report.
	if err := r.Packs.DeletePack(t.Context(), locations[1].Location.Pack); err != nil {
		t.Fatal(err)
	}
	if roots, err := r.EnumerateProtectedData(t.Context(), 9); roots != nil || !errors.Is(err, s.ErrNotFound) {
		t.Fatal("partial physical recovery proof escaped", roots, err)
	}
	if _, err := r.Packs.HeadPack(t.Context(), locations[2].Location.Pack); err != nil {
		t.Fatal("fixture also lost current generation", err)
	}
	protected, err := r.ProtectedRoots(t.Context())
	if err != nil || len(protected) != 3 {
		t.Fatal("missing payload silently withdrew a protected cut", protected, err)
	}
}

func TestRecoveryRootsKeepExactGenerationsAcrossDeleteAndRecreate(t *testing.T) {
	r, cuts, locations := generationCuts(t)
	roots, err := r.EnumerateProtectedData(t.Context(), 9) // five cuts plus four live locations
	if err != nil || len(roots) != len(cuts) {
		t.Fatal(roots, err)
	}
	for i, root := range roots {
		if root.Cut.Checkpoint != cuts[i] {
			t.Fatal("cut changed", root.Cut)
		}
		if i == 3 {
			if len(root.Locations) != 0 {
				t.Fatal("tombstone exposed payload", root.Locations)
			}
			continue
		}
		want := locations[min(i, 2)]
		if len(root.Locations) != 1 || root.Locations[0] != want {
			t.Fatal("historical canonical rebound to current generation", i, root.Locations, want)
		}
		rebuilt, err := r.Checkpointer.Rebuild(t.Context(), cuts[i])
		if err != nil {
			t.Fatal(err)
		}
		if err := rebuilt.VerifyPayload(t.Context(), r.Packs); err != nil {
			t.Fatal("root lost physical data", i, err)
		}
		live := 0
		for _, head := range rebuilt.Heads {
			if head.Deleted {
				continue
			}
			live++
			if i == 4 && head.Object.BucketID != "recreated-bucket" {
				t.Fatal("old namespace resurrected", head)
			}
		}
		if live != 1 {
			t.Fatal("namespace identity mismatch", i, rebuilt.Heads)
		}
	}
	protected, err := r.ProtectedRoots(t.Context())
	if err != nil || len(protected) != 3 {
		t.Fatal(protected, err)
	}
}
