package policy

import (
	"encoding/json"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/storage"
)

func tierFixture(t *testing.T) TieringInput {
	t.Helper()
	f := fixture(t)
	f.Pack.State = "Warm"
	f.Pack.Class = storage.Standard
	f.Pack.LastAccess = f.UTC.Lower.Add(-30 * 24 * time.Hour)
	f.Pack.ProtectedUntil = f.UTC.Lower
	return TieringInput{Policy: f.Policy, Request: f.Request, UTC: f.UTC, Pack: f.Pack, LiveBytes: 80, TotalBytes: 100, Metadata: Valid, Authority: Valid, SafeToTransition: Valid}
}
func tierPolicy(t *testing.T, i *TieringInput, edit func(*config.GlobalPolicy)) {
	t.Helper()
	v, _ := config.NewPolicyValidator(config.DSGPolicyLimits())
	p := i.Policy.Snapshot.Policy()
	edit(&p)
	s, err := v.NewGlobalSnapshot(i.Policy.RequiredRevision, p)
	if err != nil {
		t.Fatal(err)
	}
	i.Policy.Snapshot = s
}
func TestTieringSelectionBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name    string
		change  func(*TieringInput)
		verdict Verdict
		reason  Reason
		wantErr bool
	}{
		{"default IR", func(*TieringInput) {}, Eligible, Ready, false},
		{"age below", func(i *TieringInput) { i.Pack.LastAccess = i.Pack.LastAccess.Add(1) }, Denied, PackTooYoung, false},
		{"age above", func(i *TieringInput) { i.Pack.LastAccess = i.Pack.LastAccess.Add(-1) }, Eligible, Ready, false},
		{"ratio below", func(i *TieringInput) { i.LiveBytes = 79 }, Denied, LiveRatioTooLow, false},
		{"ratio above", func(i *TieringInput) { i.LiveBytes = 81 }, Eligible, Ready, false},
		{"zero total", func(i *TieringInput) { i.TotalBytes = 0 }, Indeterminate, InvalidEvidence, true},
		{"live beyond total", func(i *TieringInput) { i.LiveBytes = 101 }, Indeterminate, InvalidEvidence, true},
		{"negative live", func(i *TieringInput) { i.LiveBytes = -1 }, Indeterminate, InvalidEvidence, true},
		{"protected", func(i *TieringInput) { i.Pack.ProtectedUntil = i.UTC.Lower.Add(1) }, Denied, PackProtected, false},
		{"no protection proof", func(i *TieringInput) { i.Pack.ProtectedUntil = time.Time{} }, Indeterminate, MissingEvidence, false},
		{"retained", func(i *TieringInput) { i.Pack.MinRetentionUntil = i.UTC.Lower.Add(1) }, Denied, PackRetained, false},
		{"retention equality", func(i *TieringInput) { i.Pack.MinRetentionUntil = i.UTC.Lower }, Eligible, Ready, false},
		{"IA missing retention", func(i *TieringInput) { i.Pack.Class = storage.StandardIA }, Indeterminate, MissingEvidence, false},
		{"roots unknown", func(i *TieringInput) { i.SafeToTransition = Unknown }, Indeterminate, MissingEvidence, false},
		{"roots blocked", func(i *TieringInput) { i.SafeToTransition = Invalid }, Denied, NotPermitted, false},
		{"lost fence", func(i *TieringInput) { i.Authority = Invalid }, Denied, NotPermitted, false},
		{"deadline equality", func(i *TieringInput) { i.Request.Now = i.Request.Deadline }, Denied, Expired, false},
		{"disabled", func(i *TieringInput) { tierPolicy(t, i, func(p *config.GlobalPolicy) { p.Tiering.Enabled = false }) }, Denied, TieringDisabled, false},
		{"age zero still protection", func(i *TieringInput) {
			tierPolicy(t, i, func(p *config.GlobalPolicy) { p.Tiering.ColdAfterDays = 0 })
			i.Pack.ProtectedUntil = i.UTC.Lower.Add(1)
		}, Denied, PackProtected, false},
		{"cold not a candidate", func(i *TieringInput) { i.Pack.State = "Cold"; i.Pack.Class = storage.GlacierIR }, Denied, Unavailable, false},
		{"ratio cross multiply beyond int64", func(i *TieringInput) {
			i.Pack.Size = math.MaxInt64
			i.TotalBytes = math.MaxInt64
			i.LiveBytes = math.MaxInt64 - 1
		}, Eligible, Ready, false},
		{"forged UTC", func(i *TieringInput) {
			i.UTC = UTCProof{Trusted: true, Lower: i.UTC.Lower, Upper: i.UTC.Upper, Horizon: i.Request.Deadline}
		}, Indeterminate, MissingEvidence, false},
		{"proof binding changed", func(i *TieringInput) { i.Request.Now, _ = i.Request.Now.Add(1) }, Indeterminate, InvalidEvidence, true},
		{"missing age", func(i *TieringInput) { i.Pack.LastAccess = time.Time{} }, Indeterminate, MissingEvidence, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i := tierFixture(t)
			tc.change(&i)
			before := i
			d, e := EvaluateTiering(i)
			if d.Verdict != tc.verdict || d.Reason != tc.reason || (e != nil) != tc.wantErr {
				t.Fatalf("%+v %v", d, e)
			}
			if d.Verdict != Eligible && d.Destination != "" {
				t.Fatal("partial action", d)
			}
			if !reflect.DeepEqual(i, before) {
				t.Fatal("input mutated")
			}
		})
	}
	i := tierFixture(t)
	tierPolicy(t, &i, func(p *config.GlobalPolicy) { p.Tiering.ColdStorageClass = storage.Glacier })
	if d, e := EvaluateTiering(i); e != nil || d.Destination != storage.Glacier {
		t.Fatal(d, e)
	}
	// Exact normalized decimal arithmetic at the threshold, without float64.
	var ratio config.Decimal
	_ = json.Unmarshal([]byte(`0.8000000000000000000000000000000001`), &ratio)
	tierPolicy(t, &i, func(p *config.GlobalPolicy) { p.Tiering.MinPackLiveRatio = ratio })
	if d, e := EvaluateTiering(i); e != nil || d.Reason != LiveRatioTooLow {
		t.Fatal(d, e)
	}
}

