package policy

import (
	"errors"
	"math"
	"math/big"
	"reflect"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/clock"
)

func utcFixture(t *testing.T) (UTCSource, UTCSample, Request) {
	i := fixture(t)
	source := UTCSource{ID: "reference", Provenance: "modeled-verified", RestartID: "run-1", Verified: Valid}
	sample := UTCSample{SourceID: source.ID, Provenance: source.Provenance, RestartID: source.RestartID, Observed: i.Request.Now, ValidThrough: i.Request.Deadline, Lower: i.UTC.Lower, Upper: i.UTC.Upper, Precision: DurationBound{Known: true, Value: 3}, MeasurementError: DurationBound{Known: true, Value: 2}, NodeSkew: DurationBound{Known: true}, Drift: DriftBound{Known: true, PPB: 100}, Sync: Valid}
	return source, sample, i.Request
}

func TestUTCSampleAgeAndSkew(t *testing.T) {
	for _, age := range []time.Duration{5*time.Second - 1, 5 * time.Second, 5*time.Second + 1} {
		s, sample, r := utcFixture(t)
		r.Now, _ = r.Now.Add(age)
		p, e := ValidateUTCSample(s, sample, r)
		if age >= 5*time.Second {
			if !errors.Is(e, ErrUTCStale) || p.validated {
				t.Fatal(age, p, e)
			}
		} else if e != nil || !p.validated {
			t.Fatal(age, p, e)
		}
	}
	for _, sign := range []time.Duration{-1, 1} {
		for _, magnitude := range []time.Duration{2*time.Second - 1, 2 * time.Second, 2*time.Second + 1} {
			s, sample, r := utcFixture(t)
			sample.NodeSkew.Value = sign * magnitude
			p, e := ValidateUTCSample(s, sample, r)
			if (e == nil) != (magnitude <= 2*time.Second) {
				t.Fatal(sign*magnitude, p, e)
			}
			// Skew equality không bù được drift evidence bị thiếu.
			sample.Drift.Known = false
			if _, e := ValidateUTCSample(s, sample, r); e == nil {
				t.Fatal("skew tự tạo proof")
			}
		}
	}
}

