package simulator

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/KinasTomes/miles/internal/cost"
	"github.com/KinasTomes/miles/internal/pricing"
	"github.com/KinasTomes/miles/internal/storage"
)

type timelineReplayResult struct {
	currentLogical             uint64
	finalPhysical              uint64
	directTotals               map[string]*big.Rat
	warmStdTotals              map[string]*big.Rat
	glacierIRTotals            map[string]*big.Rat
	glacierFlexTotals          map[string]*big.Rat
	directClassMap             map[string]uint64
	warmStdClassMap            map[string]uint64
	glacierIRClassMap          map[string]uint64
	glacierFlexClassMap        map[string]uint64
	temporaryRestoredBytes     uint64
	hasObservedCold            bool
	unprovenColdReasons        map[UnprovenColdReason]bool
	hasStorageInconsistency    bool
	earlyDeletionUsageByPolicy map[PolicyName][]cost.Usage
	earlyDeletionUSDByPolicy   map[PolicyName]*big.Rat
	monthDuration              time.Duration
}

type timelineReplayState struct {
	previous                   int64
	currentLogical             uint64
	currentPhysical            uint64
	currentStorageBytes        map[storage.StorageClass]uint64
	hasObservedCold            bool
	unprovenColdReasons        map[UnprovenColdReason]bool
	hasStorageInconsistency    bool
	activeColdPacks            map[string]storage.PackInfo
	earlyDeletionUsageByPolicy map[PolicyName][]cost.Usage
	earlyDeletionUSDByPolicy   map[PolicyName]*big.Rat
	earlyDeletedPacks          map[string]bool
	unprovenRetentionPacks     map[string]bool
	directTotals               map[string]*big.Rat
	warmStdTotals              map[string]*big.Rat
	glacierIRTotals            map[string]*big.Rat
	glacierFlexTotals          map[string]*big.Rat
	restoreCopies              []*PackRestoreLifecycle
	temporaryRestoredBytes     uint64
	monthDuration              time.Duration
}

func initReplayState(baseResult Result, trace Trace) *timelineReplayState {
	var initialLogical, initialPhysical uint64
	if baseResult.Snapshot != nil {
		initialLogical = baseResult.Snapshot.Logical
		initialPhysical = baseResult.Snapshot.Physical
	} else if len(trace.Events) > 0 && trace.Events[0].Snapshot != nil {
		initialLogical = trace.Events[0].Snapshot.Logical
		initialPhysical = trace.Events[0].Snapshot.Physical
	}

	monthDuration := time.Duration(trace.MonthSeconds) * time.Second

	return &timelineReplayState{
		currentLogical:         initialLogical,
		currentPhysical:        initialPhysical,
		currentStorageBytes:    map[storage.StorageClass]uint64{},
		unprovenColdReasons:    make(map[UnprovenColdReason]bool),
		activeColdPacks:        make(map[string]storage.PackInfo),
		earlyDeletedPacks:      make(map[string]bool),
		unprovenRetentionPacks: make(map[string]bool),
		directTotals:           map[string]*big.Rat{},
		warmStdTotals:          map[string]*big.Rat{},
		glacierIRTotals:        map[string]*big.Rat{},
		glacierFlexTotals:      map[string]*big.Rat{},
		earlyDeletionUsageByPolicy: map[PolicyName][]cost.Usage{
			PolicyDirectS3:         {},
			PolicyWarmStandardOnly: {},
			PolicyWarmGlacierIR:    {},
			PolicyWarmGlacierFlex:  {},
		},
		earlyDeletionUSDByPolicy: map[PolicyName]*big.Rat{
			PolicyDirectS3:         new(big.Rat),
			PolicyWarmStandardOnly: new(big.Rat),
			PolicyWarmGlacierIR:    new(big.Rat),
			PolicyWarmGlacierFlex:  new(big.Rat),
		},
		monthDuration: monthDuration,
	}
}

func addReplayStorageUsage(totals map[string]*big.Rat, class storage.StorageClass, bytes uint64, d time.Duration, bytesPerGB uint64, monthDuration time.Duration) error {
	if bytes == 0 || d <= 0 {
		return nil
	}
	u, err := cost.StorageUsage(class, bytes, d, bytesPerGB, monthDuration)
	if err != nil {
		return err
	}
	q, ok := new(big.Rat).SetString(u.Quantity)
	if !ok {
		return errors.New("invalid storage usage quantity rational")
	}
	if totals[u.RateID] == nil {
		totals[u.RateID] = new(big.Rat)
	}
	totals[u.RateID].Add(totals[u.RateID], q)
	return nil
}

