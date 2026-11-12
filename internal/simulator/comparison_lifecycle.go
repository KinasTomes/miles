package simulator

import (
	"fmt"
	"math"
	"math/big"
	"time"

	"github.com/KinasTomes/miles/internal/storage"
)

// ratGBToBytes performs exact bounded conversion from a rational GB quantity to integer bytes:
// bytes = (q.Num * bytesPerGB) / q.Denom.
// It avoids floating-point precision loss and verifies uint64 bounds.
func ratGBToBytes(q *big.Rat, bytesPerGB uint64) (uint64, bool) {
	if q == nil || q.Sign() <= 0 || bytesPerGB == 0 {
		return 0, false
	}
	num := new(big.Int).Mul(q.Num(), new(big.Int).SetUint64(bytesPerGB))
	bytesInt := new(big.Int).Quo(num, q.Denom())
	if !bytesInt.IsUint64() {
		return 0, false
	}
	return bytesInt.Uint64(), true
}

// UnprovenColdReason represents a specific failure to prove cold pack cost or lifecycle evidence.
type UnprovenColdReason string

const (
	UnprovenColdIdentity        UnprovenColdReason = "unproven_cold_identity"
	UnprovenColdRetention       UnprovenColdReason = "unproven_cold_retention"
	UnprovenColdStorageEvidence UnprovenColdReason = "unproven_cold_storage_evidence"
	UnprovenColdRestoreDuration UnprovenColdReason = "unproven_cold_restore_duration"
	UnprovenColdReactivation    UnprovenColdReason = "unproven_cold_reactivation"
)

func (r UnprovenColdReason) OmissionText() string {
	switch r {
	case UnprovenColdIdentity:
		return "Glacier accounting unproven: cold pack operation lacks unique pack Ref.ID identity"
	case UnprovenColdRetention:
		return "Glacier retention fee unproven: cold pack deleted or retiring without verifiable retention deadline"
	case UnprovenColdStorageEvidence:
		return "Glacier storage unproven: cold storage inventory observed without per-pack tiering transition history"
	case UnprovenColdRestoreDuration:
		return "Glacier restore storage unproven: restore usage without authoritative duration or expiry observation"
	case UnprovenColdReactivation:
		return "Glacier restore unproven: expired restore window cannot be reactivated by metadata without physical restore usage"
	default:
		return fmt.Sprintf("Glacier accounting unproven: %s", string(r))
	}
}

// PackRestoreLifecycle tracks the active temporary restored Standard copy of a cold pack.
type PackRestoreLifecycle struct {
	PackRef        storage.PackRef
	StartNS        int64
	DeadlineNS     int64
	Bytes          uint64
	RetrievedBytes uint64
	Expired        bool
}

// IsActiveAt returns true if the restored copy has not expired at or before atNS.
func (l *PackRestoreLifecycle) IsActiveAt(atNS int64) bool {
	if l == nil || l.Expired {
		return false
	}
	return atNS < l.DeadlineNS
}

// ApplyAuthoritativeMetadata updates the lifecycle deadline from authoritative observation evidence.
// Returns unprovenReactivation=true if metadata claims a future restore window for an already-expired restore without restore usage.
func (l *PackRestoreLifecycle) ApplyAuthoritativeMetadata(atNS int64, packRestoredUntil time.Time, traceStart time.Time, restoredInThisEvent bool) (unprovenReactivation bool) {
	if l == nil {
		return false
	}
	if packRestoredUntil.IsZero() || traceStart.IsZero() {
		return false
	}

	eventTime := traceStart.Add(time.Duration(atNS))
	isFutureExpiry := packRestoredUntil.After(eventTime)
	packExpiryNS := packRestoredUntil.Sub(traceStart).Nanoseconds()

	if restoredInThisEvent {
		// Physical restore usage occurred in this event: apply authoritative pack metadata.
		if !isFutureExpiry {
			l.DeadlineNS = atNS
			l.Expired = true
		} else {
			l.DeadlineNS = packExpiryNS
			l.Expired = false
		}
		return false
	}

	// No physical restore usage in this event:
	if l.Expired || atNS >= l.DeadlineNS {
		// Restore was already expired: metadata cannot reactivate an expired restore without a physical restore event.
		if isFutureExpiry {
			unprovenReactivation = true
		}
		l.Expired = true
	} else {
		// Restore is currently active: update expiry according to authoritative observed evidence (shorter or longer).
		if !isFutureExpiry {
			l.DeadlineNS = atNS
			l.Expired = true
		} else {
			l.DeadlineNS = packExpiryNS
		}
	}
	return unprovenReactivation
}

