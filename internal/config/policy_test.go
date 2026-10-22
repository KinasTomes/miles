package config_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"

	c "github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/storage"
)

// Dùng cùng bộ caps DSG đã chốt; các test riêng vẫn inject limits khác.
func testPolicyLimits() c.PolicyLimits {
	return c.DSGPolicyLimits()
}

func validator(t *testing.T) c.PolicyValidator {
	t.Helper()
	v, err := c.NewPolicyValidator(testPolicyLimits())
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func globalSnapshot(t *testing.T, v c.PolicyValidator, revision uint64) c.GlobalSnapshot {
	t.Helper()
	s, err := v.NewGlobalSnapshot(revision, c.DefaultGlobalPolicy())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func bucketSnapshot(t *testing.T, v c.PolicyValidator, revision uint64) c.BucketSnapshot {
	t.Helper()
	s, err := v.NewBucketSnapshot("bucket-one", revision, c.DefaultCachePolicy())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func decimal(t *testing.T, text string) c.Decimal {
	t.Helper()
	var d c.Decimal
	if err := json.Unmarshal([]byte(text), &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestPolicyDefaultsFromW1(t *testing.T) {
	p := c.DefaultGlobalPolicy()
	if !p.Tiering.Enabled || p.Tiering.WarmStorageClass != storage.Standard || p.Tiering.ColdStorageClass != storage.GlacierIR || p.Tiering.ColdAfterDays != 30 || p.Tiering.MinPackLiveRatio != decimal(t, "0.8") || !p.Tiering.AutoRestore || p.Tiering.AutoRestoreDays != 1 || p.Tiering.AutoRestoreTier != "Standard" {
		t.Fatal(p.Tiering)
	}
	if !p.Compression.Enabled || p.Compression.Codec != "zstd" || p.Compression.Level != 3 || p.Compression.MinSavingRatio != decimal(t, "0.1") {
		t.Fatal(p.Compression)
	}
	if p.Chunking != (c.ChunkingPolicy{MinBytes: 262144, AvgBytes: 1048576, MaxBytes: 4194304}) {
		t.Fatal(p.Chunking)
	}
	if c.DefaultCachePolicy() != (c.CachePolicy{AdmitOnWrite: true, ReadaheadChunks: 4}) {
		t.Fatal(c.DefaultCachePolicy())
	}
	if err := validator(t).ValidatePolicy(p); err != nil {
		t.Fatal(err)
	}
	p.Chunking.MinBytes = 1
	if c.DefaultGlobalPolicy().Chunking.MinBytes != 262144 {
		t.Fatal("defaults share mutable state")
	}
}

func TestDeferredValidationDependencies(t *testing.T) {
	if _, err := c.NewPolicyValidator(c.PolicyLimits{}); !errors.Is(err, c.ErrPolicyDeferred) {
		t.Fatal(err)
	}
	if err := (c.PolicyValidator{}).ValidatePolicy(c.DefaultGlobalPolicy()); !errors.Is(err, c.ErrPolicyDeferred) {
		t.Fatal(err)
	}
	if err := (c.PolicyValidator{}).ValidateCache(c.DefaultCachePolicy()); !errors.Is(err, c.ErrPolicyDeferred) {
		t.Fatal(err)
	}
	for _, mutate := range []func(*c.PolicyLimits){
		func(l *c.PolicyLimits) { l.MaxJSONBytes = 0 }, func(l *c.PolicyLimits) { l.MaxDecimalDigits = 0 },
		func(l *c.PolicyLimits) { l.MaxDecimalExponent = 0 }, func(l *c.PolicyLimits) { l.MaxColdAfterDays = 0 },
		func(l *c.PolicyLimits) { l.MaxAutoRestoreDays = 0 }, func(l *c.PolicyLimits) { l.MaxReadaheadChunks = 0 },
		func(l *c.PolicyLimits) { l.RestoreTiers = nil }, func(l *c.PolicyLimits) { l.RestoreTiers[0].Tier = "" },
		func(l *c.PolicyLimits) { l.RestoreTiers[0].Class = storage.Standard },
	} {
		l := testPolicyLimits()
		mutate(&l)
		if _, err := c.NewPolicyValidator(l); !errors.Is(err, c.ErrPolicyDeferred) {
			t.Fatal(err)
		}
	}
	limits := testPolicyLimits()
	v, err := c.NewPolicyValidator(limits)
	if err != nil {
		t.Fatal(err)
	}
	limits.RestoreTiers[0].Tier = "mutated"
	if err := v.ValidatePolicy(c.DefaultGlobalPolicy()); err != nil {
		t.Fatal("validator retained caller slice", err)
	}
	limits = testPolicyLimits()
	limits.RestoreTiers = limits.RestoreTiers[1:]
	v, err = c.NewPolicyValidator(limits)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.ValidatePolicy(c.DefaultGlobalPolicy()); !errors.Is(err, c.ErrPolicyInvalid) {
		t.Fatal("missing IR/tier rule silently accepted", err)
	}
}

func TestGlobalPolicyValidation(t *testing.T) {
	v := validator(t)
	for _, tc := range []struct {
		name   string
		mutate func(*c.GlobalPolicy)
	}{
		{"warm class", func(p *c.GlobalPolicy) { p.Tiering.WarmStorageClass = storage.Glacier }},
		{"P2 class", func(p *c.GlobalPolicy) { p.Tiering.ColdStorageClass = "DEEP_ARCHIVE" }},
		{"negative age", func(p *c.GlobalPolicy) { p.Tiering.ColdAfterDays = -1 }},
		{"age cap", func(p *c.GlobalPolicy) { p.Tiering.ColdAfterDays = 1001 }},
		{"restore zero", func(p *c.GlobalPolicy) { p.Tiering.AutoRestoreDays = 0 }},
		{"restore cap", func(p *c.GlobalPolicy) { p.Tiering.AutoRestoreDays = 31 }},
		{"unknown tier", func(p *c.GlobalPolicy) { p.Tiering.AutoRestoreTier = "arbitrary" }},
		{"tier incompatible", func(p *c.GlobalPolicy) { p.Tiering.AutoRestoreTier = "Bulk" }},
		{"raw policy codec", func(p *c.GlobalPolicy) { p.Compression.Codec = "raw" }},
		{"unsupported level", func(p *c.GlobalPolicy) { p.Compression.Level = 2 }},
		{"min floor", func(p *c.GlobalPolicy) { p.Chunking.MinBytes = 16383 }},
		{"min order", func(p *c.GlobalPolicy) { p.Chunking.MinBytes = p.Chunking.AvgBytes + 1 }},
		{"max order", func(p *c.GlobalPolicy) { p.Chunking.MaxBytes = p.Chunking.AvgBytes - 1 }},
		{"max ceiling", func(p *c.GlobalPolicy) { p.Chunking.MaxBytes = 16777217 }},
		{"non power two", func(p *c.GlobalPolicy) { p.Chunking.AvgBytes = 1048577 }},
		{"missing decimal", func(p *c.GlobalPolicy) { p.Compression.MinSavingRatio = c.Decimal{} }},
		{"disabled still validated", func(p *c.GlobalPolicy) { p.Compression.Enabled = false; p.Compression.Codec = "raw" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := c.DefaultGlobalPolicy()
			tc.mutate(&p)
			if err := v.ValidatePolicy(p); !errors.Is(err, c.ErrPolicyInvalid) {
				t.Fatal(err)
			}
		})
	}
	for _, n := range []int64{16384, 16777216} {
		p := c.DefaultGlobalPolicy()
		p.Chunking = c.ChunkingPolicy{MinBytes: n, AvgBytes: n, MaxBytes: n}
		if err := v.ValidatePolicy(p); err != nil {
			t.Fatal(n, err)
		}
	}
	for _, level := range []int64{1, 3, 7, 11} {
		p := c.DefaultGlobalPolicy()
		p.Compression.Level = level
		if err := v.ValidatePolicy(p); err != nil {
			t.Fatal(err)
		}
	}
	p := c.DefaultGlobalPolicy()
	p.Tiering.ColdAfterDays = 0
	p.Tiering.ColdStorageClass = storage.Glacier
	p.Tiering.AutoRestoreTier = "Bulk"
	p.Tiering.WarmStorageClass = storage.StandardIA
	if err := v.ValidatePolicy(p); err != nil {
		t.Fatal(err)
	}
	limits := testPolicyLimits()
	limits.MaxColdAfterDays = math.MaxInt64
	limits.MaxAutoRestoreDays = math.MaxInt64
	v, err := c.NewPolicyValidator(limits)
	if err != nil {
		t.Fatal(err)
	}
	p = c.DefaultGlobalPolicy()
	p.Tiering.ColdAfterDays = math.MaxInt64
	if err := v.ValidatePolicy(p); !errors.Is(err, c.ErrPolicyInvalid) {
		t.Fatal("duration overflow accepted", err)
	}
	p = c.DefaultGlobalPolicy()
	p.Tiering.AutoRestoreDays = math.MaxInt64
	if err := v.ValidatePolicy(p); !errors.Is(err, c.ErrPolicyInvalid) {
		t.Fatal(err)
	}
}

func TestExactDecimalBoundaries(t *testing.T) {
	v := validator(t)
	for _, tc := range []struct {
		number                 string
		savingValid, liveValid bool
	}{
		{"0", true, true}, {"-0.0e9", true, true}, {"0.1", true, true}, {"1e-1", true, true},
		{"0.9999999999999999999999999999999999999999", true, true},
		{"1", false, true}, {"1.0000000000000000000000000000000000000001", false, false},
		{"-0.0000000000000000000000000000000000000001", false, false},
		{"1e257", false, false}, {"1e-257", false, false},
	} {
		t.Run(tc.number, func(t *testing.T) {
			d := decimal(t, tc.number)
			p := c.DefaultGlobalPolicy()
			p.Compression.MinSavingRatio = d
			if got := v.ValidatePolicy(p) == nil; got != tc.savingValid {
				t.Fatal("saving", tc.number, got)
			}
			p = c.DefaultGlobalPolicy()
			p.Tiering.MinPackLiveRatio = d
			if got := v.ValidatePolicy(p) == nil; got != tc.liveValid {
				t.Fatal("live", tc.number, got)
			}
		})
	}
	if decimal(t, "0.10000") != decimal(t, "10e-2") {
		t.Fatal("equivalent decimals differ")
	}
	for _, input := range []string{`"0.1"`, `null`, `true`, `[]`, `{}`, `NaN`, `01`, `1e9223372036854775808`, `1.1e-9223372036854775808`, `10e9223372036854775807`} {
		var d c.Decimal
		if err := json.Unmarshal([]byte(input), &d); err == nil {
			t.Fatal("accepted decimal", input)
		}
	}
	p := c.DefaultGlobalPolicy()
	p.Compression.MinSavingRatio = decimal(t, "0."+strings.Repeat("9", 129))
	if err := v.ValidatePolicy(p); !errors.Is(err, c.ErrPolicyInvalid) {
		t.Fatal("precision bound ignored", err)
	}
}

func TestStrictGlobalUpdateJSON(t *testing.T) {
	v := validator(t)
	current := globalSnapshot(t, v, 3)
	for _, input := range []string{
		`null`, `[]`, `true`, `{"tiering":null}`, `{"tiering":{"enabled":null}}`,
		`{"tiering":{"enabled":true,"enabled":false}}`, `{"tiering":{"enabled":true,"\u0065nabled":false}}`,
		`{"tiering":{},"tiering":{}}`, `{"expected_revision":3,"expected_revision":3}`,
		`{"unknown":1}`, `{"cache":{}}`, `{"dedup_domain":"new"}`, `{"expected_bucket_id":"bucket-one"}`,
		`{"tiering":{"unknown":1}}`, `{"Tiering":{}}`, `{"tiering":[]}`,
		`{"compression":{"enabled":"true"}}`, `{"compression":{"codec":3}}`, `{"compression":{"level":3.0}}`,
		`{"chunking":{"min_bytes":"16384"}}`, `{"chunking":{"min_bytes":1.5}}`, `{"chunking":{"min_bytes":9223372036854775808}}`,
		`{"compression":{"min_saving_ratio":"0.1"}}`, `{"compression":{"min_saving_ratio":true}}`,
		`{} {}`, `{} true`, `{} garbage`, `{"expected_revision":null}`, `{"expected_revision":"3"}`,
		`{"expected_revision":0}`, `{"expected_revision":-1}`, `{"expected_revision":3.0}`, `{"expected_revision":9007199254740992}`,
	} {
		if _, err := v.MergeGlobal(current, []byte(input), 3); !errors.Is(err, c.ErrPolicyInvalid) {
			t.Errorf("accepted %s: %v", input, err)
		}
	}
	if _, err := v.MergeGlobal(current, []byte(`{}`), 0); !errors.Is(err, c.ErrPolicyInvalid) {
		t.Fatal("missing token", err)
	}
	if _, err := v.MergeGlobal(current, []byte(`{"expected_revision":2}`), 3); !errors.Is(err, c.ErrPolicyInvalid) {
		t.Fatal("token mismatch", err)
	}
	if _, err := v.MergeGlobal(current, []byte(`{}`), storage.MaxPolicyRevision+1); !errors.Is(err, c.ErrPolicyInvalid) {
		t.Fatal(err)
	}
	if _, err := v.MergeGlobal(current, []byte(strings.Repeat(" ", 8193)), 3); !errors.Is(err, c.ErrPolicyInvalid) {
		t.Fatal("JSON budget ignored", err)
	}
	invalidUTF8 := []byte{'{', '"', 0xff, '"', ':', '0', '}'}
	if _, err := v.MergeGlobal(current, invalidUTF8, 3); !errors.Is(err, c.ErrPolicyInvalid) {
		t.Fatal(err)
	}
}

func TestGlobalMergeNoOpAndSnapshotOwnership(t *testing.T) {
	v := validator(t)
	current := globalSnapshot(t, v, 3)
	proposal, err := v.MergeGlobal(current, []byte(`{"expected_revision":3,"tiering":{"cold_storage_class":"GLACIER","cold_after_days":0},"compression":{"enabled":false}}`), 0)
	if err != nil {
		t.Fatal(err)
	}
	want := current.Policy()
	want.Tiering.ColdStorageClass = storage.Glacier
	want.Tiering.ColdAfterDays = 0
	want.Compression.Enabled = false
	if !proposal.Changed() || proposal.Policy() != want || proposal.ExpectedRevision() != 3 {
		t.Fatal(proposal)
	}
	if current.Policy() != c.DefaultGlobalPolicy() || current.Revision() != 3 {
		t.Fatal("merge mutated pinned snapshot")
	}
	candidate, err := proposal.Candidate()
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Revision != 0 || candidate.Version != storage.ModelVersion {
		t.Fatal(candidate)
	}
	baseline := append([]byte(nil), candidate.Document...)
	candidate.Document[0] = 'x'
	again, err := proposal.Candidate()
	if err != nil || !bytes.Equal(baseline, again.Document) {
		t.Fatal("candidate shares caller bytes", err)
	}
	copyOfPolicy := current.Policy()
	copyOfPolicy.Compression.Level = 11
	if current.Policy().Compression.Level != 3 {
		t.Fatal("snapshot shared mutable fields")
	}
	for run := 0; run < 100; run++ {
		p, err := v.MergeGlobal(current, []byte(`{"compression":{"min_saving_ratio":10e-2},"expected_revision":3}`), 0)
		if err != nil || p.Changed() {
			t.Fatal("semantic decimal no-op", p, err)
		}
		doc, err := p.Candidate()
		if err != nil {
			t.Fatal(err)
		}
		original, err := current.StorageSnapshot()
		if err != nil || !bytes.Equal(doc.Document, original.Document) {
			t.Fatal("nondeterministic encoding", err)
		}
	}
	if _, err := v.MergeGlobal(current, []byte(`{"expected_revision":2}`), 0); !errors.Is(err, c.ErrPolicyConflict) {
		t.Fatal("stale no-op", err)
	}
}

func TestBucketMergeAndPinnedEffectiveRevisions(t *testing.T) {
	v := validator(t)
	global := globalSnapshot(t, v, 3)
	bucket := bucketSnapshot(t, v, 7)
	effective, err := c.EffectivePolicy(global, bucket)
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := v.MergeBucket(bucket, []byte(`{"expected_revision":7,"expected_bucket_id":"bucket-one","cache":{"admit_on_write":false,"readahead_chunks":0}}`), 0)
	if err != nil {
		t.Fatal(err)
	}
	if proposal.Cache() != (c.CachePolicy{AdmitOnWrite: false, ReadaheadChunks: 0}) || !proposal.Changed() || proposal.ExpectedRevision() != 7 || proposal.ExpectedBucketID() != "bucket-one" {
		t.Fatal(proposal)
	}
	updated, err := v.NewBucketSnapshot("bucket-one", 8, proposal.Cache())
	if err != nil {
		t.Fatal(err)
	}
	newEffective, err := c.EffectivePolicy(global, updated)
	if err != nil {
		t.Fatal(err)
	}
	if effective.Global().Revision() != 3 || effective.Bucket().Revision() != 7 || effective.Bucket().Cache() != c.DefaultCachePolicy() {
		t.Fatal("old effective snapshot changed")
	}
	if newEffective.Global().Policy() != global.Policy() || newEffective.Bucket().Revision() != 8 {
		t.Fatal(newEffective)
	}
	omitted, err := v.MergeBucket(bucket, []byte(`{"expected_bucket_id":"bucket-one","cache":{"admit_on_write":false}}`), 7)
	if err != nil || omitted.Cache().ReadaheadChunks != 4 {
		t.Fatal(omitted, err)
	}
	noop, err := v.MergeBucket(bucket, []byte(`{"expected_bucket_id":"bucket-one"}`), 7)
	if err != nil || noop.Changed() {
		t.Fatal(noop, err)
	}
	for _, input := range []string{
		`{}`, `{"expected_bucket_id":null}`, `{"expected_bucket_id":3}`, `{"expected_bucket_id":""}`,
		`{"expected_bucket_id":"bucket-one","tiering":{}}`, `{"expected_bucket_id":"bucket-one","compression":{}}`,
		`{"expected_bucket_id":"bucket-one","chunking":{}}`, `{"expected_bucket_id":"bucket-one","dedup_domain":"x"}`,
		`{"expected_bucket_id":"bucket-one","cache":{"unknown":1}}`, `{"expected_bucket_id":"bucket-one","cache":{"readahead_chunks":-1}}`,
		`{"expected_bucket_id":"bucket-one","cache":{"readahead_chunks":65}}`, `{"expected_bucket_id":"bucket-one","cache":{"readahead_chunks":null}}`,
		`{"expected_bucket_id":"bucket-one","cache":{"admit_on_write":false,"admit_on_write":true}}`,
	} {
		if _, err := v.MergeBucket(bucket, []byte(input), 7); !errors.Is(err, c.ErrPolicyInvalid) {
			t.Errorf("accepted bucket %s: %v", input, err)
		}
	}
	if _, err := v.MergeBucket(bucket, []byte(`{"expected_bucket_id":"bucket-new"}`), 7); !errors.Is(err, c.ErrPolicyConflict) {
		t.Fatal(err)
	}
	if _, err := v.MergeBucket(bucket, []byte(`{"expected_bucket_id":"bucket-one"}`), 6); !errors.Is(err, c.ErrPolicyConflict) {
		t.Fatal(err)
	}
}

func TestCompleteSnapshotsHistoryAndRevisionDomain(t *testing.T) {
	v := validator(t)
	current := globalSnapshot(t, v, 3)
	record, err := current.StorageSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := v.ResolveGlobal(3, record, nil)
	if err != nil || decoded.Policy() != current.Policy() {
		t.Fatal(decoded, err)
	}
	if _, err := v.ResolveGlobal(0, record, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := v.ResolveGlobal(2, record, nil); !errors.Is(err, c.ErrPolicyInvalid) {
		t.Fatal("wrong history revision", err)
	}
	if _, err := v.ResolveGlobal(2, record, storage.ErrNotFound); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("history fallback", err)
	}
	if _, err := v.ResolveGlobal(0, record, storage.ErrUnavailable); !errors.Is(err, storage.ErrUnavailable) {
		t.Fatal(err)
	}
	for _, revision := range []uint64{0, storage.MaxPolicyRevision + 1} {
		if _, err := v.NewGlobalSnapshot(revision, c.DefaultGlobalPolicy()); !errors.Is(err, c.ErrPolicyInvalid) {
			t.Fatal(err)
		}
		if _, err := v.NewBucketSnapshot("bucket-one", revision, c.DefaultCachePolicy()); !errors.Is(err, c.ErrPolicyInvalid) {
			t.Fatal(err)
		}
	}
	for _, doc := range []string{`{}`, `{"tiering":{},"compression":{},"chunking":{}}`, `{"tiering":{"enabled":true}}`} {
		bad := record
		bad.Document = []byte(doc)
		if _, err := v.DecodeGlobal(bad); !errors.Is(err, c.ErrPolicyInvalid) {
			t.Fatal("incomplete snapshot defaulted", err)
		}
	}
	bad := record
	bad.Version++
	if _, err := v.DecodeGlobal(bad); !errors.Is(err, c.ErrPolicyInvalid) {
		t.Fatal(err)
	}
	record.Document[0] = 'x'
	if decoded.Policy() != current.Policy() {
		t.Fatal("snapshot retains input bytes")
	}
	cache, err := v.DecodeBucket("bucket-one", 7, []byte(`{"cache":{"admit_on_write":false,"readahead_chunks":0}}`))
	if err != nil || cache.Cache().AdmitOnWrite || cache.Cache().ReadaheadChunks != 0 {
		t.Fatal(cache, err)
	}
	if _, err := v.DecodeBucket("bucket-one", 7, []byte(`{"cache":{}}`)); !errors.Is(err, c.ErrPolicyInvalid) {
		t.Fatal(err)
	}
	maxGlobal := globalSnapshot(t, v, storage.MaxPolicyRevision)
	if p, err := v.MergeGlobal(maxGlobal, []byte(`{}`), storage.MaxPolicyRevision); err != nil || p.Changed() {
		t.Fatal(p, err)
	}
	if _, err := v.MergeGlobal(maxGlobal, []byte(`{"compression":{"enabled":false}}`), storage.MaxPolicyRevision); !errors.Is(err, c.ErrPolicyRevisionExhausted) {
		t.Fatal(err)
	}
	maxBucket := bucketSnapshot(t, v, storage.MaxPolicyRevision)
	if _, err := v.MergeBucket(maxBucket, []byte(`{"expected_bucket_id":"bucket-one","cache":{"readahead_chunks":0}}`), storage.MaxPolicyRevision); !errors.Is(err, c.ErrPolicyRevisionExhausted) {
		t.Fatal(err)
	}
	if _, err := (c.GlobalSnapshot{}).StorageSnapshot(); !errors.Is(err, c.ErrPolicyInvalid) {
		t.Fatal(err)
	}
	if _, err := (c.GlobalProposal{}).Candidate(); !errors.Is(err, c.ErrPolicyInvalid) {
		t.Fatal(err)
	}
	if _, err := c.EffectivePolicy(c.GlobalSnapshot{}, cache); !errors.Is(err, c.ErrPolicyInvalid) {
		t.Fatal(err)
	}
	if !reflect.TypeFor[c.GlobalPolicy]().Comparable() || !reflect.TypeFor[c.CachePolicy]().Comparable() {
		t.Fatal("policy values unexpectedly share mutable data")
	}
}
