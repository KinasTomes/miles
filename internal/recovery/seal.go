package recovery

import (
	"bytes"
	"context"
	"fmt"
	"reflect"
	"strconv"
	"time"

	s "github.com/KinasTomes/miles/internal/storage"
)

type Seal struct {
	Version        int                 `json:"r2_version"`
	History        string              `json:"history"`
	Epoch          uint64              `json:"epoch"`
	CloseID        string              `json:"close_id"`
	CutAt          string              `json:"cut_at"`
	ClosureSHA256  string              `json:"closure_sha256"`
	Producers      uint64              `json:"producers"`
	Events         uint64              `json:"events"`
	EventsSHA256   string              `json:"events_sha256"`
	Membership     AttachmentReference `json:"membership"`
	PreviousSHA256 string              `json:"previous_sha256"`
}
type Completion struct {
	Version int                 `json:"r2_version"`
	History string              `json:"history"`
	Epoch   uint64              `json:"epoch"`
	Seal    AttachmentReference `json:"seal"`
}

func epochPrefix(h string, e uint64) string {
	return "journal/epochs/" + h + "/" + strconv.FormatUint(e, 10) + "/"
}
func genesisKey(c Catalog) string {
	return "recovery/histories/" + c.History + "/genesis-" + c.Genesis.SHA256 + ".json"
}
func catalogKey(c Catalog, b []byte) string {
	return "recovery/histories/" + c.History + "/catalog-" + digest(b) + ".json"
}

// ProvisionHistory is an explicit initial-history provisioning operation. The
// caller supplies an audited base; it is never inferred from live-table scans.
// Online workers only read the selected catalog and cannot rotate history.
func ProvisionHistory(ctx context.Context, m s.RecoveryStore, objects ImmutableStore, c Catalog, base []byte) error {
	if m == nil || objects == nil || c.Validate() != nil || !reflect.DeepEqual(ref(base), c.Genesis) {
		return s.ErrInvalid
	}
	if err := putVerified(ctx, objects, genesisKey(c), base); err != nil {
		return err
	}
	body, err := Encode(c)
	if err != nil {
		return err
	}
	if err = putVerified(ctx, objects, catalogKey(c, body), body); err != nil {
		return err
	}
	if err = putVerified(ctx, objects, "recovery/active-history.json", body); err != nil {
		return err
	}
	return m.InitializeRecovery(ctx, s.RecoveryState{Version: 1, HistoryID: c.History, Incarnation: c.Incarnation, GenesisSHA256: c.Genesis.SHA256, GenesisLength: c.Genesis.Length, Revision: 1})
}
func verifyCatalog(ctx context.Context, objects ImmutableStore, c Catalog, active bool) error {
	if c.Validate() != nil {
		return s.ErrInvalid
	}
	expected, err := Encode(c)
	if err != nil {
		return err
	}
	keys := []string{catalogKey(c, expected)}
	if active {
		keys = append(keys, "recovery/active-history.json")
	}
	for _, key := range keys {
		body, err := objects.Read(ctx, key)
		if err != nil {
			return err
		}
		if !bytes.Equal(body, expected) {
			return s.ErrConflict
		}
	}
	body, err := objects.Read(ctx, genesisKey(c))
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(ref(body), c.Genesis) {
		return s.ErrInvalid
	}
	return nil
}

type Sealer struct {
	Exporter
	Store   s.RecoveryStore
	Catalog Catalog
}