// RestoreTracker tracks all pack restore lifecycles across the timeline.
// It is the shared single source of truth for both metrics (restore amplification)
// and replay (temporary restored Standard GB-Mo calculation), ensuring complete semantic parity.
type RestoreTracker struct {
	Packs           map[storage.PackRef]*PackRestoreLifecycle
	anonCounter     int
	HasUnprovenCold bool
	UnprovenReasons map[UnprovenColdReason]bool
}

func NewRestoreTracker() *RestoreTracker {
	return &RestoreTracker{
		Packs:           make(map[storage.PackRef]*PackRestoreLifecycle),
		UnprovenReasons: make(map[UnprovenColdReason]bool),
	}
}

func (t *RestoreTracker) MarkUnproven(reason UnprovenColdReason) {
	t.HasUnprovenCold = true
	t.UnprovenReasons[reason] = true
}

func (t *RestoreTracker) ExpirePastDeadlines(atNS int64) {
	for _, p := range t.Packs {
		if atNS >= p.DeadlineNS {
			p.Expired = true
		}
	}
}

func (t *RestoreTracker) RegisterRestore(eventIdx int, event Event, trace Trace, retrievedBytes uint64, packBytes uint64) (*PackRestoreLifecycle, bool) {
	restoreDur, ok := trace.RestoreDuration(event)
	if !ok {
		t.MarkUnproven(UnprovenColdRestoreDuration)
		return nil, false
	}
	durNS := restoreDur.Nanoseconds()
	if durNS <= 0 || durNS > math.MaxInt64-event.AtNS {
		t.MarkUnproven(UnprovenColdRestoreDuration)
		return nil, false
	}
	expiry := event.AtNS + durNS

	var packRef storage.PackRef
	if event.Tiering != nil {
		packRef = event.Tiering.Pack.Ref
		if !event.Tiering.Pack.RestoredUntil.IsZero() && !trace.Start.IsZero() {
			eventTime := trace.Start.Add(time.Duration(event.AtNS))
			if !event.Tiering.Pack.RestoredUntil.After(eventTime) {
				expiry = event.AtNS
			} else {
				expiry = event.Tiering.Pack.RestoredUntil.Sub(trace.Start).Nanoseconds()
			}
		}
	}
	if packRef.ID == "" {
		t.anonCounter++
		packRef = storage.PackRef{ID: fmt.Sprintf("__anon_restore_%d_%d", eventIdx, t.anonCounter)}
	}

	pInfo := t.Packs[packRef]
	if pInfo == nil {
		pInfo = &PackRestoreLifecycle{
			PackRef:    packRef,
			StartNS:    event.AtNS,
			DeadlineNS: expiry,
			Bytes:      packBytes,
			Expired:    (event.AtNS >= expiry),
		}
		t.Packs[packRef] = pInfo
	} else {
		if packBytes > 0 {
			pInfo.Bytes = packBytes
		}
		if expiry > pInfo.DeadlineNS {
			pInfo.DeadlineNS = expiry
			pInfo.Expired = (event.AtNS >= expiry)
		}
	}

	if retrievedBytes > math.MaxUint64-pInfo.RetrievedBytes {
		t.MarkUnproven(UnprovenColdRestoreDuration)
		return nil, false
	}
	pInfo.RetrievedBytes += retrievedBytes
	return pInfo, true
}

func (t *RestoreTracker) ApplyAuthoritativeMetadata(event Event, trace Trace, restoredInThisEvent map[storage.PackRef]bool) {
	if event.Tiering == nil {
		return
	}
	packRef := event.Tiering.Pack.Ref
	pInfo := t.Packs[packRef]
	if pInfo == nil || pInfo.RetrievedBytes == 0 {
		return
	}

	if event.Tiering.Pack.State != "" && event.Tiering.Pack.State != "Restored" && event.Tiering.Pack.State != "Restoring" {
		pInfo.Expired = true
		pInfo.DeadlineNS = event.AtNS
	}

	if !event.Tiering.Pack.RestoredUntil.IsZero() && !trace.Start.IsZero() {
		unprovenReactivation := pInfo.ApplyAuthoritativeMetadata(event.AtNS, event.Tiering.Pack.RestoredUntil, trace.Start, restoredInThisEvent[packRef])
		if unprovenReactivation {
			t.MarkUnproven(UnprovenColdReactivation)
		}
	}
}

