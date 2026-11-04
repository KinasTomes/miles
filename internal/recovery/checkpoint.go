package recovery

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"

	s "github.com/KinasTomes/miles/internal/storage"
)

// Snapshot describes historical facts. Installation normalization never edits it.
// BeforeFirst is reserved for an explicitly audited genesis, not a live scan.
type Snapshot struct {
	RequestedTarget  string                `json:"requested_target,omitempty"`
	SourceCatalog    Catalog               `json:"source_catalog"`
	SourceCheckpoint AttachmentReference   `json:"source_checkpoint"`
	SourceTarget     uint64                `json:"source_target"`
	InheritedSupport s.RecoveryCatalogHead `json:"inherited_support"`
	Version          int                   `json:"r2_version"`
	Mode             string                `json:"mode"`
	History          string                `json:"history"`
	BeforeFirst      bool                  `json:"before_first"`
	Target           uint64                `json:"target_epoch"`
	Seal             AttachmentReference   `json:"seal"`
	Base             AttachmentReference   `json:"base"`
	Pages            []AttachmentReference `json:"pages"`
	Dependencies     []Dependency          `json:"dependencies"`
	DependencyPages  []AttachmentReference `json:"dependency_pages,omitempty"`
}
type Dependency struct {
	Event s.OutboxEvent       `json:"event"`
	Root  AttachmentReference `json:"root"`
}
type snapshotPage struct {
	Version int      `json:"r2_version"`
	History string   `json:"history"`
	Number  int      `json:"number"`
	Rows    []Effect `json:"rows"`
}
type Image struct {
	Snapshot     Snapshot
	Rows         map[string]Effect
	Dependencies []Dependency
}
type dependencyPage struct {
	Version      int
	Dependencies []Dependency
}

// Checkpointer operates only on immutable, checksum-bound artifacts. All limits
// fail closed; no partially built checkpoint is published as supported.
type Checkpointer struct {
	anchor  *SupportedCut
	Objects ImmutableStore
	Catalog Catalog
}
type snapshotDepthKey struct{}

const MaxCheckpointRows = 128 * MaxFanout

