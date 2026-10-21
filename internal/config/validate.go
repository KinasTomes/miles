package config

import (
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/KinasTomes/miles/internal/storage"
)

type RestoreTierRule struct {
	Class storage.StorageClass
	Tier  string
}

// PolicyLimits là dependency explicit. DSGPolicyLimits cung cấp caps P1 đã chốt;
// tests/consumer có thể inject budgets riêng hoặc allowlist hẹp hơn trong P1.
// Constructor không tự điền defaults cho dependency thiếu hoặc historical data.
type PolicyLimits struct {
	MaxJSONBytes       int
	MaxDecimalDigits   int
	MaxDecimalExponent int64
	MaxColdAfterDays   int64
	MaxAutoRestoreDays int64
	MaxReadaheadChunks int64
	RestoreTiers       []RestoreTierRule
}

type PolicyValidator struct {
	limits PolicyLimits
	tiers  map[RestoreTierRule]struct{}
	ready  bool
}

func NewPolicyValidator(limits PolicyLimits) (PolicyValidator, error) {
	if limits.MaxJSONBytes <= 0 || limits.MaxDecimalDigits <= 0 || limits.MaxDecimalExponent <= 0 || limits.MaxColdAfterDays <= 0 || limits.MaxAutoRestoreDays <= 0 || limits.MaxReadaheadChunks <= 0 || len(limits.RestoreTiers) == 0 {
		return PolicyValidator{}, ErrPolicyDeferred
	}
	tiers := make(map[RestoreTierRule]struct{}, len(limits.RestoreTiers))
	for _, rule := range limits.RestoreTiers {
		if !supportedRestoreTier(rule) {
			return PolicyValidator{}, ErrPolicyDeferred
		}
		tiers[rule] = struct{}{}
	}
	limits.RestoreTiers = nil // Do not retain the caller's mutable slice.
	return PolicyValidator{limits: limits, tiers: tiers, ready: true}, nil
}

func revisionValid(revision uint64) bool {
	return revision > 0 && revision <= storage.MaxPolicyRevision
}

func (v PolicyValidator) ValidatePolicy(p GlobalPolicy) error {
	if !v.ready {
		return ErrPolicyDeferred
	}
	t := p.Tiering
	if t.WarmStorageClass != storage.Standard && t.WarmStorageClass != storage.StandardIA || t.ColdStorageClass != storage.GlacierIR && t.ColdStorageClass != storage.Glacier {
		return fmt.Errorf("%w: storage class", ErrPolicyInvalid)
	}
	// 24h UTC days and Duration representability are source arithmetic rules,
	// not arbitrary numeric product caps. Caller-supplied caps apply as well.
	maxDurationDays := int64(math.MaxInt64) / int64(24*time.Hour)
	if t.ColdAfterDays < 0 || t.ColdAfterDays > v.limits.MaxColdAfterDays || t.ColdAfterDays > maxDurationDays || t.AutoRestoreDays <= 0 || t.AutoRestoreDays > v.limits.MaxAutoRestoreDays || t.AutoRestoreDays > maxDurationDays {
		return fmt.Errorf("%w: tiering days", ErrPolicyInvalid)
	}
	if _, ok := v.tiers[RestoreTierRule{Class: t.ColdStorageClass, Tier: t.AutoRestoreTier}]; !ok {
		return fmt.Errorf("%w: restore tier/class is not in supplied reviewed rules", ErrPolicyInvalid)
	}
	c := p.Compression
	if c.Codec != "zstd" || c.Level != 1 && c.Level != 3 && c.Level != 7 && c.Level != 11 {
		return fmt.Errorf("%w: compression codec/level", ErrPolicyInvalid)
	}
	for _, ratio := range []struct {
		d          Decimal
		includeOne bool
	}{{t.MinPackLiveRatio, true}, {c.MinSavingRatio, false}} {
		if len(ratio.d.digits) > v.limits.MaxDecimalDigits || ratio.d.exponent > v.limits.MaxDecimalExponent || ratio.d.exponent < -v.limits.MaxDecimalExponent || !ratio.d.ratio(ratio.includeOne) {
			return fmt.Errorf("%w: exact decimal ratio or supplied resource bound", ErrPolicyInvalid)
		}
	}
	b := p.Chunking
	if b.MinBytes < 16384 || b.MinBytes > b.AvgBytes || b.AvgBytes > b.MaxBytes || b.MaxBytes > 16777216 || b.AvgBytes&(b.AvgBytes-1) != 0 {
		return fmt.Errorf("%w: FastCDC bounds/power-of-two", ErrPolicyInvalid)
	}
	document, err := json.Marshal(p)
	if err != nil || len(document) > v.limits.MaxJSONBytes {
		return fmt.Errorf("%w: complete policy exceeds supplied JSON budget", ErrPolicyInvalid)
	}
	return nil
}

func (v PolicyValidator) ValidateCache(p CachePolicy) error {
	if !v.ready {
		return ErrPolicyDeferred
	}
	if p.ReadaheadChunks < 0 || p.ReadaheadChunks > v.limits.MaxReadaheadChunks {
		return fmt.Errorf("%w: cache readahead bound", ErrPolicyInvalid)
	}
	document, err := json.Marshal(struct {
		Cache CachePolicy `json:"cache"`
	}{Cache: p})
	if err != nil || len(document) > v.limits.MaxJSONBytes {
		return fmt.Errorf("%w: complete bucket policy exceeds supplied JSON budget", ErrPolicyInvalid)
	}
	return nil
}

func (v PolicyValidator) NewGlobalSnapshot(revision uint64, p GlobalPolicy) (GlobalSnapshot, error) {
	if err := v.ValidatePolicy(p); err != nil {
		return GlobalSnapshot{}, err
	}
	if !revisionValid(revision) {
		return GlobalSnapshot{}, fmt.Errorf("%w: global revision", ErrPolicyInvalid)
	}
	return GlobalSnapshot{revision: revision, policy: p, valid: true}, nil
}

func (v PolicyValidator) NewBucketSnapshot(bucketID string, revision uint64, p CachePolicy) (BucketSnapshot, error) {
	if err := v.ValidateCache(p); err != nil {
		return BucketSnapshot{}, err
	}
	if bucketID == "" || !revisionValid(revision) {
		return BucketSnapshot{}, fmt.Errorf("%w: bucket incarnation/revision", ErrPolicyInvalid)
	}
	return BucketSnapshot{bucketID: bucketID, revision: revision, cache: p, valid: true}, nil
}
