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
	hasUnprovenColdPacks       bool
	hasStorageInconsistency    bool
	earlyDeletionUsageByPolicy map[PolicyName][]cost.Usage
	earlyDeletionUSDByPolicy   map[PolicyName]*big.Rat
	monthDuration              time.Duration
}

func replayTimeline(ctx context.Context, trace Trace, catalog *pricing.Catalog, baseResult Result) (timelineReplayResult, error) {
	var initialLogical, initialPhysical uint64
	if baseResult.Snapshot != nil {
		initialLogical = baseResult.Snapshot.Logical
		initialPhysical = baseResult.Snapshot.Physical
	} else if len(trace.Events) > 0 && trace.Events[0].Snapshot != nil {
		initialLogical = trace.Events[0].Snapshot.Logical
		initialPhysical = trace.Events[0].Snapshot.Physical
	}

	monthDuration := time.Duration(trace.MonthSeconds) * time.Second

	directTotals := map[string]*big.Rat{}
	warmStdTotals := map[string]*big.Rat{}
	glacierIRTotals := map[string]*big.Rat{}
	glacierFlexTotals := map[string]*big.Rat{}

	currentLogical := initialLogical
	currentPhysical := initialPhysical
	currentStorageBytes := map[storage.StorageClass]uint64{}

	hasObservedCold := false
	hasUnprovenColdPacks := false
	hasStorageInconsistency := false
	activeColdPacks := make(map[string]storage.PackInfo)
	var previous int64

	addStorageUsage := func(totals map[string]*big.Rat, class storage.StorageClass, bytes uint64, d time.Duration) error {
		if bytes == 0 || d <= 0 {
			return nil
		}
		u, err := cost.StorageUsage(class, bytes, d, trace.BytesPerGB, monthDuration)
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

	step := func(to int64) error {
		d := time.Duration(to - previous)
		if d <= 0 {
			previous = to
			return nil
		}

		// 1. Direct S3 baseline: stores logical bytes on Standard
		if err := addStorageUsage(directTotals, storage.Standard, currentLogical, d); err != nil {
			return err
		}

		// Calculate current physical split across all valid classes
		totalPhysical := uint64(0)
		standardBytes := uint64(0)
		coldBytes := uint64(0)

		if len(currentStorageBytes) > 0 {
			sumStorageBytes := uint64(0)
			for class, b := range currentStorageBytes {
				sumStorageBytes += b
				switch class {
				case storage.Standard, storage.StandardIA:
					standardBytes += b
				case storage.Glacier, storage.GlacierIR:
					coldBytes += b
				}
			}
			if currentPhysical > 0 && currentPhysical != sumStorageBytes {
				hasStorageInconsistency = true
			}
			totalPhysical = currentPhysical
			if totalPhysical == 0 {
				totalPhysical = sumStorageBytes
			}
		} else {
			totalPhysical = currentPhysical
			standardBytes = currentPhysical
			coldBytes = 0
		}

		// 2. WarmStandardOnly: all physical bytes (including IA and Cold) stay on Standard
		if err := addStorageUsage(warmStdTotals, storage.Standard, totalPhysical, d); err != nil {
			return err
		}

		// 3. WarmGlacierIR: standard/IA on Standard, cold on Glacier IR
		// Applies Glacier IR storage rule: 128 KiB minimum billable bytes padding per pack
		irBillableCold := uint64(0)
		flexStandardBytes := standardBytes
		flexGlacierBytes := uint64(0)

		if coldBytes > 0 {
			hasObservedCold = true

			sumColdPackBytes := uint64(0)
			for _, pack := range activeColdPacks {
				if pack.Size > 0 {
					sumColdPackBytes += uint64(pack.Size)
				}
			}

			if len(activeColdPacks) > 0 && sumColdPackBytes == coldBytes {
				// Measured per-pack accounting from observed tiering events
				if irRule, ok := catalog.RuleForClass(string(storage.GlacierIR)); ok {
					for _, pack := range activeColdPacks {
						adj := cost.ApplyStorageRules(irRule, uint64(pack.Size), 1)
						irBillableCold += adj.BillableBytes
					}
				} else {
					irBillableCold = coldBytes
				}

				if flexRule, ok := catalog.RuleForClass(string(storage.Glacier)); ok {
					for _, pack := range activeColdPacks {
						adj := cost.ApplyStorageRules(flexRule, uint64(pack.Size), 1)
						flexGlacierBytes += adj.BillableBytes + adj.MetadataArchiveBytes
						flexStandardBytes += adj.MetadataStandardBytes
					}
				} else {
					flexGlacierBytes = coldBytes
				}
			} else {
				// Cold storage bytes are present but per-pack distribution is unproven.
				// Fail closed on completeness.
				hasUnprovenColdPacks = true
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

		if err := addStorageUsage(glacierIRTotals, storage.Standard, standardBytes, d); err != nil {
			return err
		}
		if err := addStorageUsage(glacierIRTotals, storage.GlacierIR, irBillableCold, d); err != nil {
			return err
		}

		// 4. WarmGlacierFlexible: standard/IA on Standard, cold on Glacier
		// Applies Glacier Flexible storage rule: 8 KiB Standard + 32 KiB Glacier metadata per pack
		if err := addStorageUsage(glacierFlexTotals, storage.Standard, flexStandardBytes, d); err != nil {
			return err
		}
		if err := addStorageUsage(glacierFlexTotals, storage.Glacier, flexGlacierBytes, d); err != nil {
			return err
		}

		previous = to
		return nil
	}

	earlyDeletionUsageByPolicy := map[PolicyName][]cost.Usage{
		PolicyDirectS3:         {},
		PolicyWarmStandardOnly: {},
		PolicyWarmGlacierIR:    {},
		PolicyWarmGlacierFlex:  {},
	}
	earlyDeletionUSDByPolicy := map[PolicyName]*big.Rat{
		PolicyDirectS3:         new(big.Rat),
		PolicyWarmStandardOnly: new(big.Rat),
		PolicyWarmGlacierIR:    new(big.Rat),
		PolicyWarmGlacierFlex:  new(big.Rat),
	}
	earlyDeletedPacks := make(map[string]bool)
	unprovenRetentionPacks := make(map[string]bool)

	// Replay events through timeline
	for _, event := range trace.Events {
		if err := ctx.Err(); err != nil {
			return timelineReplayResult{}, err
		}
		if err := step(event.AtNS); err != nil {
			return timelineReplayResult{}, err
		}

		if event.Snapshot != nil {
			currentLogical = event.Snapshot.Logical
			currentPhysical = event.Snapshot.Physical
		}
		if event.StorageBytes != nil {
			currentStorageBytes = event.StorageBytes
		}

		// Account early deletion if pack in a class with minimum retention is deleted or retired before minimum retention days.
		// Deduplicate lifecycle observations by pack identity and generation so early deletion is charged exactly once.
		if event.Tiering != nil {
			pack := event.Tiering.Pack
			if pack.Size > 0 {
				eventTime := trace.Start.Add(time.Duration(event.AtNS))
				isDeleting := pack.State == "Deleting" || pack.State == "Retiring" ||
					pack.State == "Deleted" || pack.State == "Retired"

				if isDeleting {
					// Only classes with minimum retention obligations (e.g. Glacier IR, Glacier Flexible)
					// incur early deletion fees and require retention identity tracking.
					rule, ok := catalog.RuleForClass(string(pack.Class))
					if !ok || rule.MinimumDays <= 0 {
						if pack.Ref.ID != "" {
							packKey := fmt.Sprintf("%s:%s", pack.Ref.ID, pack.Ref.Generation)
							delete(activeColdPacks, packKey)
						}
						continue
					}

					// For classes with minimum retention obligations:
					// Pack identity is required for idempotent early deletion accounting.
					if pack.Ref.ID == "" {
						hasUnprovenColdPacks = true
						continue
					}

					packKey := fmt.Sprintf("%s:%s", pack.Ref.ID, pack.Ref.Generation)
					delete(activeColdPacks, packKey)

					chargeKey := fmt.Sprintf("%s:%s:%s", pack.Ref.ID, pack.Ref.Generation, pack.Class)
					if earlyDeletedPacks[chargeKey] {
						continue // Idempotent: already accounted for this pack ref and class
					}

					// Retention deadline timing must have verified provenance (MinRetentionUntil).
					// Do not guess from LastAccess (a recent read does not reset retention start).
					if pack.MinRetentionUntil.IsZero() {
						// Record as unproven pending subsequent lifecycle observations of this pack.
						unprovenRetentionPacks[chargeKey] = true
						continue
					}

					// Verified retention deadline is present: clear unproven status and mark charged.
					delete(unprovenRetentionPacks, chargeKey)
					earlyDeletedPacks[chargeKey] = true

					minRetention := time.Duration(rule.MinimumDays) * 24 * time.Hour

					var retained time.Duration
					if pack.MinRetentionUntil.After(eventTime) {
						remaining := pack.MinRetentionUntil.Sub(eventTime)
						if remaining > minRetention {
							remaining = minRetention // Clamp penalty to at most full minimum retention
						}
						retained = minRetention - remaining
						if retained < 0 {
							retained = 0
						}
					} else {
						retained = minRetention // Retention obligation already fulfilled (expired)
					}

					if retained < minRetention {
						u, err := cost.EarlyDeletionUsage(pack.Class, uint64(pack.Size), retained, minRetention, trace.BytesPerGB, monthDuration)
						if err == nil && u.Quantity != "" {
							var targetPolicy PolicyName
							switch pack.Class {
							case storage.GlacierIR:
								targetPolicy = PolicyWarmGlacierIR
							case storage.Glacier:
								targetPolicy = PolicyWarmGlacierFlex
							}
							if targetPolicy != "" {
								earlyDeletionUsageByPolicy[targetPolicy] = append(earlyDeletionUsageByPolicy[targetPolicy], u)
								if earlyReport, err := cost.Evaluate(catalog, []cost.Usage{u}); err == nil {
									if val, ok := new(big.Rat).SetString(earlyReport.Subtotal); ok {
										earlyDeletionUSDByPolicy[targetPolicy].Add(earlyDeletionUSDByPolicy[targetPolicy], val)
									}
								}
							}
						}
					}
				} else if pack.Class == storage.Glacier || pack.Class == storage.GlacierIR || pack.State == "Cold" {
					if pack.Ref.ID != "" {
						packKey := fmt.Sprintf("%s:%s", pack.Ref.ID, pack.Ref.Generation)
						activeColdPacks[packKey] = pack
					}
				}
			}
		}
	}

	if len(unprovenRetentionPacks) > 0 {
		hasUnprovenColdPacks = true
	}

	if err := ctx.Err(); err != nil {
		return timelineReplayResult{}, err
	}
	if err := step(trace.DurationNS); err != nil {
		return timelineReplayResult{}, err
	}

	var temporaryRestoredBytes uint64
	// Track temporary restored Standard storage byte-time during restores in Glacier Flexible
	for _, event := range trace.Events {
		for _, u := range event.Usage {
			if u.RateID == "s3.restore_standard_gb" || u.RateID == "s3.restore_bulk_gb" {
				if q, ok := new(big.Rat).SetString(u.Quantity); ok {
					gbFloat, _ := q.Float64()
					restoredBytes := uint64(gbFloat * float64(trace.BytesPerGB))
					if restoredBytes > 0 {
						remainingTraceDur := time.Duration(trace.DurationNS - event.AtNS)
						if remainingTraceDur <= 0 {
							continue
						}
						tempRestoreDur := 24 * time.Hour // default 1-day temporary Standard copy
						if tempRestoreDur > remainingTraceDur {
							tempRestoreDur = remainingTraceDur
						}
						tempU, err := cost.TemporaryRestoredUsage(restoredBytes, tempRestoreDur, trace.BytesPerGB, monthDuration)
						if err == nil {
							temporaryRestoredBytes += restoredBytes
							if qRat, ok := new(big.Rat).SetString(tempU.Quantity); ok {
								if glacierFlexTotals[tempU.RateID] == nil {
									glacierFlexTotals[tempU.RateID] = new(big.Rat)
								}
								glacierFlexTotals[tempU.RateID].Add(glacierFlexTotals[tempU.RateID], qRat)
							}
						}
					}
				}
			}
		}
	}

	finalPhysical := currentPhysical
	finalStandardBytes := uint64(0)
	finalColdBytes := uint64(0)
	if len(currentStorageBytes) > 0 {
		sumStorageBytes := uint64(0)
		for class, b := range currentStorageBytes {
			sumStorageBytes += b
			switch class {
			case storage.Standard, storage.StandardIA:
				finalStandardBytes += b
			case storage.Glacier, storage.GlacierIR:
				finalColdBytes += b
			}
		}
		if currentPhysical > 0 && currentPhysical != sumStorageBytes {
			hasStorageInconsistency = true
		}
	} else {
		finalStandardBytes = currentPhysical
		finalColdBytes = 0
	}
	if finalPhysical == 0 {
		finalPhysical = finalStandardBytes + finalColdBytes
	}

	directClassMap := map[string]uint64{"STANDARD": currentLogical}
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
		currentLogical:             currentLogical,
		finalPhysical:              finalPhysical,
		directTotals:               directTotals,
		warmStdTotals:              warmStdTotals,
		glacierIRTotals:            glacierIRTotals,
		glacierFlexTotals:          glacierFlexTotals,
		directClassMap:             directClassMap,
		warmStdClassMap:            warmStdClassMap,
		glacierIRClassMap:          glacierIRClassMap,
		glacierFlexClassMap:        glacierFlexClassMap,
		temporaryRestoredBytes:     temporaryRestoredBytes,
		hasObservedCold:            hasObservedCold,
		hasUnprovenColdPacks:       hasUnprovenColdPacks,
		hasStorageInconsistency:    hasStorageInconsistency,
		earlyDeletionUsageByPolicy: earlyDeletionUsageByPolicy,
		earlyDeletionUSDByPolicy:   earlyDeletionUSDByPolicy,
		monthDuration:              monthDuration,
	}, nil
}
