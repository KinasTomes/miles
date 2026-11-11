package recovery

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/KinasTomes/miles/internal/lifecycle"
	"github.com/KinasTomes/miles/internal/recovery/format"
	s "github.com/KinasTomes/miles/internal/storage"
)

// PITRSource exports an immutable historical image for the requested exact cut.
// A provider may use PITR only with a shared watermark across every table. Its
// result is checked against reference sealed replay before it can be installed.
type PITRSource interface {
	AtCut(context.Context, SupportedCut) (AttachmentReference, error)
}
type Recoverer struct {
	Retention
	Installer s.IndexInstaller
	PITR      PITRSource
}
type RecoverRequest struct {
	Target               time.Time
	Bounds               s.R1Bounds
	History, Incarnation string
}

func (w Recoverer) Recover(ctx context.Context, r RecoverRequest) (Catalog, error) {
	started := time.Now()
	var zero Catalog
	if w.Installer == nil || !isHex(r.History, 32) || !isHex(r.Incarnation, 32) || r.History == w.Catalog.History || r.Incarnation == w.Catalog.Incarnation {
		return zero, s.ErrInvalid
	}
	if body, e := w.Objects.Read(ctx, "recovery/active/"+r.Incarnation+".json"); e == nil {
		var frozen Catalog
		if Decode(body, &frozen) != nil || frozen.History != r.History || frozen.Incarnation != r.Incarnation {
			return zero, s.ErrConflict
		}
		image, e := (Checkpointer{Objects: w.Objects, Catalog: frozen}).Load(ctx, AttachmentReference{Key: genesisKey(frozen), Reference: frozen.Genesis})
		if e != nil {
			return zero, e
		}
		if image.Snapshot.SourceCatalog != w.Catalog || image.Snapshot.RequestedTarget != r.Target.UTC().Format(time.RFC3339Nano) {
			return zero, s.ErrConflict
		}
		current, e := w.Rebuild(ctx, image.Snapshot.SourceCheckpoint)
		if e != nil {
			return zero, e
		}
		if e = current.VerifyPayload(ctx, w.Packs); e != nil {
			return zero, e
		}
		return w.installFrozen(ctx, frozen, SupportedCut{Epoch: image.Snapshot.SourceTarget, Checkpoint: image.Snapshot.SourceCheckpoint}, ageRecoveryBounds(r.Bounds, time.Since(started)))
	} else if !errors.Is(e, s.ErrNotFound) {
		return zero, e
	}
	cut, err := w.Select(ctx, r.Target, r.Bounds)
	if err != nil {
		return zero, err
	}
	reference, err := w.Load(ctx, cut.Checkpoint)
	if err != nil {
		return zero, err
	}
	chosen := cut.Checkpoint
	if w.PITR != nil {
		candidate, e := w.PITR.AtCut(ctx, cut)
		if e == nil {
			image, e := w.Load(ctx, candidate)
			if e != nil {
				return zero, e
			}
			if image.Snapshot.Target != cut.Epoch || image.Snapshot.BeforeFirst || !reflect.DeepEqual(image.Rows, reference.Rows) || !reflect.DeepEqual(image.Dependencies, reference.Dependencies) {
				return zero, s.ErrConflict
			}
			chosen = candidate
		} else if !errors.Is(e, s.ErrNotFound) && !errors.Is(e, s.ErrUnavailable) {
			return zero, e
		}
	}
	rebuilt, err := w.Rebuild(ctx, chosen)
	if err != nil {
		return zero, err
	}
	if err = rebuilt.VerifyPayload(ctx, w.Packs); err != nil {
		return zero, err
	}
	// A new historical root records installation normalization explicitly. It
	// retains the original target and exact source checkpoint as provenance.
	genesis := Snapshot{Version: 1, Mode: "normalized-genesis", RequestedTarget: r.Target.UTC().Format(time.RFC3339Nano), History: r.History, BeforeFirst: true, Pages: []AttachmentReference{}, Dependencies: []Dependency{}, SourceCatalog: w.Catalog, SourceCheckpoint: cut.Checkpoint, SourceTarget: cut.Epoch}
	inherited, _, err := w.Read(ctx)
	if err != nil {
		return zero, err
	}
	genesis.InheritedSupport = inherited
	if err = w.writeDependencies(ctx, &genesis, reference.Dependencies); err != nil {
		return zero, err
	}
	keys := make([]string, 0, len(reference.Rows))
	for k := range reference.Rows {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	rows := []Effect{}
	physicalPacks := map[string]s.PackInfo{}
	for key := range rebuilt.Live {
		ref := rebuilt.Chunks[key].Location.Pack
		id := ref.ID + "/" + ref.Generation
		if _, ok := physicalPacks[id]; ok {
			continue
		}
		p := rebuilt.Packs[ref]
		observed, e := w.Packs.HeadPack(ctx, ref)
		if e != nil {
			return zero, e
		}
		p.Class = observed.Class
		p.RestoredUntil = observed.RestoredUntil
		p.StateRevision++
		if p.StateRevision > maxInteger {
			return zero, s.ErrInvalid
		}
		switch p.Class {
		case s.Standard, s.StandardIA:
			p.State = "Warm"
		case s.GlacierIR:
			p.State = "Cold"
		case s.Glacier:
			p.State = "Restored"
		default:
			return zero, s.ErrInvalid
		}
		physicalPacks[id] = p
	}
	for _, key := range keys {
		row := reference.Rows[key]
		kind, id, value, e := projection(row)
		if e != nil {
			return zero, e
		}
		if kind == "QuotaReservations" || kind == "ProducerScope" || kind == "UsersUsage" {
			continue
		}
		if kind == "Chunks" {
			// The replacement index has no row for a retired winner. Its new
			// history must also allow the first insertion to start at revision 1.
			if row.Deleted {
				continue
			}
			var chunk s.Chunk
			if strictJSON(value, &chunk) != nil {
				return zero, s.ErrInvalid
			}
			if !rebuilt.Live[chunk.Key] {
				continue
			}
		}
		if !row.Deleted {
			var normalized any
			switch kind {
			case "Packs":
				if p, ok := physicalPacks[id]; ok {
					row.Revision = p.StateRevision
					normalized = struct {
						Version  uint32
						Ref      s.PackRef
						Size     int64
						Class    s.StorageClass
						State    string
						Revision uint64
					}{1, p.Ref, p.Size, p.Class, p.State, p.StateRevision}
				}
			case "PackLifecycleFacts":
				if _, ok := physicalPacks[id]; ok {
					continue
				}
			case "Credentials":
				var v struct {
					Credential s.Credential
					Security   s.CredentialSecurity
				}
				if strictJSON(value, &v) != nil {
					return zero, s.ErrInvalid
				}
				v.Credential.Active = false
				normalized = v
			case "AdminTokens":
				var v s.AdminToken
				if strictJSON(value, &v) != nil {
					return zero, s.ErrInvalid
				}
				v.Active = false
				normalized = v
			case "CompletedUpload", "Uploads":
				var v s.MultipartUpload
				if strictJSON(value, &v) != nil {
					return zero, s.ErrInvalid
				}
				v.Writer = s.WriterFence{}
				v.ClosedWriter = s.WriterFence{}
				v.LeaseUntil = time.Time{}
				v.Publishers = 0
				v.PublishedBytes = 0
				v.ProvisionalBytes = 0
				if v.State != "Completed" && v.State != "Aborted" {
					v.State = "Aborted"
					v.Revision++
					v.Prepared = s.ObjectHead{}
					row.Revision = v.Revision
				}
				normalized = v
			case "PackLifecycle":
				var v lifecycle.Action
				if strictJSON(value, &v) != nil || v.ValidateSchema() != nil {
					return zero, s.ErrInvalid
				}
				v.Fence = s.WriterFence{}
				v.Source.Writer = s.WriterFence{}
				v.ResultPack.Writer = s.WriterFence{}
				v.Chunks = nil
				normalized = v
			}
			if normalized != nil {
				v, e := json.Marshal(normalized)
				if e != nil {
					return zero, e
				}
				body, e := json.Marshal(format.Projection{Version: 1, Entity: kind, Value: v})
				if e != nil {
					return zero, e
				}
				row.Value = base64.StdEncoding.EncodeToString(body)
			}
		}
		rows = append(rows, row)
	}
	for id, p := range physicalPacks {
		value, _ := json.Marshal(struct {
			Version                          uint32
			Ref                              s.PackRef
			RestoredUntil, MinRetentionUntil time.Time
		}{1, p.Ref, p.RestoredUntil, p.MinRetentionUntil})
		body, e := json.Marshal(format.Projection{Version: 1, Entity: "PackLifecycleFacts", Value: value})
		if e != nil {
			return zero, e
		}
		rows = append(rows, Effect{Key: "PackLifecycleFacts/" + hex.EncodeToString([]byte(id)), Revision: p.StateRevision, Value: base64.StdEncoding.EncodeToString(body)})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Key < rows[j].Key })
	for at := 0; at < len(rows); {
		page := snapshotPage{Version: 1, History: r.History, Number: len(genesis.Pages), Rows: []Effect{}}
		for at < len(rows) && len(page.Rows) < 128 {
			page.Rows = append(page.Rows, rows[at])
			if _, e := Encode(page); e != nil {
				page.Rows = page.Rows[:len(page.Rows)-1]
				break
			}
			at++
		}
		if len(page.Rows) == 0 || len(genesis.Pages) >= MaxFanout {
			return zero, s.ErrInvalid
		}
		body, e := Encode(page)
		if e != nil {
			return zero, e
		}
		ref := AttachmentReference{Key: "recovery/histories/" + r.History + "/pages/" + digest(body) + ".json", Reference: ref(body)}
		if e = putVerified(ctx, w.Objects, ref.Key, body); e != nil {
			return zero, e
		}
		genesis.Pages = append(genesis.Pages, ref)
	}
	body, err := Encode(genesis)
	if err != nil {
		return zero, err
	}
	catalog := Catalog{Version: 1, History: r.History, Incarnation: r.Incarnation, Genesis: ref(body)}
	if err = putVerified(ctx, w.Objects, genesisKey(catalog), body); err != nil {
		return zero, err
	}
	encoded, err := Encode(catalog)
	if err != nil {
		return zero, err
	}
	if err = putVerified(ctx, w.Objects, catalogKey(catalog, encoded), encoded); err != nil {
		return zero, err
	}
	if err = putVerified(ctx, w.Objects, selectedCatalogKey(catalog), encoded); err != nil {
		return zero, err
	}
	return w.installFrozen(ctx, catalog, cut, ageRecoveryBounds(r.Bounds, time.Since(started)))
}
func (w Recoverer) installFrozen(ctx context.Context, catalog Catalog, cut SupportedCut, bounds s.R1Bounds) (Catalog, error) {
	started := time.Now()
	var zero Catalog
	r := RecoverRequest{History: catalog.History, Incarnation: catalog.Incarnation}
	// Derive installation rows from the exact new genesis, not from the old
	// credential-active historical image or an operator-provided replacement.
	next, err := (Checkpointer{Objects: w.Objects, Catalog: catalog}).Rebuild(ctx, AttachmentReference{Key: genesisKey(catalog), Reference: catalog.Genesis})
	if err != nil {
		return zero, err
	}
	if err = w.checkFrozenReadiness(ctx, next, ageRecoveryBounds(bounds, time.Since(started))); err != nil {
		return zero, err
	}
	chain := ""
	for _, record := range next.Records {
		chain, err = s.IndexRecordDigest(chain, record)
		if err != nil {
			return zero, err
		}
	}
	request := s.IndexInstall{SourceHistory: w.Catalog.History, SourceIncarnation: w.Catalog.Incarnation, TargetSHA256: cut.Checkpoint.Reference.SHA256, History: r.History, Incarnation: r.Incarnation, GenesisSHA256: catalog.Genesis.SHA256, GenesisLength: catalog.Genesis.Length, RecordsSHA256: chain}
	if err = w.Installer.BeginIndexInstall(ctx, request); err != nil {
		return zero, err
	}
	for _, record := range next.Records {
		if strings.Contains(record.Kind, "lease") {
			return zero, s.ErrInvalid
		}
		if err = w.Installer.StageIndexRecord(ctx, request, record); err != nil {
			return zero, fmt.Errorf("%w: staging %s", err, record.Kind)
		}
	}
	if err = w.checkFrozenReadiness(ctx, next, ageRecoveryBounds(bounds, time.Since(started))); err != nil {
		return zero, err
	}
	horizonDeadline := started.Add(bounds.Horizon.Sub(bounds.Upper))
	var archiveDeadline time.Time
	for key := range next.Live {
		p := next.Packs[next.Chunks[key].Location.Pack]
		if p.Class == s.Glacier {
			deadline := started.Add(p.RestoredUntil.Sub(bounds.Upper))
			if archiveDeadline.IsZero() || deadline.Before(archiveDeadline) {
				archiveDeadline = deadline
			}
		}
	}
	validUntil := horizonDeadline
	if !archiveDeadline.IsZero() && archiveDeadline.Before(validUntil) {
		validUntil = archiveDeadline
	}
	finalContext := s.WithIndexReadiness(ctx, request, validUntil, func(ctx context.Context) error {
		// Native invokes this after isolation proof, metadata reads and transaction
		// preparation. Refresh physical facts first, then do only constant-time local
		// deadline checks immediately before SDK dispatch.
		if e := w.checkFrozenReadiness(ctx, next, ageRecoveryBounds(bounds, time.Since(started))); e != nil {
			return e
		}
		now := time.Now()
		if !now.Before(horizonDeadline) {
			return s.ErrFenced
		}
		if !archiveDeadline.IsZero() && !now.Before(archiveDeadline) {
			return s.ErrCold
		}
		return nil
	})
	if err = w.Installer.FinishIndexInstall(finalContext, request); err != nil {
		return zero, err
	}
	return catalog, nil
}