func stepTimelineInterval(state *timelineReplayState, to int64, trace Trace, catalog *pricing.Catalog) error {
	d := time.Duration(to - state.previous)
	if d <= 0 {
		state.previous = to
		return nil
	}

	// 1. Direct S3 baseline: stores logical bytes on Standard
	if err := addReplayStorageUsage(state.directTotals, storage.Standard, state.currentLogical, d, trace.BytesPerGB, state.monthDuration); err != nil {
		return err
	}

	// Calculate current physical split across all valid classes
	totalPhysical := uint64(0)
	standardBytes := uint64(0)
	coldBytes := uint64(0)

	if len(state.currentStorageBytes) > 0 {
		sumStorageBytes := uint64(0)
		for class, b := range state.currentStorageBytes {
			sumStorageBytes += b
			switch class {
			case storage.Standard, storage.StandardIA:
				standardBytes += b
			case storage.Glacier, storage.GlacierIR:
				coldBytes += b
			}
		}
		if state.currentPhysical > 0 && state.currentPhysical != sumStorageBytes {
			state.hasStorageInconsistency = true
		}
		totalPhysical = state.currentPhysical
		if totalPhysical == 0 {
			totalPhysical = sumStorageBytes
		}
	} else {
		totalPhysical = state.currentPhysical
		standardBytes = state.currentPhysical
		coldBytes = 0
	}

	// 2. WarmStandardOnly: all physical bytes stay on Standard
	if err := addReplayStorageUsage(state.warmStdTotals, storage.Standard, totalPhysical, d, trace.BytesPerGB, state.monthDuration); err != nil {
		return err
	}

	// 3. WarmGlacierIR and 4. WarmGlacierFlexible
	irBillableCold := uint64(0)
	flexStandardBytes := standardBytes
	flexGlacierBytes := uint64(0)

	if coldBytes > 0 {
		state.hasObservedCold = true

		sumColdPackBytes := uint64(0)
		for _, pack := range state.activeColdPacks {
			if pack.Size > 0 {
				sumColdPackBytes += uint64(pack.Size)
			}
		}

		if len(state.activeColdPacks) > 0 && sumColdPackBytes == coldBytes {
			if irRule, ok := catalog.RuleForClass(string(storage.GlacierIR)); ok {
				for _, pack := range state.activeColdPacks {
					adj := cost.ApplyStorageRules(irRule, uint64(pack.Size), 1)
					irBillableCold += adj.BillableBytes
				}
			} else {
				irBillableCold = coldBytes
			}

			if flexRule, ok := catalog.RuleForClass(string(storage.Glacier)); ok {
				for _, pack := range state.activeColdPacks {
					adj := cost.ApplyStorageRules(flexRule, uint64(pack.Size), 1)
					flexGlacierBytes += adj.BillableBytes + adj.MetadataArchiveBytes
					flexStandardBytes += adj.MetadataStandardBytes
				}
			} else {
				flexGlacierBytes = coldBytes
			}
		} else {
			state.unprovenColdReasons[UnprovenColdStorageEvidence] = true
			if irRule, ok := catalog.RuleForClass(string(storage.GlacierIR)); ok {
				adj := cost.ApplyStorageRules(irRule, coldBytes, 1)
				irBillableCold = adj.BillableBytes
			} else {
				irBillableCold = coldBytes
			}

			if flexRule, ok := catalog.RuleForClass(string(storage.Glacier)); ok {
				adj := cost.ApplyStorageRules(flexRule, coldBytes, 1)
				flexGlacierBytes += adj.BillableBytes + adj.MetadataArchiveBytes
				flexStandardBytes += adj.MetadataStandardBytes
			} else {
				flexGlacierBytes = coldBytes
			}
		}
	}

	if err := addReplayStorageUsage(state.glacierIRTotals, storage.Standard, standardBytes, d, trace.BytesPerGB, state.monthDuration); err != nil {
		return err
	}
	if err := addReplayStorageUsage(state.glacierIRTotals, storage.GlacierIR, irBillableCold, d, trace.BytesPerGB, state.monthDuration); err != nil {
		return err
	}

	if err := addReplayStorageUsage(state.glacierFlexTotals, storage.Standard, flexStandardBytes, d, trace.BytesPerGB, state.monthDuration); err != nil {
		return err
	}
	if err := addReplayStorageUsage(state.glacierFlexTotals, storage.Glacier, flexGlacierBytes, d, trace.BytesPerGB, state.monthDuration); err != nil {
		return err
	}

	state.previous = to
	return nil
}