func TestUTCInvalidSamples(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*UTCSource, *UTCSample, *Request)
		want   error
	}{
		{"future", func(_ *UTCSource, s *UTCSample, _ *Request) { s.Observed, _ = s.Observed.Add(1) }, ErrEvidence},
		{"unknown source", func(s *UTCSource, _ *UTCSample, _ *Request) { s.ID = "" }, ErrUTCSource},
		{"unverified", func(s *UTCSource, _ *UTCSample, _ *Request) { s.Verified = Unknown }, ErrUTCSource},
		{"wrong source", func(_ *UTCSource, s *UTCSample, _ *Request) { s.SourceID = "other" }, ErrUTCSource},
		{"no provenance", func(_ *UTCSource, s *UTCSample, _ *Request) { s.Provenance = "" }, ErrUTCSource},
		{"restart mismatch", func(_ *UTCSource, s *UTCSample, _ *Request) { s.RestartID = "old-run" }, ErrUTCSource},
		{"unsynced", func(_ *UTCSource, s *UTCSample, _ *Request) { s.Sync = Invalid }, ErrUTCUnsynced},
		{"sync unknown", func(_ *UTCSource, s *UTCSample, _ *Request) { s.Sync = Unknown }, ErrUTCUnsynced},
		{"precision missing", func(_ *UTCSource, s *UTCSample, _ *Request) { s.Precision.Known = false }, ErrEvidence},
		{"error missing", func(_ *UTCSource, s *UTCSample, _ *Request) { s.MeasurementError.Known = false }, ErrEvidence},
		{"skew missing", func(_ *UTCSource, s *UTCSample, _ *Request) { s.NodeSkew.Known = false }, ErrEvidence},
		{"drift missing", func(_ *UTCSource, s *UTCSample, _ *Request) { s.Drift.Known = false }, ErrEvidence},
		{"negative precision", func(_ *UTCSource, s *UTCSample, _ *Request) { s.Precision.Value = -1 }, ErrEvidence},
		{"invalid interval", func(_ *UTCSource, s *UTCSample, _ *Request) { s.Lower = s.Upper.Add(1) }, ErrEvidence},
		{"missing UTC", func(_ *UTCSource, s *UTCSample, _ *Request) { s.Lower = time.Time{} }, ErrEvidence},
		{"insufficient horizon", func(_ *UTCSource, s *UTCSample, r *Request) { s.ValidThrough, _ = r.Deadline.Add(-1) }, ErrUTCHorizon},
		{"missing horizon", func(_ *UTCSource, s *UTCSample, _ *Request) { s.ValidThrough = clock.Instant{} }, clock.ErrDomain},
		{"padding overflow", func(_ *UTCSource, s *UTCSample, _ *Request) {
			s.Precision.Value = time.Duration(math.MaxInt64)
			s.MeasurementError.Value = 1
		}, clock.ErrOverflow},
		{"drift overflow", func(_ *UTCSource, s *UTCSample, _ *Request) { s.Drift.PPB = math.MaxUint64 }, clock.ErrOverflow},
		{"UTC overflow", func(_ *UTCSource, s *UTCSample, _ *Request) {
			s.Upper = time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC)
		}, clock.ErrOverflow},
		{"UTC underflow", func(_ *UTCSource, s *UTCSample, _ *Request) { s.Lower = time.Date(1, 1, 1, 0, 0, 0, 1, time.UTC) }, clock.ErrOverflow},
		{"cancelled", func(_ *UTCSource, _ *UTCSample, r *Request) { r.Cancelled = true }, ErrEvidence},
		{"deadline equal", func(_ *UTCSource, _ *UTCSample, r *Request) { r.Now = r.Deadline }, ErrEvidence},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, sample, r := utcFixture(t)
			tc.change(&s, &sample, &r)
			before := sample
			p, e := ValidateUTCSample(s, sample, r)
			if !errors.Is(e, tc.want) || p.validated || !p.Lower.IsZero() || !p.Upper.IsZero() {
				t.Fatal(p, e)
			}
			if !reflect.DeepEqual(before, sample) {
				t.Fatal("mutate sample")
			}
		})
	}
	s, sample, r := utcFixture(t)
	other, e := clock.NewVirtual(sample.Lower, 10)
	if e != nil {
		t.Fatal(e)
	}
	for _, horizon := range []bool{false, true} {
		bad := sample
		if horizon {
			bad.ValidThrough = other.Now().Instant
		} else {
			bad.Observed = other.Now().Instant
		}
		if _, e := ValidateUTCSample(s, bad, r); !errors.Is(e, clock.ErrDomain) {
			t.Fatal(e)
		}
	}
}

func TestUTCConservativeBindingAndDecisions(t *testing.T) {
	s, sample, r := utcFixture(t)
	p, e := ValidateUTCSample(s, sample, r)
	if e != nil {
		t.Fatal(e)
	}
	// 60s * 100ppb = 6000ns, precision3ns + measurement2ns.
	if !p.Lower.Equal(sample.Lower.Add(-6005)) || !p.Upper.Equal(sample.Upper.Add(6005)) {
		t.Fatal(p)
	}
	r2 := r
	r2.Now, _ = r.Now.Add(time.Second)
	p2, e := ValidateUTCSample(s, sample, r2)
	if e != nil {
		t.Fatal(e)
	}
	if p2.Upper.Sub(p2.Lower) < p.Upper.Sub(p.Lower) || !p2.Upper.Equal(p.Upper.Add(time.Second)) {
		t.Fatal("thu hẹp uncertainty", p, p2)
	}
	for _, offset := range []time.Duration{-1, 0, 1} {
		in := restored(t)
		in.Request = r
		in.UTC = p
		in.Pack.RestoredUntil = p.Upper.Add(time.Minute + 120*time.Second).Add(offset)
		d, e := EvaluateRead(in)
		want := Eligible
		if offset < 0 {
			want = Denied
		}
		if e != nil || d.Verdict != want {
			t.Fatal(d, e)
		}
	}
	in := protectionFixture(t)
	in.Request = r
	in.UTC = p
	in.Precision = time.Second
	d, e := EvaluateProtection(in)
	if e != nil {
		t.Fatal(e)
	}
	want := sample.Upper.Add(16*time.Minute + time.Second)
	if !d.Minimum.Equal(want) {
		t.Fatal(d)
	}
	in.Pack.ProtectedUntil = want.Add(time.Hour)
	d, e = EvaluateProtection(in)
	if e != nil || d.Target.Before(in.Pack.ProtectedUntil) {
		t.Fatal(d, e)
	}
	// Proof cũ không được tái dùng tại Now mới; recompute từ sample vẫn hợp lệ.
	in.Request = r2
	if d, e := EvaluateProtection(in); !errors.Is(e, ErrEvidence) || d.Verdict != Indeterminate {
		t.Fatal(d, e)
	}
	in.UTC = p2
	if d, e := EvaluateProtection(in); e != nil || d.Verdict != Eligible {
		t.Fatal(d, e)
	}
	for _, mutate := range []func(*UTCProof){func(p *UTCProof) { p.Lower = p.Lower.Add(1) }, func(p *UTCProof) { p.Upper = p.Upper.Add(-1) }} {
		bad := p
		mutate(&bad)
		in.Request = r
		in.UTC = bad
		if d, e := EvaluateProtection(in); !errors.Is(e, ErrEvidence) || d.Verdict != Indeterminate {
			t.Fatal(d, e)
		}
	}
	in.Request = r
	in.UTC = UTCProof{Trusted: true, Lower: p.Lower, Upper: p.Upper, Horizon: r.Deadline}
	if d, e := EvaluateProtection(in); e != nil || d.Verdict != Indeterminate || d.Reason != MissingEvidence {
		t.Fatal("Trusted bypass", d, e)
	}
	read := restored(t)
	read.Request = r
	read.UTC = in.UTC
	if d, e := EvaluateRead(read); e != nil || d.Verdict != Indeterminate || d.Reason != MissingEvidence {
		t.Fatal("read Trusted bypass", d, e)
	}
	read.UTC = p
	read.Request = r2
	if d, e := EvaluateRead(read); !errors.Is(e, ErrEvidence) || d.Verdict != Indeterminate {
		t.Fatal("read reused proof", d, e)
	}
}

