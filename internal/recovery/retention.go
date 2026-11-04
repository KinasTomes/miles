package recovery

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"time"

	s "github.com/KinasTomes/miles/internal/storage"
)

const ProtectedWindow = 24 * time.Hour
const RetentionBuffer = 10 * time.Minute

type SupportedCut struct {
	Epoch      uint64                `json:"epoch"`
	CutAt      string                `json:"cut_at"`
	Checkpoint AttachmentReference   `json:"checkpoint"`
	Roots      []AttachmentReference `json:"roots"`
	Pins       []string              `json:"pins"`
}
type SupportCatalog struct {
	Version              int `json:"r2_version"`
	History, Incarnation string
	Revision             uint64
	Previous             s.RecoveryCatalogHead
	Cuts                 []SupportedCut
}
type rootPage struct {
	Version int
	Packs   []s.PackRef
}
type Retention struct {
	Checkpointer
	Store  CatalogStore
	Packs  s.PackStore
	Worker s.WriterFence
}
type CatalogStore interface {
	s.RecoveryCatalogStore
	CheckRecoveryWorker(context.Context, s.WriterFence) error
}

func supportMarker(history string, revision uint64) string {
	return fmt.Sprintf("recovery/support/%s/complete/%020d.json", history, revision)
}

// Read discovers immutable completion slots even after loss of the metadata DB.
// With a live metadata store it also reconciles a marker whose pointer ACK was lost.
func (w Retention) Read(ctx context.Context) (s.RecoveryCatalogHead, SupportCatalog, error) {
	head := s.RecoveryCatalogHead{}
	if w.Store != nil {
		var e error
		head, e = w.Store.GetRecoveryCatalog(ctx, w.Catalog.History)
		if e != nil && !errors.Is(e, s.ErrNotFound) {
			return head, SupportCatalog{}, e
		}
	}
	catalog := SupportCatalog{Version: 1, History: w.Catalog.History, Incarnation: w.Catalog.Incarnation, Cuts: []SupportedCut{}}
	load := func(h s.RecoveryCatalogHead) (SupportCatalog, error) {
		body, e := readArtifact(ctx, w.Objects, AttachmentReference{Key: h.Key, Reference: Reference{Length: h.Length, SHA256: h.SHA256}})
		if e != nil {
			return SupportCatalog{}, e
		}
		var c SupportCatalog
		if Decode(body, &c) != nil || c.Version != 1 || c.History != h.History || c.Incarnation != h.Incarnation || c.Revision != h.Revision || c.History != w.Catalog.History || c.Incarnation != w.Catalog.Incarnation {
			return c, s.ErrInvalid
		}
		return c, nil
	}
	if head.Revision > 0 {
		var e error
		catalog, e = load(head)
		if e != nil {
			return head, catalog, e
		}
		body, e := w.Objects.Read(ctx, supportMarker(head.History, head.Revision))
		if e != nil {
			return head, catalog, e
		}
		var marker s.RecoveryCatalogHead
		if Decode(body, &marker) != nil || marker != head {
			return head, catalog, s.ErrInvalid
		}
	}
	for pages := 0; pages < 10000; pages++ {
		body, e := w.Objects.Read(ctx, supportMarker(w.Catalog.History, head.Revision+1))
		if errors.Is(e, s.ErrNotFound) {
			return head, catalog, nil
		}
		if e != nil {
			return head, catalog, e
		}
		var next s.RecoveryCatalogHead
		if Decode(body, &next) != nil || next.Version != 1 || next.History != w.Catalog.History || next.Incarnation != w.Catalog.Incarnation || next.Revision != head.Revision+1 {
			return head, catalog, s.ErrInvalid
		}
		c, e := load(next)
		if e != nil {
			return head, catalog, e
		}
		if c.Previous != head {
			return head, catalog, s.ErrConflict
		}
		if w.Store != nil {
			if e = w.Store.CompareRecoveryCatalog(ctx, head, next, w.Worker); e != nil {
				return head, catalog, e
			}
		}
		head = next
		catalog = c
	}
	return head, catalog, s.ErrUnavailable
}