func (w Checkpointer) Load(ctx context.Context, root AttachmentReference) (Image, error) {
	var out Image
	depth, _ := ctx.Value(snapshotDepthKey{}).(int)
	if depth >= 16 || w.Objects == nil || w.Catalog.Validate() != nil {
		return out, s.ErrInvalid
	}
	ctx = context.WithValue(ctx, snapshotDepthKey{}, depth+1)
	body, err := readArtifact(ctx, w.Objects, root)
	if err != nil {
		return out, err
	}
	if Decode(body, &out.Snapshot) != nil {
		return Image{}, s.ErrInvalid
	}
	cp := out.Snapshot
	if cp.Version != 1 || cp.Mode != "historical" && cp.Mode != "normalized-genesis" || cp.History != w.Catalog.History || cp.Target >= maxInteger || cp.Pages == nil || cp.Dependencies == nil {
		return Image{}, s.ErrInvalid
	}
	ancestral := map[string]Dependency{}
	if cp.SourceCatalog.Version != 0 {
		if cp.SourceCatalog.Validate() != nil || cp.SourceCatalog.History == cp.History {
			return Image{}, s.ErrInvalid
		}
		origin, e := (Checkpointer{Objects: w.Objects, Catalog: cp.SourceCatalog}).Load(ctx, cp.SourceCheckpoint)
		if e != nil {
			return Image{}, e
		}
		if origin.Snapshot.BeforeFirst || origin.Snapshot.Target != cp.SourceTarget {
			return Image{}, s.ErrInvalid
		}
		for _, d := range origin.Dependencies {
			ancestral[d.Root.Key+"/"+d.Root.Reference.SHA256] = d
		}
	}
	if cp.BeforeFirst {
		if cp.Target != 0 || cp.Seal != (AttachmentReference{}) || root.Key != genesisKey(w.Catalog) || root.Reference != w.Catalog.Genesis {
			return Image{}, s.ErrInvalid
		}
		if cp.Mode == "normalized-genesis" {
			if cp.SourceCatalog.Version == 0 {
				return Image{}, s.ErrInvalid
			}
		}
	} else {
		if cp.Mode != "historical" {
			return Image{}, s.ErrInvalid
		}
		seal, e := w.verifyTarget(ctx, cp.Target)
		if e != nil {
			return Image{}, e
		}
		if seal != cp.Seal {
			return Image{}, s.ErrConflict
		}
	}
	out.Rows = map[string]Effect{}
	last := ""
	for n, r := range cp.Pages {
		b, e := readArtifact(ctx, w.Objects, r)
		if e != nil {
			return Image{}, e
		}
		var p snapshotPage
		if Decode(b, &p) != nil || p.Version != 1 || p.History != cp.History || p.Number != n || len(p.Rows) == 0 || len(p.Rows) > 128 {
			return Image{}, s.ErrInvalid
		}
		for _, row := range p.Rows {
			if !validEffect(row) || row.Key <= last {
				return Image{}, s.ErrInvalid
			}
			out.Rows[row.Key] = row
			last = row.Key
		}
	}
	out.Dependencies = append([]Dependency{}, cp.Dependencies...)
	for _, r := range cp.DependencyPages {
		body, e := readArtifact(ctx, w.Objects, r)
		if e != nil {
			return Image{}, e
		}
		var page dependencyPage
		if Decode(body, &page) != nil || page.Version != 1 || len(page.Dependencies) == 0 || len(page.Dependencies) > 64 {
			return Image{}, s.ErrInvalid
		}
		out.Dependencies = append(out.Dependencies, page.Dependencies...)
	}
	for _, d := range out.Dependencies {
		prior, exists := ancestral[d.Root.Key+"/"+d.Root.Reference.SHA256]
		original := exists && reflect.DeepEqual(prior, d)
		if d.Event.Validate() != nil || !original && (d.Event.HistoryID != cp.History || d.Event.Epoch > cp.Target) {
			return Image{}, s.ErrInvalid
		}
		if e := VerifyAttachment(ctx, w.Objects, d.Root, d.Event); e != nil {
			return Image{}, e
		}
	}
	return out, nil
}
func validEffect(e Effect) bool {
	return e.Key != "" && e.Revision > 0 && e.Revision <= maxInteger && (!e.Deleted || e.Value == "")
}