func TestUTCDriftCeilAndHorizonOverflow(t *testing.T) {
	s, sample, r := utcFixture(t)
	r.Deadline, _ = r.Now.Add(1)
	sample.ValidThrough = r.Deadline
	sample.Precision.Value = 0
	sample.MeasurementError.Value = 0
	sample.Drift.PPB = 1
	p, e := ValidateUTCSample(s, sample, r)
	if e != nil || !p.Upper.Equal(sample.Upper.Add(1)) || !p.Lower.Equal(sample.Lower.Add(-1)) {
		t.Fatal("drift fractional ns phải ceil", p, e)
	}
	s, sample, r = utcFixture(t)
	sample.Observed, _ = r.Now.Add(-1)
	r.Deadline, _ = r.Now.Add(time.Duration(math.MaxInt64))
	sample.ValidThrough = r.Deadline
	if p, e := ValidateUTCSample(s, sample, r); !errors.Is(e, clock.ErrOverflow) || p.validated {
		t.Fatal("horizon overflow", p, e)
	}
}

func TestUTCBoundRealVirtualParity(t *testing.T) {
	wall := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	virtual, e := clock.NewVirtual(wall, 10)
	if e != nil {
		t.Fatal(e)
	}
	var firstLower, firstUpper time.Time
	for n, c := range []clock.Clock{clock.NewReal(), virtual} {
		now := c.Now().Instant
		observed, e := now.Add(-time.Second)
		if e != nil {
			t.Fatal(e)
		}
		deadline, e := now.Add(time.Minute)
		if e != nil {
			t.Fatal(e)
		}
		r := Request{Now: now, Deadline: deadline}
		s := UTCSource{ID: "injected", Provenance: "modeled", RestartID: "run", Verified: Valid}
		sample := UTCSample{SourceID: s.ID, Provenance: s.Provenance, RestartID: s.RestartID, Observed: observed, ValidThrough: deadline, Lower: wall, Upper: wall, Precision: DurationBound{Known: true, Value: 1}, MeasurementError: DurationBound{Known: true, Value: 2}, NodeSkew: DurationBound{Known: true, Value: MaxNodeSkew}, Drift: DriftBound{Known: true, PPB: 101}, Sync: Valid}
		p, e := ValidateUTCSample(s, sample, r)
		if e != nil {
			t.Fatal(e)
		}
		// ceil(61s*101ppb)=6161ns, cộng 3ns measurement/precision.
		if !p.Upper.Equal(wall.Add(time.Second+6164)) || !p.Lower.Equal(wall.Add(time.Second-6164)) {
			t.Fatal(p)
		}
		if n == 0 {
			firstLower, firstUpper = p.Lower, p.Upper
		} else if !p.Lower.Equal(firstLower) || !p.Upper.Equal(firstUpper) {
			t.Fatal("parity")
		}
		read := restored(t)
		read.Request = r
		read.UTC = p
		read.Pack.RestoredUntil = p.Upper.Add(time.Minute + 120*time.Second)
		if d, e := EvaluateRead(read); e != nil || d.Verdict != Eligible {
			t.Fatal(d, e)
		}
	}
}