// Publish only appends support. A newer checkpoint cannot withdraw a protected
// old cut, its exact generations, dependencies, base or discovery anchors.
func (w Retention) Publish(ctx context.Context, checkpoint AttachmentReference) error {
	if w.Store == nil || w.Packs == nil {
		return s.ErrInvalid
	}
	image, err := w.Load(ctx, checkpoint)
	if err != nil {
		return err
	}
	if image.Snapshot.BeforeFirst {
		return s.ErrInvalid
	}
	rebuilt, err := w.Rebuild(ctx, checkpoint)
	if err != nil {
		return err
	}
	if err = rebuilt.verifyPayload(ctx, w.Packs, true); err != nil {
		return err
	}
	// Existence and storage class are checked for every required exact generation.
	// Archive content verification remains an explicit restore-then-verify path.
	refs := map[s.PackRef]bool{}
	for key := range rebuilt.Live {
		refs[rebuilt.Chunks[key].Location.Pack] = true
	}
	list := make([]s.PackRef, 0, len(refs))
	for r := range refs {
		list = append(list, r)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].ID != list[j].ID {
			return list[i].ID < list[j].ID
		}
		return list[i].Generation < list[j].Generation
	})
	for _, r := range list {
		physical, e := w.Packs.HeadPack(ctx, r)
		if e != nil {
			return e
		}
		historical := rebuilt.Packs[r]
		if physical.Ref != r || physical.Size != historical.Size || physical.Class.Validate() != nil {
			return s.ErrInvalid
		}
	}
	head, catalog, err := w.Read(ctx)
	if err != nil {
		return err
	}
	for _, cut := range catalog.Cuts {
		if cut.Epoch == image.Snapshot.Target {
			if cut.Checkpoint != checkpoint {
				return s.ErrConflict
			}
			return nil
		}
	}
	base := AttachmentReference{Key: genesisKey(w.Catalog), Reference: w.Catalog.Genesis}
	for _, cut := range catalog.Cuts {
		if cut.Epoch < image.Snapshot.Target {
			base = cut.Checkpoint
		}
	}
	if image.Snapshot.Base != base {
		return s.ErrConflict
	}
	// A checksum and matching seal do not prove the supplied state projection.
	// Re-derive it from a trusted published base before advertising support.
	expectedRoot, err := w.Build(ctx, base, image.Snapshot.Target)
	if err != nil {
		return err
	}
	expected, err := w.Load(ctx, expectedRoot)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(expected.Rows, image.Rows) || !reflect.DeepEqual(expected.Dependencies, image.Dependencies) || expected.Snapshot.SourceCatalog != image.Snapshot.SourceCatalog || expected.Snapshot.SourceCheckpoint != image.Snapshot.SourceCheckpoint || expected.Snapshot.InheritedSupport != image.Snapshot.InheritedSupport {
		return s.ErrConflict
	}
	body, err := readArtifact(ctx, w.Objects, image.Snapshot.Seal)
	if err != nil {
		return err
	}
	var seal Seal
	if Decode(body, &seal) != nil {
		return s.ErrInvalid
	}
	cut := SupportedCut{Epoch: seal.Epoch, CutAt: seal.CutAt, Checkpoint: checkpoint, Roots: []AttachmentReference{}, Pins: []string{}}
	for at := 0; at < len(list); at += 128 {
		body, e := Encode(rootPage{Version: 1, Packs: list[at:min(at+128, len(list))]})
		if e != nil {
			return e
		}
		r := AttachmentReference{Key: "recovery/roots/" + catalog.History + "/" + digest(body) + ".json", Reference: ref(body)}
		if e = putVerified(ctx, w.Objects, r.Key, body); e != nil {
			return e
		}
		cut.Roots = append(cut.Roots, r)
	}
	catalog.Cuts = append(catalog.Cuts, cut)
	sort.Slice(catalog.Cuts, func(i, j int) bool { return catalog.Cuts[i].Epoch < catalog.Cuts[j].Epoch })
	return w.save(ctx, head, catalog)
}
func (w Retention) save(ctx context.Context, head s.RecoveryCatalogHead, catalog SupportCatalog) error {
	if w.Store == nil {
		return s.ErrInvalid
	}
	objects := guardedObjects{w.Objects, func(ctx context.Context) error { return w.Store.CheckRecoveryWorker(ctx, w.Worker) }}
	catalog.Revision = head.Revision + 1
	catalog.Previous = head
	body, err := Encode(catalog)
	if err != nil {
		return err
	}
	next := s.RecoveryCatalogHead{Version: 1, History: catalog.History, Incarnation: catalog.Incarnation, Revision: catalog.Revision, Key: "recovery/support/" + catalog.History + "/" + digest(body) + ".json", SHA256: digest(body), Length: len(body)}
	if err = putVerified(ctx, objects, next.Key, body); err != nil {
		return err
	}
	marker, err := Encode(next)
	if err != nil {
		return err
	}
	if err = putVerified(ctx, objects, supportMarker(next.History, next.Revision), marker); err != nil {
		return err
	}
	return w.Store.CompareRecoveryCatalog(ctx, head, next, w.Worker)
}

