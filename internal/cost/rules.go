package cost

import (
	"math/big"
	"time"

	"github.com/KinasTomes/miles/internal/pricing"
	"github.com/KinasTomes/miles/internal/storage"
)

// StorageRuleAdjustment adjusts physical storage bytes according to the pricing catalog's storage rules:
// - Minimum billable size padding (e.g. 128 KiB for Standard-IA and Glacier IR).
// - Archival metadata storage overhead (e.g. 8 KiB Standard + 32 KiB Glacier per object for Glacier Flexible Archive).
type StorageRuleAdjustment struct {
	BillableBytes         uint64 `json:"billable_bytes"`
	MetadataStandardBytes uint64 `json:"metadata_standard_bytes"`
	MetadataArchiveBytes  uint64 `json:"metadata_archive_bytes"`
}

// ApplyStorageRules calculates the adjusted billable bytes and archive metadata bytes for
// a given physical byte quantity and object count based on the class's pricing.StorageRule.
func ApplyStorageRules(rule pricing.StorageRule, objectBytes uint64, objectCount uint64) StorageRuleAdjustment {
	adj := StorageRuleAdjustment{
		BillableBytes: objectBytes,
	}
	if objectBytes > 0 && objectCount == 0 {
		objectCount = 1
	}
	if rule.MinimumBillableBytes > 0 && objectCount > 0 {
		minTotal := uint64(rule.MinimumBillableBytes) * objectCount
		if adj.BillableBytes < minTotal {
			adj.BillableBytes = minTotal
		}
	}
	adj.MetadataStandardBytes = uint64(rule.MetadataStandardBytes) * objectCount
	adj.MetadataArchiveBytes = uint64(rule.MetadataArchiveBytes) * objectCount
	return adj
}

// EarlyDeletionUsage converts unexpired retention time for an early-deleted object to GB-Mo.
// If retained is less than minimum retention, the remaining duration is billed at the class rate.
func EarlyDeletionUsage(class storage.StorageClass, bytes uint64, retained time.Duration, minRetention time.Duration, bytesPerGB uint64, month time.Duration) (Usage, error) {
	if retained >= minRetention || bytes == 0 {
		return Usage{}, nil
	}
	remaining := minRetention - retained
	return StorageUsage(class, bytes, remaining, bytesPerGB, month)
}

// TemporaryRestoredUsage converts temporary Standard copy duration during Glacier restore to GB-Mo.
func TemporaryRestoredUsage(bytes uint64, restoreDuration time.Duration, bytesPerGB uint64, month time.Duration) (Usage, error) {
	if bytes == 0 || restoreDuration <= 0 {
		return Usage{}, nil
	}
	return StorageUsage(storage.Standard, bytes, restoreDuration, bytesPerGB, month)
}

// BreakEven calculates the break-even storage volume in GB where cumulative monthly storage savings
// offset fixed monthly infrastructure costs.
// If fixed costs are 0 or storage saving per GB is <= 0, returns nil.
func BreakEven(fixedMonthlyInfraUSD *big.Rat, monthlySavingPerGBUSD *big.Rat) *string {
	if fixedMonthlyInfraUSD == nil || monthlySavingPerGBUSD == nil || fixedMonthlyInfraUSD.Sign() <= 0 || monthlySavingPerGBUSD.Sign() <= 0 {
		return nil
	}
	breakEvenGB := new(big.Rat).Quo(fixedMonthlyInfraUSD, monthlySavingPerGBUSD)
	s := breakEvenGB.RatString()
	return &s
}

// RestoreAmplification calculates the ratio of physical bytes retrieved from cold storage vs
// logical bytes delivered to the client during cold pack reads.
func RestoreAmplification(physicalBytesRetrieved uint64, logicalBytesDelivered uint64) *float64 {
	if logicalBytesDelivered == 0 || physicalBytesRetrieved == 0 {
		return nil
	}
	ratio := float64(physicalBytesRetrieved) / float64(logicalBytesDelivered)
	return &ratio
}