func TestUTCProofHorizonMutationRegression(t *testing.T) {
	for _, consumer := range []string{"read", "protection"} {
		t.Run(consumer, func(t *testing.T) {
			s, sample, r := utcFixture(t)
			proof, err := ValidateUTCSample(s, sample, r)
			if err != nil {
				t.Fatal(err)
			}
			changed, err := proof.Horizon.Add(time.Nanosecond)
			if err != nil {
				t.Fatal(err)
			}
			cmp, err := changed.Compare(proof.Horizon)
			if err != nil || cmp != 1 {
				t.Fatal("không cùng domain", cmp, err)
			}
			proof.Horizon = changed
			if consumer == "read" {
				in := restored(t)
				in.Request = r
				in.UTC = proof
				in.Pack.RestoredUntil = proof.Upper.Add(time.Hour)
				d, err := EvaluateRead(in)
				if !errors.Is(err, ErrEvidence) || d.Verdict != Indeterminate || d.Reason != InvalidEvidence || d.Actions != nil {
					t.Fatal(d, err)
				}
			} else {
				in := protectionFixture(t)
				in.Request = r
				in.UTC = proof
				d, err := EvaluateProtection(in)
				if !errors.Is(err, ErrEvidence) || d.Verdict != Indeterminate || d.Reason != InvalidEvidence || !d.Minimum.IsZero() || !d.Target.IsZero() || d.ExtensionRequired {
					t.Fatal(d, err)
				}
			}
		})
	}
}

func TestUTCPreEpochConservativeOracleRegression(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		age                   time.Duration
		ppb                   uint64
		precision, errorBound time.Duration
	}{
		{"fractional drift", time.Nanosecond, 1, 3, 2},
		{"one second age", time.Second, 101, 7, 11},
		{"near stale", 5*time.Second - 1, 1000000, 100, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, sample, r := utcFixture(t)
			sample.Lower = time.Date(1900, 1, 1, 7, 0, 0, 123456789, time.FixedZone("UTC+7", 7*3600))
			sample.Upper = sample.Lower.Add(13 * time.Nanosecond)
			r.Now, _ = r.Now.Add(tc.age)
			r.Deadline, _ = r.Deadline.Add(time.Nanosecond) // tạo fractional drift tick
			sample.ValidThrough = r.Deadline
			sample.Precision.Value = tc.precision
			sample.MeasurementError.Value = tc.errorBound
			sample.Drift.PPB = tc.ppb
			p, err := ValidateUTCSample(s, sample, r)
			if err != nil {
				t.Fatal(err)
			}
			horizon, err := r.Deadline.Sub(sample.Observed)
			if err != nil {
				t.Fatal(err)
			}
			// Oracle độc lập dùng quotient/remainder thay công thức cộng denominator-1.
			product := new(big.Int).Mul(big.NewInt(int64(horizon)), new(big.Int).SetUint64(tc.ppb))
			q, rem := new(big.Int), new(big.Int)
			q.QuoRem(product, big.NewInt(1000000000), rem)
			if rem.Sign() != 0 {
				q.Add(q, big.NewInt(1))
			}
			if !q.IsInt64() {
				t.Fatal("oracle overflow")
			}
			pad := time.Duration(q.Int64()) + tc.precision + tc.errorBound
			wantLower := sample.Lower.UTC().Add(tc.age - pad)
			wantUpper := sample.Upper.UTC().Add(tc.age + pad)
			if !p.Lower.Equal(wantLower) || !p.Upper.Equal(wantUpper) || p.Lower.After(wantLower) || p.Upper.Before(wantUpper) {
				t.Fatal(p, wantLower, wantUpper)
			}
			// Ceil phải bao ít nhất 1ns còn dư; không truncate uncertainty inward.
			if rem.Sign() != 0 {
				floorPad := time.Duration(new(big.Int).Sub(q, big.NewInt(1)).Int64()) + tc.precision + tc.errorBound
				if !p.Lower.Before(sample.Lower.UTC().Add(tc.age-floorPad)) || !p.Upper.After(sample.Upper.UTC().Add(tc.age+floorPad)) {
					t.Fatal("rounding thu hẹp interval", p)
				}
			}
		})
	}
}