func TestTieringRealVirtualParityAndNoImmediatePUTTiering(t *testing.T) {
	wall := time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)
	virtual, _ := clock.NewVirtual(wall, 32)
	var decisions []TieringDecision
	for _, c := range []clock.Clock{clock.NewReal(), virtual} {
		i := tierFixture(t)
		i.Request.Now = c.Now().Instant
		i.Request.Deadline, _ = i.Request.Now.Add(time.Minute)
		// Controlled source fixture uses each clock's own domain, not host wall.
		i.UTC = testUTC(t, i.Request, wall, wall)
		i.Pack.LastAccess = wall.Add(-30 * 24 * time.Hour)
		i.Pack.ProtectedUntil = wall
		d, e := EvaluateTiering(i)
		if e != nil {
			t.Fatal(e)
		}
		decisions = append(decisions, d)
		tierPolicy(t, &i, func(p *config.GlobalPolicy) { p.Tiering.ColdAfterDays = 0 })
		i.Pack.ProtectedUntil = wall.Add(time.Minute + 15*time.Minute)
		if d, e := EvaluateTiering(i); e != nil || d.Verdict != Denied || d.Reason != PackProtected {
			t.Fatal("new PUT tiered", d, e)
		}
	}
	if !reflect.DeepEqual(decisions[0], decisions[1]) {
		t.Fatal("parity", decisions)
	}
	virtual.SetWall(wall.Add(-time.Hour))
	i := tierFixture(t)
	i.Request.Now = virtual.Now().Instant
	i.Request.Deadline, _ = i.Request.Now.Add(time.Minute)
	i.UTC = testUTC(t, i.Request, wall, wall)
	i.Pack.LastAccess = wall.Add(-30 * 24 * time.Hour)
	i.Pack.ProtectedUntil = wall
	if d, e := EvaluateTiering(i); e != nil || !reflect.DeepEqual(d, decisions[0]) {
		t.Fatal("host wall used", d, e)
	}
}