// ResolvePackForDelivery resolves the pack responsible for a client cold read.
// Invariant: If pack Ref.ID is given, look up that exact pack.
// Invariant: If no pack Ref.ID is given, delivery can only be proven if exactly ONE pack was restored in the trace.
func (t *RestoreTracker) ResolvePackForDelivery(event Event) (*PackRestoreLifecycle, bool) {
	if event.Tiering != nil && event.Tiering.Pack.Ref.ID != "" {
		pInfo := t.Packs[event.Tiering.Pack.Ref]
		if pInfo == nil || pInfo.RetrievedBytes == 0 {
			return nil, false
		}
		return pInfo, true
	}

	// Trace event lacks pack Ref. Check if there is a single restored pack.
	var solePack *PackRestoreLifecycle
	numRestored := 0
	for _, p := range t.Packs {
		if p.RetrievedBytes > 0 {
			numRestored++
			solePack = p
		}
	}
	if numRestored == 1 {
		return solePack, true
	}
	return nil, false
}

// CheckDeliveryProvenance validates the delivery claim for an event under polName.
func (t *RestoreTracker) CheckDeliveryProvenance(event Event, polName PolicyName, trace Trace, clientTraffic uint64, hasColdUsageInEvent bool, coldClassesWithRetrievalInEvent int, coldClassesInTrace int) (delivered uint64, ok bool) {
	var targetClass storage.StorageClass
	switch polName {
	case PolicyWarmGlacierIR:
		targetClass = storage.GlacierIR
	case PolicyWarmGlacierFlex:
		targetClass = storage.Glacier
	default:
		return 0, false
	}

	b, hasDeliveryClaim := event.ColdDeliveredForClass(targetClass)
	if hasDeliveryClaim {
		if len(event.ColdLogicalDeliveredByClass) > 0 {
			delivered = b
		} else {
			if hasColdUsageInEvent && coldClassesWithRetrievalInEvent == 1 {
				delivered = b
			} else if !hasColdUsageInEvent && coldClassesInTrace == 1 {
				delivered = b
			} else {
				hasDeliveryClaim = false
			}
		}
	}

	if hasColdUsageInEvent {
		if clientTraffic > 0 && !hasDeliveryClaim {
			t.MarkUnproven(UnprovenColdIdentity)
			return 0, false
		}
	}

	if hasDeliveryClaim {
		if polName == PolicyWarmGlacierFlex {
			if event.Tiering != nil && !event.Tiering.Pack.RestoredUntil.IsZero() && !trace.Start.IsZero() {
				eventTime := trace.Start.Add(time.Duration(event.AtNS))
				if !event.Tiering.Pack.RestoredUntil.After(eventTime) {
					t.MarkUnproven(UnprovenColdRestoreDuration)
					return 0, false
				}
			}

			packInfo, resolved := t.ResolvePackForDelivery(event)
			if !resolved || packInfo == nil || packInfo.RetrievedBytes == 0 {
				t.MarkUnproven(UnprovenColdIdentity)
				return 0, false
			}
			if packInfo.Expired || event.AtNS >= packInfo.DeadlineNS {
				t.MarkUnproven(UnprovenColdRestoreDuration)
				return 0, false
			}
		} else if polName == PolicyWarmGlacierIR {
			if !hasColdUsageInEvent {
				t.MarkUnproven(UnprovenColdIdentity)
				return 0, false
			}
		}

		if clientTraffic == 0 {
			t.MarkUnproven(UnprovenColdIdentity)
			return 0, false
		}
		totalColdDelivered, validTotal := event.TotalColdDeliveredBytes()
		if !validTotal || totalColdDelivered > clientTraffic || delivered > clientTraffic {
			t.MarkUnproven(UnprovenColdIdentity)
			return 0, false
		}
		return delivered, true
	}

	totalColdDelivered, validTotal := event.TotalColdDeliveredBytes()
	if !validTotal || (totalColdDelivered > 0 && totalColdDelivered > clientTraffic) {
		t.MarkUnproven(UnprovenColdIdentity)
		return 0, false
	}
	return 0, true
}