// The immutable plan is never rebased to mutable HEAD facts. If its published
// read policy no longer matches physical readiness, leave installation closed.
func (w Recoverer) checkFrozenReadiness(ctx context.Context, index Rebuilt, b s.R1Bounds) error {
	started := time.Now()
	if b.Upper.IsZero() || b.Lower.IsZero() || b.Upper.Before(b.Lower) || b.Horizon.Before(b.Upper) {
		return s.ErrInvalid
	}
	seen := map[s.PackRef]bool{}
	var earliestRestore time.Time
	for key := range index.Live {
		ref := index.Chunks[key].Location.Pack
		if seen[ref] {
			continue
		}
		seen[ref] = true
		frozen := index.Packs[ref]
		current, e := w.Packs.HeadPack(ctx, ref)
		if e != nil {
			return e
		}
		if current.Ref != ref || current.Size != frozen.Size || current.Class != frozen.Class || !current.RestoredUntil.Equal(frozen.RestoredUntil) {
			return fmt.Errorf("%w: frozen pack facts require offline reconciliation", s.ErrConflict)
		}
		if !lifecycle.Readable(frozen, b.Upper) {
			return s.ErrCold
		}
		if frozen.Class == s.Glacier && (earliestRestore.IsZero() || frozen.RestoredUntil.Before(earliestRestore)) {
			earliestRestore = frozen.RestoredUntil
		}
	}
	currentBounds := ageRecoveryBounds(b, time.Since(started))
	if currentBounds.Horizon.Before(currentBounds.Upper) {
		return s.ErrInvalid
	}
	if !earliestRestore.IsZero() && !earliestRestore.After(currentBounds.Upper) {
		return s.ErrCold
	}
	return nil
}

// Advance the supplied trusted sample with monotonic elapsed time. Its original
// horizon stays fixed, so long verification/staging cannot reuse expired bounds.
func ageRecoveryBounds(b s.R1Bounds, elapsed time.Duration) s.R1Bounds {
	if b.Lower.IsZero() || b.Upper.IsZero() {
		return b
	}
	b.Lower = b.Lower.Add(elapsed)
	b.Upper = b.Upper.Add(elapsed)
	return b
}