// Select never rounds forward. Bounds are trusted UTC lower/upper, not an
// operator-provided wall clock. Outside-window/future targets are rejected.
func (w Retention) Select(ctx context.Context, target time.Time, bounds s.R1Bounds) (SupportedCut, error) {
	var selected SupportedCut
	if target.IsZero() || bounds.Lower.IsZero() || bounds.Upper.Before(bounds.Lower) || target.After(bounds.Lower) || target.Before(bounds.Upper.Add(-ProtectedWindow)) {
		return selected, s.ErrInvalid
	}
	_, catalog, err := w.Read(ctx)
	if err != nil {
		return selected, err
	}
	found := false
	for _, cut := range catalog.Cuts {
		at, e := time.Parse(time.RFC3339Nano, cut.CutAt)
		if e != nil {
			return SupportedCut{}, s.ErrInvalid
		}
		if !at.After(target) && !at.Before(bounds.Upper.Add(-ProtectedWindow)) && (!found || cut.Epoch > selected.Epoch) {
			selected = cut
			found = true
		}
	}
	if !found {
		return selected, s.ErrNotFound
	}
	image, e := w.Load(ctx, selected.Checkpoint)
	if e != nil {
		return SupportedCut{}, e
	}
	if image.Snapshot.BeforeFirst || image.Snapshot.Target != selected.Epoch {
		return SupportedCut{}, s.ErrInvalid
	}
	sealBody, e := readArtifact(ctx, w.Objects, image.Snapshot.Seal)
	if e != nil {
		return SupportedCut{}, e
	}
	var seal Seal
	if Decode(sealBody, &seal) != nil || seal.CutAt != selected.CutAt {
		return SupportedCut{}, s.ErrInvalid
	}
	rebuilt, e := w.Rebuild(ctx, selected.Checkpoint)
	if e != nil {
		return SupportedCut{}, e
	}
	expected := map[s.PackRef]bool{}
	for key := range rebuilt.Live {
		expected[rebuilt.Chunks[key].Location.Pack] = true
	}
	seen := map[s.PackRef]bool{}
	if err != nil {
		return SupportedCut{}, err
	}
	for _, r := range selected.Roots {
		body, e := readArtifact(ctx, w.Objects, r)
		if e != nil {
			return SupportedCut{}, e
		}
		var p rootPage
		if Decode(body, &p) != nil || p.Version != 1 || len(p.Packs) == 0 {
			return SupportedCut{}, s.ErrInvalid
		}
		for _, ref := range p.Packs {
			if !expected[ref] || seen[ref] {
				return SupportedCut{}, s.ErrInvalid
			}
			seen[ref] = true
		}
	}
	if len(seen) != len(expected) {
		return SupportedCut{}, s.ErrInvalid
	}
	return selected, nil
}