func applyEventTieringTransitions(state *timelineReplayState, event Event, trace Trace, catalog *pricing.Catalog) error {
	if event.Snapshot != nil {
		state.currentLogical = event.Snapshot.Logical
		state.currentPhysical = event.Snapshot.Physical
	}
	if event.StorageBytes != nil {
		state.currentStorageBytes = event.StorageBytes
	}

	if event.Tiering == nil || event.Tiering.Pack.Size <= 0 {
		return nil
	}

	pack := event.Tiering.Pack
	eventTime := trace.Start.Add(time.Duration(event.AtNS))
	isDeleting := pack.State == "Deleting" || pack.State == "Retiring" ||
		pack.State == "Deleted" || pack.State == "Retired"

	if isDeleting {
		rule, ok := catalog.RuleForClass(string(pack.Class))
		if !ok || rule.MinimumDays <= 0 {
			if pack.Ref.ID != "" {
				packKey := fmt.Sprintf("%s:%s", pack.Ref.ID, pack.Ref.Generation)
				delete(state.activeColdPacks, packKey)
			}
			return nil
		}

		if pack.Ref.ID == "" {
			state.unprovenColdReasons[UnprovenColdIdentity] = true
			return nil
		}

		packKey := fmt.Sprintf("%s:%s", pack.Ref.ID, pack.Ref.Generation)
		delete(state.activeColdPacks, packKey)

		chargeKey := fmt.Sprintf("%s:%s:%s", pack.Ref.ID, pack.Ref.Generation, pack.Class)
		if state.earlyDeletedPacks[chargeKey] {
			return nil
		}

		if pack.MinRetentionUntil.IsZero() {
			state.unprovenRetentionPacks[chargeKey] = true
			return nil
		}

		delete(state.unprovenRetentionPacks, chargeKey)
		state.earlyDeletedPacks[chargeKey] = true

		minRetention := time.Duration(rule.MinimumDays) * 24 * time.Hour
		var retained time.Duration
		if pack.MinRetentionUntil.After(eventTime) {
			remaining := pack.MinRetentionUntil.Sub(eventTime)
			if remaining > minRetention {
				remaining = minRetention
			}
			retained = minRetention - remaining
			if retained < 0 {
				retained = 0
			}
		} else {
			retained = minRetention
		}

		if retained < minRetention {
			u, err := cost.EarlyDeletionUsage(pack.Class, uint64(pack.Size), retained, minRetention, trace.BytesPerGB, state.monthDuration)
			if err != nil {
				return fmt.Errorf("calculating early deletion usage for pack %s: %w", pack.Ref.ID, err)
			}
			if u.Quantity != "" {
				var targetPolicy PolicyName
				switch pack.Class {
				case storage.GlacierIR:
					targetPolicy = PolicyWarmGlacierIR
				case storage.Glacier:
					targetPolicy = PolicyWarmGlacierFlex
				}
				if targetPolicy != "" {
					state.earlyDeletionUsageByPolicy[targetPolicy] = append(state.earlyDeletionUsageByPolicy[targetPolicy], u)
					earlyReport, err := cost.Evaluate(catalog, []cost.Usage{u})
					if err != nil {
						return fmt.Errorf("evaluating early deletion cost for pack %s: %w", pack.Ref.ID, err)
					}
					if val, ok := new(big.Rat).SetString(earlyReport.Subtotal); ok {
						state.earlyDeletionUSDByPolicy[targetPolicy].Add(state.earlyDeletionUSDByPolicy[targetPolicy], val)
					}
				}
			}
		}
	} else if pack.Class == storage.Glacier || pack.Class == storage.GlacierIR || pack.State == "Cold" {
		if pack.Ref.ID != "" {
			packKey := fmt.Sprintf("%s:%s", pack.Ref.ID, pack.Ref.Generation)
			state.activeColdPacks[packKey] = pack
		}
	}
	return nil
}