func (w Sealer) Seal(ctx context.Context, r s.RecoveryCutRequest) (AttachmentReference, error) {
	var zero AttachmentReference
	if w.Store == nil || w.Objects == nil || r.Validate() != nil || r.HistoryID != w.Catalog.History || r.Worker != w.Worker {
		return zero, s.ErrInvalid
	}
	if err := w.Store.CheckRecoveryWorker(ctx, w.Worker); err != nil {
		return zero, err
	}
	if err := verifyCatalog(ctx, w.Objects, w.Catalog, true); err != nil {
		return zero, err
	}
	e, err := w.Store.GetRecoveryEpoch(ctx, r.HistoryID, r.Epoch)
	if err != nil {
		return zero, err
	}
	if e.CloseID != r.CloseID || e.Worker != r.Worker || (e.State != "Closed" && e.State != "Sealed") {
		return zero, s.ErrFenced
	}
	if e.State == "Sealed" {
		cut, err := VerifyCut(ctx, w.Objects, w.Catalog, r.Epoch)
		if err != nil {
			return zero, err
		}
		if cut.Key != e.SealKey || cut.Reference.SHA256 != e.SealSHA256 {
			return zero, s.ErrConflict
		}
		return cut, nil
	}
	state, err := w.Store.GetRecoveryState(ctx, r.HistoryID)
	if err != nil {
		return zero, err
	}
	if state.NextToSeal != r.Epoch || state.GenesisSHA256 != w.Catalog.Genesis.SHA256 || state.GenesisLength != w.Catalog.Genesis.Length {
		return zero, s.ErrConflict
	}
	previous := w.Catalog.Genesis.SHA256
	if r.Epoch > 0 {
		prior, err := VerifyCut(ctx, w.Objects, w.Catalog, r.Epoch-1)
		if err != nil {
			return zero, err
		}
		if prior.Key != state.SealKey || prior.Reference.SHA256 != state.SealSHA256 {
			return zero, s.ErrConflict
		}
		previous = prior.Reference.SHA256
	}
	objects := guardedObjects{w.Objects, func(ctx context.Context) error { return w.Store.CheckRecoveryWorker(ctx, w.Worker) }}
	tree := membershipBuilder{objects: objects, prefix: epochPrefix(r.HistoryID, r.Epoch), history: r.HistoryID, epoch: r.Epoch}
	q := s.OutboxQuery{HistoryID: r.HistoryID, Epoch: r.Epoch, Limit: 128}
	count, last, chain := uint64(0), "", ""
	for {
		page, err := w.Store.ListOutbox(ctx, q)
		if err != nil {
			return zero, err
		}
		for _, event := range page.Events {
			if event.Validate() != nil || event.HistoryID != r.HistoryID || event.Epoch != r.Epoch || event.ID <= last || event.ID <= q.After {
				return zero, s.ErrInvalid
			}
			if err = w.Exporter.Export(ctx, event.ID); err != nil {
				return zero, err
			}
			progress, err := w.Store.GetOutboxExport(ctx, event.ID)
			if err != nil {
				return zero, err
			}
			if !reflect.DeepEqual(progress.Event, event) || progress.DescriptorKey != eventRoot(event)+"descriptor.json" {
				return zero, s.ErrConflict
			}
			body, err := w.Objects.Read(ctx, progress.DescriptorKey)
			if err != nil {
				return zero, err
			}
			if digest(body) != progress.DescriptorSHA256 {
				return zero, s.ErrInvalid
			}
			if err = VerifyEvent(ctx, w.Objects, Member{ID: event.ID, Descriptor: ref(body)}, r.HistoryID, r.Epoch); err != nil {
				return zero, err
			}
			if err = tree.add(ctx, Member{ID: event.ID, Descriptor: ref(body)}); err != nil {
				return zero, err
			}
			chain, err = s.RecoveryDigest(chain, event)
			if err != nil {
				return zero, err
			}
			last = event.ID
			count++
		}
		if page.Next == "" {
			break
		}
		if page.Next <= q.After || page.Next < last || !isHex(page.Next, 64) {
			return zero, s.ErrInvalid
		}
		q.After = page.Next
	}
	// Independently obtained at closure from atomically registered mutations.
	// A missing/extra member or a filtered/truncated discovery page cannot seal.
	if count != e.EventCount || chain != e.EventsSHA256 {
		return zero, fmt.Errorf("%w: exact closed event membership", s.ErrConflict)
	}
	root, err := tree.finish(ctx)
	if err != nil {
		return zero, err
	}
	seal := Seal{Version: 1, History: r.HistoryID, Epoch: r.Epoch, CloseID: r.CloseID, CutAt: e.CutAt.UTC().Format(time.RFC3339Nano), ClosureSHA256: e.ProofSHA256, Producers: e.ProducerCount, Events: count, EventsSHA256: chain, Membership: root, PreviousSHA256: previous}
	body, err := Encode(seal)
	if err != nil {
		return zero, err
	}
	result := AttachmentReference{Key: epochPrefix(r.HistoryID, r.Epoch) + "seal-" + digest(body) + ".json", Reference: ref(body)}
	if err = putVerified(ctx, objects, result.Key, body); err != nil {
		return zero, err
	}
	marker, err := Encode(Completion{Version: 1, History: r.HistoryID, Epoch: r.Epoch, Seal: result})
	if err != nil {
		return zero, err
	}
	if err = putVerified(ctx, objects, epochPrefix(r.HistoryID, r.Epoch)+"complete.json", marker); err != nil {
		return zero, err
	}
	if _, err = VerifyCut(ctx, w.Objects, w.Catalog, r.Epoch); err != nil {
		return zero, err
	}
	if err = w.Store.PublishRecoverySeal(ctx, r, e.Revision, result.Key, result.Reference.SHA256); err != nil {
		return zero, err
	}
	return result, nil
}