// ProtectedRoots includes all published cuts plus their anchors, including
// outside-window pinned cuts. No age-only deletion permission is returned.
func (w Retention) ProtectedRoots(ctx context.Context) ([]s.PackRef, error) {
	_, catalog, err := w.Read(ctx)
	if err != nil {
		return nil, err
	}
	set := map[s.PackRef]bool{}
	cuts := append([]SupportedCut{}, catalog.Cuts...)
	current := w.Catalog
	for depth := 0; depth < 16; depth++ {
		image, e := (Checkpointer{Objects: w.Objects, Catalog: current}).Load(ctx, AttachmentReference{Key: genesisKey(current), Reference: current.Genesis})
		if e != nil {
			return nil, e
		}
		inherited := image.Snapshot.InheritedSupport
		if inherited.Revision == 0 {
			break
		}
		body, e := readArtifact(ctx, w.Objects, AttachmentReference{Key: inherited.Key, Reference: Reference{Length: inherited.Length, SHA256: inherited.SHA256}})
		if e != nil {
			return nil, e
		}
		var prior SupportCatalog
		if Decode(body, &prior) != nil || prior.History != inherited.History || prior.Incarnation != inherited.Incarnation || prior.Revision != inherited.Revision {
			return nil, s.ErrInvalid
		}
		cuts = append(cuts, prior.Cuts...)
		current = image.Snapshot.SourceCatalog
		if current.Validate() != nil {
			return nil, s.ErrInvalid
		}
		if depth == 15 {
			return nil, s.ErrUnavailable
		}
	}
	for _, cut := range cuts {
		for _, r := range cut.Roots {
			body, e := readArtifact(ctx, w.Objects, r)
			if e != nil {
				return nil, e
			}
			var p rootPage
			if Decode(body, &p) != nil || p.Version != 1 {
				return nil, s.ErrInvalid
			}
			for _, ref := range p.Packs {
				if ref.Validate() != nil {
					return nil, s.ErrInvalid
				}
				set[ref] = true
			}
		}
	}
	out := make([]s.PackRef, 0, len(set))
	for r := range set {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ID != out[j].ID {
			return out[i].ID < out[j].ID
		}
		return out[i].Generation < out[j].Generation
	})
	return out, nil
}

// Pin is an internal, fenced consumer dependency. Removing one pin never
// removes another consumer's pin or independently withdraws a supported cut.
func (w Retention) Pin(ctx context.Context, epoch uint64, id string, retain bool) error {
	if id == "" || len(id) > 128 {
		return s.ErrInvalid
	}
	head, c, e := w.Read(ctx)
	if e != nil {
		return e
	}
	for i := range c.Cuts {
		if c.Cuts[i].Epoch != epoch {
			continue
		}
		pins := c.Cuts[i].Pins
		found := -1
		for n, p := range pins {
			if p == id {
				found = n
			}
		}
		if retain && found < 0 {
			pins = append(pins, id)
		} else if !retain && found >= 0 {
			pins = append(pins[:found], pins[found+1:]...)
		} else {
			return nil
		}
		sort.Strings(pins)
		c.Cuts[i].Pins = pins
		return w.save(ctx, head, c)
	}
	return s.ErrNotFound
}

// Prune withdraws only expired, unpinned advertised targets. It never deletes
// physical payload, checkpoint pages, marker history or provenance anchors;
// downstream GC must still discharge every independent R1/R2 root.
func (w Retention) Prune(ctx context.Context, b s.R1Bounds) error {
	if b.Lower.IsZero() || b.Upper.Before(b.Lower) || b.Horizon.Before(b.Upper) {
		return s.ErrInvalid
	}
	head, c, e := w.Read(ctx)
	if e != nil {
		return e
	}
	kept := []SupportedCut{}
	for _, cut := range c.Cuts {
		at, e := time.Parse(time.RFC3339Nano, cut.CutAt)
		if e != nil {
			return s.ErrInvalid
		}
		if len(cut.Pins) > 0 || !b.Lower.After(at.Add(ProtectedWindow+RetentionBuffer)) {
			kept = append(kept, cut)
		}
	}
	if len(kept) == len(c.Cuts) {
		return nil
	}
	c.Cuts = kept
	return w.save(ctx, head, c)
}

// VerifyCutAfter confirms that published recovery support has advanced beyond
// the source's retirement. Callers must independently check ProtectedRoots.
func (w Retention) VerifyCutAfter(ctx context.Context, retired time.Time) error {
	_, catalog, e := w.Read(ctx)
	if e != nil {
		return e
	}
	for _, cut := range catalog.Cuts {
		at, e := time.Parse(time.RFC3339Nano, cut.CutAt)
		if e != nil {
			return s.ErrInvalid
		}
		if at.After(retired) {
			return nil
		}
	}
	return s.ErrR1Busy
}