func applyEventRestores(state *timelineReplayState, event Event, trace Trace) error {
	// 1. Mark expiration for existing restore copies if current time reached deadline
	for _, c := range state.restoreCopies {
		if event.AtNS >= c.DeadlineNS {
			c.Expired = true
		}
	}

	// 2. Identify new restore usage in this event
	for _, u := range event.Usage {
		if u.RateID == "s3.restore_standard_gb" || u.RateID == "s3.restore_bulk_gb" {
			if q, ok := new(big.Rat).SetString(u.Quantity); ok && q.Sign() > 0 {
				restoredBytes, ok := ratGBToBytes(q, trace.BytesPerGB)
				if !ok {
					state.unprovenColdReasons[UnprovenColdRestoreDuration] = true
					continue
				}
				if restoredBytes > 0 {
					restoreDur, ok := trace.RestoreDuration(event)
					if !ok {
						state.unprovenColdReasons[UnprovenColdRestoreDuration] = true
						continue
					}
					durNS := restoreDur.Nanoseconds()
					if durNS <= 0 {
						continue
					}
					expiryNS := event.AtNS + durNS

					var packRef storage.PackRef
					if event.Tiering != nil {
						packRef = event.Tiering.Pack.Ref
						if !event.Tiering.Pack.RestoredUntil.IsZero() && !trace.Start.IsZero() {
							eventTime := trace.Start.Add(time.Duration(event.AtNS))
							if !event.Tiering.Pack.RestoredUntil.After(eventTime) {
								expiryNS = event.AtNS
							} else {
								expiryNS = event.Tiering.Pack.RestoredUntil.Sub(trace.Start).Nanoseconds()
							}
						}
					}

					var sameEventCopy *PackRestoreLifecycle
					if packRef.ID != "" {
						for _, c := range state.restoreCopies {
							if c.PackRef == packRef && c.StartNS == event.AtNS && !c.Expired {
								sameEventCopy = c
								break
							}
						}
					}
					if sameEventCopy != nil {
						sameEventCopy.Bytes += restoredBytes
						if expiryNS > sameEventCopy.DeadlineNS {
							sameEventCopy.DeadlineNS = expiryNS
						}
					} else {
						if packRef.ID != "" {
							for _, prev := range state.restoreCopies {
								if prev.PackRef == packRef && !prev.Expired {
									prev.DeadlineNS = event.AtNS
									prev.Expired = true
								}
							}
						}
						state.restoreCopies = append(state.restoreCopies, &PackRestoreLifecycle{
							PackRef:    packRef,
							StartNS:    event.AtNS,
							DeadlineNS: expiryNS,
							Bytes:      restoredBytes,
							Expired:    (event.AtNS >= expiryNS),
						})
					}
				}
			}
		}
	}

	// 3. Authoritative pack metadata observation
	if event.Tiering != nil {
		packRef := event.Tiering.Pack.Ref
		var activeCopy *PackRestoreLifecycle
		if packRef.ID != "" {
			for _, c := range state.restoreCopies {
				if c.PackRef == packRef && !c.Expired {
					activeCopy = c
					break
				}
			}
		}
		if activeCopy != nil {
			eventTime := trace.Start.Add(time.Duration(event.AtNS))
			if event.Tiering.Pack.State != "" && event.Tiering.Pack.State != "Restored" && event.Tiering.Pack.State != "Restoring" {
				activeCopy.DeadlineNS = event.AtNS
				activeCopy.Expired = true
			}
			if !event.Tiering.Pack.RestoredUntil.IsZero() && !trace.Start.IsZero() {
				isFuture := event.Tiering.Pack.RestoredUntil.After(eventTime)
				packExpiryNS := event.Tiering.Pack.RestoredUntil.Sub(trace.Start).Nanoseconds()
				if !isFuture {
					activeCopy.DeadlineNS = event.AtNS
					activeCopy.Expired = true
				} else {
					activeCopy.DeadlineNS = packExpiryNS
				}
			}
		}
	}
	return nil
}

func finalizeTemporaryRestores(state *timelineReplayState, trace Trace) error {
	for _, c := range state.restoreCopies {
		if c.Bytes == 0 {
			continue
		}
		endNS := c.DeadlineNS
		if endNS > trace.DurationNS {
			endNS = trace.DurationNS
		}
		durNS := endNS - c.StartNS
		if durNS <= 0 {
			continue
		}
		tempRestoreDur := time.Duration(durNS)
		tempU, err := cost.TemporaryRestoredUsage(c.Bytes, tempRestoreDur, trace.BytesPerGB, state.monthDuration)
		if err != nil {
			return fmt.Errorf("calculating temporary restored usage: %w", err)
		}
		state.temporaryRestoredBytes += c.Bytes
		if qRat, ok := new(big.Rat).SetString(tempU.Quantity); ok {
			if state.glacierFlexTotals[tempU.RateID] == nil {
				state.glacierFlexTotals[tempU.RateID] = new(big.Rat)
			}
			state.glacierFlexTotals[tempU.RateID].Add(state.glacierFlexTotals[tempU.RateID], qRat)
		}
	}
	return nil
}