func (w Checkpointer) Build(ctx context.Context, base AttachmentReference, target uint64) (AttachmentReference, error) {
	var zero AttachmentReference
	if w.Objects == nil || w.Catalog.Validate() != nil || target >= maxInteger {
		return zero, s.ErrInvalid
	}
	image, err := w.Load(ctx, base)
	if err != nil {
		return zero, err
	}
	cp := image.Snapshot
	for key, row := range image.Rows {
		if strings.HasPrefix(key, "SystemConfig/") {
			h := row
			h.Key = "PolicyHistory/" + strings.TrimPrefix(key, "SystemConfig/") + fmt.Sprintf("/%020d", row.Revision)
			if prior, ok := image.Rows[h.Key]; ok && prior != h {
				return zero, s.ErrConflict
			}
			image.Rows[h.Key] = h
		}
	}
	if !cp.BeforeFirst && target < cp.Target {
		return zero, s.ErrInvalid
	}
	if !cp.BeforeFirst && target == cp.Target {
		return base, nil
	}
	seal, err := w.verifyTarget(ctx, target)
	if err != nil {
		return zero, err
	}
	start := uint64(0)
	if !cp.BeforeFirst {
		start = cp.Target + 1
	}
	deps := append([]Dependency{}, image.Dependencies...)
	depSeen := map[string]bool{}
	for _, d := range deps {
		depSeen[d.Root.Key+"/"+d.Root.Reference.SHA256] = true
	}
	for epoch := start; epoch <= target; epoch++ {
		b, e := w.Objects.Read(ctx, epochPrefix(cp.History, epoch)+"complete.json")
		if e != nil {
			return zero, e
		}
		var marker Completion
		if Decode(b, &marker) != nil {
			return zero, s.ErrInvalid
		}
		b, e = readArtifact(ctx, w.Objects, marker.Seal)
		if e != nil {
			return zero, e
		}
		var closed Seal
		if Decode(b, &closed) != nil {
			return zero, s.ErrInvalid
		}
		// Event hashes are not transaction order. Merge by entity revision, and
		// reject contradictory values at every repeated revision within the cut.
		seen := map[string]Effect{}
		v := membershipVerifier{objects: w.Objects, history: cp.History, epoch: epoch, visit: func(m Member) error {
			e := s.OutboxEvent{HistoryID: cp.History, Epoch: epoch, ID: m.ID}
			body, err := readArtifact(ctx, w.Objects, AttachmentReference{Key: eventRoot(e) + "descriptor.json", Reference: m.Descriptor})
			if err != nil {
				return err
			}
			var d Descriptor
			if Decode(body, &d) != nil {
				return s.ErrInvalid
			}
			for i, r := range d.Pages {
				body, err = readArtifact(ctx, w.Objects, AttachmentReference{Key: eventRoot(e) + "pages/" + strconv.Itoa(i) + ".json", Reference: r})
				if err != nil {
					return err
				}
				var p EventPage
				if Decode(body, &p) != nil {
					return s.ErrInvalid
				}
				for _, row := range p.Effects {
					// UsersUsage is derived and its revision is object-local, not
					// a user sequence. It cannot be ordered across distinct objects.
					if strings.HasPrefix(row.Key, "UsersUsage/") {
						continue
					}
					if strings.HasPrefix(row.Key, "SystemConfig/") {
						h := row
						h.Key = "PolicyHistory/" + strings.TrimPrefix(row.Key, "SystemConfig/") + fmt.Sprintf("/%020d", row.Revision)
						if prior, ok := image.Rows[h.Key]; ok && prior != h {
							return s.ErrConflict
						}
						image.Rows[h.Key] = h
					}
					key := fmt.Sprintf("%s/%d", row.Key, row.Revision)
					if old, ok := seen[key]; ok && old != row {
						return s.ErrConflict
					}
					seen[key] = row
					if len(seen) > 8*MaxCheckpointRows {
						return s.ErrUnavailable
					}
					old, ok := image.Rows[row.Key]
					if ok && old.Revision == row.Revision && old != row {
						return s.ErrConflict
					}
					if !ok || old.Revision < row.Revision {
						image.Rows[row.Key] = row
						if len(image.Rows) > MaxCheckpointRows {
							return s.ErrUnavailable
						}
					}
				}
			}
			for _, r := range d.Dependencies {
				key := r.Key + "/" + r.Reference.SHA256
				if !depSeen[key] {
					deps = append(deps, Dependency{Event: d.Source, Root: r})
					depSeen[key] = true
				}
				if len(deps) > 64*MaxFanout {
					return s.ErrUnavailable
				}
			}
			return nil
		}}
		if _, e = v.walk(ctx, closed.Membership, -1); e != nil {
			return zero, e
		}
	}
	result := Snapshot{Version: 1, Mode: "historical", History: cp.History, Target: target, Seal: seal, Base: base, Pages: []AttachmentReference{}, Dependencies: []Dependency{}, SourceCatalog: cp.SourceCatalog, SourceCheckpoint: cp.SourceCheckpoint, SourceTarget: cp.SourceTarget, InheritedSupport: cp.InheritedSupport}
	if err := w.writeDependencies(ctx, &result, deps); err != nil {
		return zero, err
	}
	keys := make([]string, 0, len(image.Rows))
	for k := range image.Rows {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	// Split by both count and encoded length. Metadata pages can contain large
	// encrypted credential/projection values, so a count alone is insufficient.
	for at := 0; at < len(keys); {
		p := snapshotPage{Version: 1, History: cp.History, Number: len(result.Pages), Rows: []Effect{}}
		for at < len(keys) && len(p.Rows) < 128 {
			p.Rows = append(p.Rows, image.Rows[keys[at]])
			if _, e := Encode(p); e != nil {
				p.Rows = p.Rows[:len(p.Rows)-1]
				break
			}
			at++
		}
		if len(p.Rows) == 0 || len(result.Pages) >= MaxFanout {
			return zero, s.ErrInvalid
		}
		b, e := Encode(p)
		if e != nil {
			return zero, e
		}
		r := AttachmentReference{Key: "recovery/checkpoints/" + cp.History + "/pages/" + digest(b) + ".json", Reference: ref(b)}
		if e = putVerified(ctx, w.Objects, r.Key, b); e != nil {
			return zero, e
		}
		result.Pages = append(result.Pages, r)
	}
	body, err := Encode(result)
	if err != nil {
		return zero, err
	}
	root := AttachmentReference{Key: "recovery/checkpoints/" + cp.History + "/" + digest(body) + ".json", Reference: ref(body)}
	if err = putVerified(ctx, w.Objects, root.Key, body); err != nil {
		return zero, err
	}
	check, err := w.Load(ctx, root)
	if err != nil {
		return zero, err
	}
	if !reflect.DeepEqual(check.Rows, image.Rows) {
		return zero, s.ErrConflict
	}
	return root, nil
}

func (w Checkpointer) writeDependencies(ctx context.Context, snapshot *Snapshot, deps []Dependency) error {
	seen := map[string]bool{}
	unique := []Dependency{}
	for _, d := range deps {
		key := d.Root.Key + "/" + d.Root.Reference.SHA256
		if !seen[key] {
			seen[key] = true
			unique = append(unique, d)
		}
	}
	for at := 0; at < len(unique); at += 64 {
		if len(snapshot.DependencyPages) >= MaxFanout {
			return s.ErrInvalid
		}
		body, e := Encode(dependencyPage{Version: 1, Dependencies: unique[at:min(at+64, len(unique))]})
		if e != nil {
			return e
		}
		r := AttachmentReference{Key: "recovery/checkpoints/" + snapshot.History + "/dependencies/" + digest(body) + ".json", Reference: ref(body)}
		if e = putVerified(ctx, w.Objects, r.Key, body); e != nil {
			return e
		}
		snapshot.DependencyPages = append(snapshot.DependencyPages, r)
	}
	return nil
}

// An anchor is installed only from the fenced, immutable support catalog.
// Its checksum-bound snapshot was independently replay-verified at publication.
func (w Checkpointer) verifyTarget(ctx context.Context, target uint64) (AttachmentReference, error) {
	if w.anchor == nil || target < w.anchor.Epoch {
		return VerifyCut(ctx, w.Objects, w.Catalog, target)
	}
	body, e := readArtifact(ctx, w.Objects, w.anchor.Checkpoint)
	if e != nil {
		return AttachmentReference{}, e
	}
	var cp Snapshot
	if Decode(body, &cp) != nil || cp.History != w.Catalog.History || cp.Mode != "historical" || cp.BeforeFirst || cp.Target != w.anchor.Epoch {
		return AttachmentReference{}, s.ErrInvalid
	}
	if _, e = readArtifact(ctx, w.Objects, cp.Seal); e != nil {
		return AttachmentReference{}, e
	}
	if target == cp.Target {
		return cp.Seal, nil
	}
	return verifyCutFrom(ctx, w.Objects, w.Catalog, target, cp.Target+1, cp.Seal.Reference.SHA256)
}
