package cost

import (
	"math/big"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/pricing"
	"github.com/KinasTomes/miles/internal/storage"
)

func TestApplyStorageRulesPaddingAndMetadata(t *testing.T) {
	// Standard: no min bytes, no metadata
	stdRule := pricing.StorageRule{Class: "STANDARD", MinimumDays: 0, MinimumBillableBytes: 0, MetadataStandardBytes: 0, MetadataArchiveBytes: 0}
	adjStd := ApplyStorageRules(stdRule, 1024, 1)
	if adjStd.BillableBytes != 1024 || adjStd.MetadataStandardBytes != 0 || adjStd.MetadataArchiveBytes != 0 {
		t.Fatalf("unexpected standard adjustment: %+v", adjStd)
	}

	// Glacier IR: 128 KiB min billable bytes
	irRule := pricing.StorageRule{Class: "GLACIER_IR", MinimumDays: 90, MinimumBillableBytes: 131072, MetadataStandardBytes: 0, MetadataArchiveBytes: 0}
	adjIRSmall := ApplyStorageRules(irRule, 1024, 2)
	// 2 objects padded to 2 * 131072 = 262144 bytes
	if adjIRSmall.BillableBytes != 262144 {
		t.Fatalf("expected 262144 padded bytes, got %d", adjIRSmall.BillableBytes)
	}

	adjIRLarge := ApplyStorageRules(irRule, 1000000, 2)
	if adjIRLarge.BillableBytes != 1000000 {
		t.Fatalf("expected 1000000 unpadded bytes, got %d", adjIRLarge.BillableBytes)
	}

	// Glacier Flexible: 8 KiB standard metadata + 32 KiB archive metadata per object
	glacierRule := pricing.StorageRule{Class: "GLACIER", MinimumDays: 90, MinimumBillableBytes: 0, MetadataStandardBytes: 8192, MetadataArchiveBytes: 32768}
	adjGlacier := ApplyStorageRules(glacierRule, 1048576, 5)
	if adjGlacier.BillableBytes != 1048576 {
		t.Fatalf("expected 1048576 billable bytes, got %d", adjGlacier.BillableBytes)
	}
	if adjGlacier.MetadataStandardBytes != 5*8192 {
		t.Fatalf("expected %d standard metadata bytes, got %d", 5*8192, adjGlacier.MetadataStandardBytes)
	}
	if adjGlacier.MetadataArchiveBytes != 5*32768 {
		t.Fatalf("expected %d archive metadata bytes, got %d", 5*32768, adjGlacier.MetadataArchiveBytes)
	}
}

func TestEarlyDeletionUsage(t *testing.T) {
	bytesPerGB := uint64(1024 * 1024 * 1024)
	month := 30 * 24 * time.Hour
	minRetention := 90 * 24 * time.Hour

	// Retained for 10 days out of 90 days -> 80 days remaining
	u, err := EarlyDeletionUsage(storage.Glacier, 1024*1024*1024, 10*24*time.Hour, minRetention, bytesPerGB, month)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 80 days / 30 days = 8/3 GB-Mo
	if u.RateID != "s3.storage.glacier" || u.Quantity != "8/3" {
		t.Fatalf("expected 8/3 GB-Mo for 80 remaining days, got %+v", u)
	}

	// Retained for 90 days (full retention) -> 0 usage
	uZero, err := EarlyDeletionUsage(storage.Glacier, 1024*1024*1024, 90*24*time.Hour, minRetention, bytesPerGB, month)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if uZero.Quantity != "" {
		t.Fatalf("expected zero usage when retention satisfied, got %+v", uZero)
	}
}

func TestTemporaryRestoredUsage(t *testing.T) {
	bytesPerGB := uint64(1024 * 1024 * 1024)
	month := 30 * 24 * time.Hour

	// 1 GiB restored for 3 days
	u, err := TemporaryRestoredUsage(1024*1024*1024, 3*24*time.Hour, bytesPerGB, month)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 3 days / 30 days = 1/10 GB-Mo
	if u.RateID != "s3.storage.standard" || u.Quantity != "1/10" {
		t.Fatalf("expected 1/10 GB-Mo, got %+v", u)
	}
}

func TestBreakEvenAndRestoreAmplification(t *testing.T) {
	// Fixed infra: $15/month. Storage saving: $0.015/GB-Mo (15/1000)
	fixedInfra, _ := new(big.Rat).SetString("15")
	savingPerGB, _ := new(big.Rat).SetString("15/1000")
	be := BreakEven(fixedInfra, savingPerGB)
	if be == nil || *be != "1000" {
		t.Fatalf("expected 1000 GB break-even, got %v", be)
	}

	// Zero infra -> nil break-even
	zeroInfra := new(big.Rat)
	if BreakEven(zeroInfra, savingPerGB) != nil {
		t.Fatal("expected nil break-even for zero infra")
	}

	// Restore amplification: 16 MiB retrieved for 1 MiB delivered
	amp := RestoreAmplification(16*1024*1024, 1*1024*1024)
	if amp == nil || *amp != 16.0 {
		t.Fatalf("expected 16.0 amplification, got %v", amp)
	}

	if RestoreAmplification(0, 1024) != nil {
		t.Fatal("expected nil amplification for 0 retrieved bytes")
	}
}