func buildReplayResult(state *timelineReplayState) timelineReplayResult {
	finalPhysical := state.currentPhysical
	finalStandardBytes := uint64(0)
	finalColdBytes := uint64(0)
	if len(state.currentStorageBytes) > 0 {
		sumStorageBytes := uint64(0)
		for class, b := range state.currentStorageBytes {
			sumStorageBytes += b
			switch class {
			case storage.Standard, storage.StandardIA:
				finalStandardBytes += b
			case storage.Glacier, storage.GlacierIR:
				finalColdBytes += b
			}
		}
		if state.currentPhysical > 0 && state.currentPhysical != sumStorageBytes {
			state.hasStorageInconsistency = true
		}
	} else {
		finalStandardBytes = state.currentPhysical
		finalColdBytes = 0
	}
	if finalPhysical == 0 {
		finalPhysical = finalStandardBytes + finalColdBytes
	}

	directClassMap := map[string]uint64{"STANDARD": state.currentLogical}
	warmStdClassMap := map[string]uint64{"STANDARD": finalPhysical}

	glacierIRClassMap := map[string]uint64{}
	if finalStandardBytes > 0 {
		glacierIRClassMap["STANDARD"] = finalStandardBytes
	}
	if finalColdBytes > 0 {
		glacierIRClassMap["GLACIER_IR"] = finalColdBytes
	}

	glacierFlexClassMap := map[string]uint64{}
	if finalStandardBytes > 0 {
		glacierFlexClassMap["STANDARD"] = finalStandardBytes
	}
	if finalColdBytes > 0 {
		glacierFlexClassMap["GLACIER"] = finalColdBytes
	}

	return timelineReplayResult{
		currentLogical:             state.currentLogical,
		finalPhysical:              finalPhysical,
		directTotals:               state.directTotals,
		warmStdTotals:              state.warmStdTotals,
		glacierIRTotals:            state.glacierIRTotals,
		glacierFlexTotals:          state.glacierFlexTotals,
		directClassMap:             directClassMap,
		warmStdClassMap:            warmStdClassMap,
		glacierIRClassMap:          glacierIRClassMap,
		glacierFlexClassMap:        glacierFlexClassMap,
		temporaryRestoredBytes:     state.temporaryRestoredBytes,
		hasObservedCold:            state.hasObservedCold,
		unprovenColdReasons:        state.unprovenColdReasons,
		hasStorageInconsistency:    state.hasStorageInconsistency,
		earlyDeletionUsageByPolicy: state.earlyDeletionUsageByPolicy,
		earlyDeletionUSDByPolicy:   state.earlyDeletionUSDByPolicy,
		monthDuration:              state.monthDuration,
	}
}

func replayTimeline(ctx context.Context, trace Trace, catalog *pricing.Catalog, baseResult Result) (timelineReplayResult, error) {
	state := initReplayState(baseResult, trace)

	for _, event := range trace.Events {
		if err := ctx.Err(); err != nil {
			return timelineReplayResult{}, err
		}
		if err := stepTimelineInterval(state, event.AtNS, trace, catalog); err != nil {
			return timelineReplayResult{}, err
		}
		if err := applyEventTieringTransitions(state, event, trace, catalog); err != nil {
			return timelineReplayResult{}, err
		}
	}

	if len(state.unprovenRetentionPacks) > 0 {
		state.unprovenColdReasons[UnprovenColdRetention] = true
	}

	if err := ctx.Err(); err != nil {
		return timelineReplayResult{}, err
	}
	if err := stepTimelineInterval(state, trace.DurationNS, trace, catalog); err != nil {
		return timelineReplayResult{}, err
	}

	for _, event := range trace.Events {
		if err := applyEventRestores(state, event, trace); err != nil {
			return timelineReplayResult{}, err
		}
	}

	if err := finalizeTemporaryRestores(state, trace); err != nil {
		return timelineReplayResult{}, err
	}

	return buildReplayResult(state), nil
}
