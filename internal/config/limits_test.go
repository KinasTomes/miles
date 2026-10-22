package config_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	c "github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/storage"
)

func TestDSGJSONBudget(t *testing.T) {
	v := validator(t)
	s := globalSnapshot(t, v, 1)
	for _, size := range []int{8192, 8193} {
		input := []byte(`{"compression":{"enabled":false}}`)
		input = append(input, []byte(strings.Repeat(" ", size-len(input)))...)
		_, err := v.MergeGlobal(s, input, 1)
		if (err == nil) != (size == 8192) {
			t.Fatal(size, err)
		}
		bucket := bucketSnapshot(t, v, 1)
		patch := `{"expected_bucket_id":"bucket-one","cache":{"admit_on_write":false}}`
		_, err = v.MergeBucket(bucket, []byte(patch+strings.Repeat(" ", size-len(patch))), 1)
		if (err == nil) != (size == 8192) {
			t.Fatal("bucket", size, err)
		}
	}
	// Với caps decimal DSG, complete document hợp lệ nhỏ hơn 8192 bytes.
	// Inject decimal budgets rộng chỉ để cô lập JSON candidate gate tại 8192;
	// không thay bộ DSG hoặc dùng bộ test này như production configuration.
	limits := c.DSGPolicyLimits()
	limits.MaxDecimalDigits = 10000
	limits.MaxDecimalExponent = 10000
	wide, err := c.NewPolicyValidator(limits)
	if err != nil {
		t.Fatal(err)
	}
	current := globalSnapshot(t, wide, 1)
	patch := []byte(`{"compression":{"min_saving_ratio":0.` + strings.Repeat("9", 7900) + `}}`)
	if len(patch) > 8192 {
		t.Fatal("fixture input vượt cap")
	}
	p := current.Policy()
	if err := json.Unmarshal(patch, &p); err != nil {
		t.Fatal(err)
	}
	full, err := json.Marshal(p)
	if err != nil || len(full) <= 8192 {
		t.Fatal("fixture candidate chưa vượt cap", len(full), err)
	}
	if _, err := wide.MergeGlobal(current, patch, 1); !errors.Is(err, c.ErrPolicyInvalid) {
		t.Fatal("candidate vượt cap", err)
	}
	if err := wide.ValidatePolicy(p); !errors.Is(err, c.ErrPolicyInvalid) {
		t.Fatal(err)
	}
}

func TestDSGNormalizedDecimalLimits(t *testing.T) {
	v := validator(t)
	for _, tc := range []struct {
		name, number string
		valid        bool
	}{
		{"coefficient128", "0." + strings.Repeat("9", 128), true},
		{"coefficient129", "0." + strings.Repeat("9", 129), false},
		{"normalized coefficient", "0." + strings.Repeat("9", 128) + strings.Repeat("0", 100), true},
		{"exponent minus256", "1e-256", true},
		{"exponent minus257", "1e-257", false},
		{"normalized exponent minus256", "10e-257", true},
		// +256 nằm trong exponent cap nhưng vẫn ngoài ratio range; không nới ratio.
		{"exponent plus256 ratio rejected", "1e256", false},
		{"exponent plus257 rejected", "1e257", false},
		{"normalized exponent plus256 ratio rejected", "10e255", false},
		{"raw exponent beyond cap normalizes inside", "0." + strings.Repeat("0", 300) + "1e300", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, live := range []bool{false, true} {
				p := c.DefaultGlobalPolicy()
				d := decimal(t, tc.number)
				if live {
					p.Tiering.MinPackLiveRatio = d
				} else {
					p.Compression.MinSavingRatio = d
				}
				err := v.ValidatePolicy(p)
				if (err == nil) != tc.valid {
					t.Fatal(live, err)
				}
				if err != nil && !errors.Is(err, c.ErrPolicyInvalid) {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestDSGDayAndReadAheadBounds(t *testing.T) {
	v := validator(t)
	for _, n := range []int64{0, 1000, 1001} {
		p := c.DefaultGlobalPolicy()
		p.Tiering.ColdAfterDays = n
		if err := v.ValidatePolicy(p); (err == nil) != (n <= 1000) {
			t.Fatal("cold", n, err)
		}
	}
	for _, n := range []int64{1, 30, 0, 31} {
		p := c.DefaultGlobalPolicy()
		p.Tiering.AutoRestoreDays = n
		if err := v.ValidatePolicy(p); (err == nil) != (n >= 1 && n <= 30) {
			t.Fatal("restore", n, err)
		}
	}
	for _, n := range []int64{0, 64, 65} {
		p := c.DefaultCachePolicy()
		p.ReadaheadChunks = n
		if err := v.ValidateCache(p); (err == nil) != (n <= 64) {
			t.Fatal("cache", n, err)
		}
	}
}

func TestDSGRestoreAllowlist(t *testing.T) {
	v := validator(t)
	for _, tc := range []struct {
		class storage.StorageClass
		tier  string
		valid bool
	}{
		{storage.Glacier, "Expedited", true}, {storage.Glacier, "Standard", true}, {storage.Glacier, "Bulk", true},
		{storage.GlacierIR, "Standard", true}, {storage.GlacierIR, "Expedited", false}, {storage.GlacierIR, "Bulk", false},
		{storage.Glacier, "standard", false}, {storage.Glacier, "EXPEDITED", false}, {storage.Glacier, "bulk", false},
		{"DEEP_ARCHIVE", "Standard", false}, {storage.Standard, "Standard", false}, {"glacier", "Standard", false}, {storage.Glacier, "Other", false},
	} {
		p := c.DefaultGlobalPolicy()
		p.Tiering.ColdStorageClass = tc.class
		p.Tiering.AutoRestoreTier = tc.tier
		if err := v.ValidatePolicy(p); (err == nil) != tc.valid {
			t.Fatal(tc, err)
		}
	}
	for _, rule := range []c.RestoreTierRule{{Class: storage.GlacierIR, Tier: "Bulk"}, {Class: storage.Glacier, Tier: "standard"}, {Class: "DEEP_ARCHIVE", Tier: "Bulk"}} {
		l := c.DSGPolicyLimits()
		l.RestoreTiers = append(l.RestoreTiers, rule)
		if _, err := c.NewPolicyValidator(l); !errors.Is(err, c.ErrPolicyDeferred) {
			t.Fatal("custom allowlist mở ngoài P1", rule, err)
		}
	}
	// Constructor/copy ownership và custom caps thực sự qua validation path.
	l := c.DSGPolicyLimits()
	l.RestoreTiers[0].Tier = "Other"
	if err := validator(t).ValidatePolicy(c.DefaultGlobalPolicy()); err != nil {
		t.Fatal("factory shared slice", err)
	}
	l = c.DSGPolicyLimits()
	l.MaxColdAfterDays = 10
	custom, err := c.NewPolicyValidator(l)
	if err != nil {
		t.Fatal(err)
	}
	if err := custom.ValidatePolicy(c.DefaultGlobalPolicy()); !errors.Is(err, c.ErrPolicyInvalid) {
		t.Fatal("custom cap bypass", err)
	}
	p := c.DefaultGlobalPolicy()
	p.Tiering.ColdAfterDays = 10
	if err := custom.ValidatePolicy(p); err != nil {
		t.Fatal(err)
	}
}