// VerifyCut reads immutable artifacts only, including every contiguous link and
// event dependency. It proves journal completeness, NOT physical GC retention,
// checkpoint replay or readiness of a recovered installation.
func VerifyCut(ctx context.Context, objects ImmutableStore, c Catalog, target uint64) (AttachmentReference, error) {
	var result AttachmentReference
	if objects == nil || target >= maxInteger {
		return result, s.ErrInvalid
	}
	if err := verifyCatalog(ctx, objects, c, false); err != nil {
		return result, err
	}
	previous := c.Genesis.SHA256
	for epoch := uint64(0); epoch <= target; epoch++ {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		body, err := objects.Read(ctx, epochPrefix(c.History, epoch)+"complete.json")
		if err != nil {
			return result, err
		}
		var marker Completion
		if Decode(body, &marker) != nil || marker.Version != 1 || marker.History != c.History || marker.Epoch != epoch || marker.Seal.Key != epochPrefix(c.History, epoch)+"seal-"+marker.Seal.Reference.SHA256+".json" {
			return result, s.ErrInvalid
		}
		body, err = readArtifact(ctx, objects, marker.Seal)
		if err != nil {
			return result, err
		}
		var seal Seal
		if Decode(body, &seal) != nil || seal.Version != 1 || seal.History != c.History || seal.Epoch != epoch || !isHex(seal.CloseID, 32) || seal.PreviousSHA256 != previous || seal.Events > maxInteger || seal.Producers > maxInteger || (seal.Producers == 0) != (seal.ClosureSHA256 == "") || seal.ClosureSHA256 != "" && !isHex(seal.ClosureSHA256, 64) || (seal.Events == 0) != (seal.EventsSHA256 == "") {
			return result, s.ErrInvalid
		}
		cut, err := time.Parse(time.RFC3339Nano, seal.CutAt)
		if err != nil || cut.IsZero() || seal.CutAt != cut.UTC().Format(time.RFC3339Nano) {
			return result, s.ErrInvalid
		}
		scan := membershipVerifier{objects: objects, history: c.History, epoch: epoch}
		count, err := scan.walk(ctx, seal.Membership, -1)
		if err != nil {
			return result, err
		}
		if count != seal.Events || scan.chain != seal.EventsSHA256 {
			return result, s.ErrInvalid
		}
		previous = marker.Seal.Reference.SHA256
		result = marker.Seal
	}
	return result, nil
}
func readArtifact(ctx context.Context, objects ImmutableStore, r AttachmentReference) ([]byte, error) {
	if r.Key == "" || r.Reference.Validate() != nil {
		return nil, s.ErrInvalid
	}
	b, err := objects.Read(ctx, r.Key)
	if err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(ref(b), r.Reference) {
		return nil, s.ErrInvalid
	}
	return b, nil
}
