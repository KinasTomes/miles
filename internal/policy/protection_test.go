package policy

import (
	"errors"
	"math"
	"math/big"
	"reflect"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/storage"
)

func protectionFixture(t *testing.T) ProtectionInput {
	i := fixture(t)
	return ProtectionInput{Policy: i.Policy, Request: i.Request, UTC: i.UTC, Pack: i.Pack, OperationID: "op", ServiceIncarnation: "svc", Writer: storage.WriterFence{LeaseID: "writer", ServiceIncarnation: "svc", Epoch: 7}, Binding: Valid, Authority: Valid, Precision: time.Nanosecond}
}

func TestProtectionMaxAndBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name   string
		offset time.Duration
		absent bool
		extend bool
	}{
		{"no old", 0, true, true}, {"minus 1ns", -1, false, true}, {"equal", 0, false, false}, {"plus 1ns", 1, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i := protectionFixture(t)
			required := i.UTC.Upper.Add(time.Minute).Add(15 * time.Minute)
			if !tc.absent {
				i.Pack.ProtectedUntil = required.Add(tc.offset)
			}
			before := i.Pack
			for n := 0; n < 20; n++ {
				d, err := EvaluateProtection(i)
				target := required
				if !tc.absent && tc.offset > 0 {
					target = i.Pack.ProtectedUntil
				}
				if err != nil || d.Verdict != Eligible || d.Reason != Ready || d.PolicyRevision != 3 || !d.Minimum.Equal(required) || !d.Target.Equal(target) || d.ExtensionRequired != tc.extend || d.Pack != i.Pack.Ref || d.StateRevision != i.Pack.StateRevision {
					t.Fatal(d, err)
				}
				if !reflect.DeepEqual(before, i.Pack) {
					t.Fatal("mutated protection")
				}
			}
		})
	}
}

func TestProtectionInvalidEvidence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*ProtectionInput)
		v      Verdict
		r      Reason
		err    error
	}{
		{"missing UTC", func(i *ProtectionInput) { i.UTC = UTCProof{} }, Indeterminate, MissingEvidence, nil},
		{"untrusted", func(i *ProtectionInput) { i.UTC.Trusted = false }, Indeterminate, MissingEvidence, nil},
		{"stale", func(i *ProtectionInput) { i.UTC.Horizon = i.Request.Now }, Indeterminate, MissingEvidence, nil},
		{"inverted", func(i *ProtectionInput) { i.UTC.Lower = i.UTC.Upper.Add(1) }, Indeterminate, InvalidEvidence, ErrEvidence},
		{"zero lower", func(i *ProtectionInput) { i.UTC.Lower = time.Time{} }, Indeterminate, InvalidEvidence, ErrEvidence},
		{"precision missing", func(i *ProtectionInput) { i.Precision = 0 }, Indeterminate, MissingEvidence, nil},
		{"precision negative", func(i *ProtectionInput) { i.Precision = -1 }, Indeterminate, InvalidEvidence, ErrEvidence},
		{"deadline equality", func(i *ProtectionInput) { i.Request.Now = i.Request.Deadline }, Denied, Expired, nil},
		{"cancelled", func(i *ProtectionInput) { i.Request.Cancelled = true }, Denied, Cancelled, nil},
		{"binding missing", func(i *ProtectionInput) { i.Binding = Unknown }, Indeterminate, MissingEvidence, nil},
		{"binding invalid", func(i *ProtectionInput) { i.Binding = Invalid }, Denied, NotPermitted, nil},
		{"authority unknown", func(i *ProtectionInput) { i.Authority = Unknown }, Indeterminate, MissingEvidence, nil},
		{"authority lost", func(i *ProtectionInput) { i.Authority = Invalid }, Denied, NotPermitted, nil},
		{"bad fact", func(i *ProtectionInput) { i.Authority = Fact(20) }, Indeterminate, InvalidEvidence, ErrEvidence},
		{"no op", func(i *ProtectionInput) { i.OperationID = "" }, Indeterminate, InvalidEvidence, ErrEvidence},
		{"incarnation mismatch", func(i *ProtectionInput) { i.Writer.ServiceIncarnation = "old" }, Indeterminate, InvalidEvidence, ErrEvidence},
		{"missing fence", func(i *ProtectionInput) { i.Writer = storage.WriterFence{} }, Indeterminate, InvalidEvidence, ErrEvidence},
		{"missing policy", func(i *ProtectionInput) { i.Policy.RequiredRevision = 4 }, Indeterminate, RevisionMismatch, nil},
		{"invalid old", func(i *ProtectionInput) { i.Pack.ProtectedUntil = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC) }, Indeterminate, InvalidEvidence, ErrEvidence},
		{"arithmetic overflow", func(i *ProtectionInput) { i.UTC.Upper = time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC) }, Indeterminate, InvalidEvidence, ErrEvidence},
		{"duration overflow", func(i *ProtectionInput) {
			i.Request.Now, _ = i.Request.Now.Add(-1)
			i.Request.Deadline, _ = i.Request.Deadline.Add(time.Duration(math.MaxInt64) - time.Minute)
			i.UTC.Horizon = i.Request.Deadline
		}, Indeterminate, InvalidEvidence, clock.ErrOverflow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i := protectionFixture(t)
			tc.change(&i)
			d, e := EvaluateProtection(i)
			if d.Verdict != tc.v || d.Reason != tc.r || !errors.Is(e, tc.err) || !d.Minimum.IsZero() || !d.Target.IsZero() || d.ExtensionRequired {
				t.Fatal(d, e)
			}
		})
	}
}

func TestProtectionUTCAndPrecision(t *testing.T) {
	i := protectionFixture(t)
	i.UTC.Lower = i.UTC.Lower.Add(-time.Second)
	i.UTC.Upper = i.UTC.Upper.Add(time.Second + 1)
	i.Precision = time.Second
	d, e := EvaluateProtection(i)
	want := i.UTC.Upper.Add(time.Minute + 15*time.Minute).Truncate(time.Second).Add(time.Second)
	if e != nil || !d.Target.Equal(want) || !d.Minimum.Equal(want) {
		t.Fatal(d, e)
	}
	// A longer old value off the backend lattice also rounds UP, never down.
	i.Pack.ProtectedUntil = want.Add(time.Second + 1)
	d, e = EvaluateProtection(i)
	if e != nil || !d.Target.Equal(want.Add(2*time.Second)) || !d.ExtensionRequired {
		t.Fatal(d, e)
	}
	// Remaining budget is measured from the supplied monotonic Now, not admission.
	i = protectionFixture(t)
	i.Request.Now, _ = i.Request.Now.Add(10 * time.Second)
	d, e = EvaluateProtection(i)
	if e != nil || !d.Minimum.Equal(i.UTC.Upper.Add(50*time.Second+15*time.Minute)) {
		t.Fatal(d, e)
	}
	// Last supported UTC instant is valid; rounding beyond year 9999 rejects.
	last := time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC)
	i = protectionFixture(t)
	i.UTC.Upper = last.Add(-16 * time.Minute)
	i.UTC.Lower = i.UTC.Upper
	d, e = EvaluateProtection(i)
	if e != nil || !d.Minimum.Equal(last) {
		t.Fatal(d, e)
	}
	i.Precision = time.Second
	d, e = EvaluateProtection(i)
	if !errors.Is(e, ErrEvidence) || d.Verdict != Indeterminate || !d.Target.IsZero() {
		t.Fatal(d, e)
	}
	for _, duration := range []time.Duration{0, -1} {
		if _, e := roundProtectionUp(last, duration); !errors.Is(e, ErrEvidence) {
			t.Fatal(e)
		}
	}
}

func TestProtectionDomainsAndRoots(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*ProtectionInput)
		want   Verdict
		reason Reason
		err    error
	}{
		{"writer without recovery", func(*ProtectionInput) {}, Eligible, Ready, nil},
		{"recovery without writer", func(i *ProtectionInput) { i.Writer = storage.WriterFence{} }, Indeterminate, InvalidEvidence, ErrEvidence},
		{"writer invalid recovery", func(i *ProtectionInput) { i.Recovery.Binding = Invalid }, Denied, NotPermitted, nil},
		{"recovery invalid writer", func(i *ProtectionInput) { i.Authority = Invalid }, Denied, NotPermitted, nil},
		{"writer invalid handle", func(i *ProtectionInput) { i.Writer.Epoch = 0 }, Indeterminate, InvalidEvidence, ErrEvidence},
		{"both independently invalid", func(i *ProtectionInput) { i.Authority = Invalid; i.Recovery.Binding = Invalid }, Denied, NotPermitted, nil},
		{"recovery missing binding", func(i *ProtectionInput) { i.Recovery.Binding = Unknown }, Indeterminate, MissingEvidence, nil},
		{"recovery invalid evidence", func(i *ProtectionInput) { i.Recovery.Binding = Fact(20) }, Indeterminate, InvalidEvidence, ErrEvidence},
		{"recovery wrong incarnation", func(i *ProtectionInput) { i.Recovery.ServiceIncarnation = "old" }, Indeterminate, InvalidEvidence, ErrEvidence},
		{"recovery missing history", func(i *ProtectionInput) { i.Recovery.HistoryID = "" }, Indeterminate, InvalidEvidence, ErrEvidence},
		{"recovery zero epoch", func(i *ProtectionInput) { i.Recovery.Epoch = 0 }, Indeterminate, InvalidEvidence, ErrEvidence},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i := protectionFixture(t)
			if tc.name != "writer without recovery" {
				i.Recovery = &RecoveryProtectionContext{HistoryID: "history", Epoch: RecoveryEpoch(999), ServiceIncarnation: "svc", Binding: Valid}
			}
			tc.change(&i)
			var before RecoveryProtectionContext
			if i.Recovery != nil {
				before = *i.Recovery
			}
			inputBefore := i
			d, err := EvaluateProtection(i)
			if d.Verdict != tc.want || d.Reason != tc.reason || !errors.Is(err, tc.err) || (err != nil) != (tc.err != nil) {
				t.Fatal(d, err)
			}
			if tc.want != Eligible && (!d.Minimum.IsZero() || !d.Target.IsZero() || d.ExtensionRequired) {
				t.Fatal("partial proposal", d)
			}
			if tc.want == Eligible && (d.Minimum.IsZero() || d.Target.Before(d.Minimum) || !d.ExtensionRequired) {
				t.Fatal("missing ordinary R1 proposal", d)
			}
			if !reflect.DeepEqual(i, inputBefore) {
				t.Fatal("mutated input")
			}
			if i.Recovery != nil && *i.Recovery != before {
				t.Fatal("mutated R2")
			}
		})
	}
	i := protectionFixture(t)
	baseline, e := EvaluateProtection(i)
	if e != nil {
		t.Fatal(e)
	}
	for _, roots := range []Fact{Unknown, Valid, Invalid} {
		i.RetentionRoots = roots
		i.Pack.ProtectedUntil = i.UTC.Lower.Add(-time.Hour)
		i.Pack.MinRetentionUntil = i.UTC.Upper.Add(100 * time.Hour)
		d, e := EvaluateProtection(i)
		if e != nil || !reflect.DeepEqual(d, baseline) || i.Pack.MinRetentionUntil != i.UTC.Upper.Add(100*time.Hour) {
			t.Fatal("deadline cannot discharge retention roots", d, e)
		}
	}
	// Independent R2 epoch values never influence R1 fence or deadline arithmetic.
	i = protectionFixture(t)
	i.Recovery = &RecoveryProtectionContext{HistoryID: "h", Epoch: 1, ServiceIncarnation: "svc", Binding: Valid}
	a, e := EvaluateProtection(i)
	if e != nil {
		t.Fatal(e)
	}
	i.Recovery.Epoch = RecoveryEpoch(math.MaxUint64)
	b, e := EvaluateProtection(i)
	if e != nil || !reflect.DeepEqual(a, b) || i.Writer.Epoch != 7 {
		t.Fatal(a, b, e)
	}
	other, e := clock.NewVirtual(i.UTC.Upper, 10)
	if e != nil {
		t.Fatal(e)
	}
	for _, horizon := range []bool{false, true} {
		in := i
		if horizon {
			in.UTC.Horizon = other.Now().Instant
		} else {
			in.Request.Deadline = other.Now().Instant
		}
		d, e := EvaluateProtection(in)
		if !errors.Is(e, clock.ErrDomain) || d.Verdict != Indeterminate || !d.Target.IsZero() {
			t.Fatal(d, e)
		}
	}
}

// oracleProtectionCeil uses arbitrary-precision integer division on nanoseconds
// since year 1 UTC, independent of time.Truncate and roundProtectionUp. This
// tests the proposed lattice, not compatibility with any production backend.
func oracleProtectionCeil(t time.Time, tick time.Duration) (time.Time, bool) {
	toNS := func(t time.Time) *big.Int {
		seconds := new(big.Int).Add(big.NewInt(t.Unix()), big.NewInt(62135596800))
		return seconds.Add(seconds.Mul(seconds, big.NewInt(1e9)), big.NewInt(int64(t.Nanosecond())))
	}
	q, r := new(big.Int), new(big.Int)
	q.QuoRem(toNS(t), big.NewInt(int64(tick)), r)
	if r.Sign() != 0 {
		q.Add(q, big.NewInt(1))
	}
	ns := q.Mul(q, big.NewInt(int64(tick)))
	last := time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC)
	if ns.Cmp(toNS(last)) > 0 {
		return time.Time{}, false
	}
	seconds, nanos := new(big.Int), new(big.Int)
	seconds.QuoRem(ns, big.NewInt(1e9), nanos)
	seconds.Sub(seconds, big.NewInt(62135596800))
	return time.Unix(seconds.Int64(), nanos.Int64()).UTC(), true
}

func TestProtectionPrecisionLatticeRegression(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		tick      time.Duration
	}{
		{"7ns", "2026-10-21T00:00:00.123456789Z", 7},
		{"1.5s", "2026-10-21T00:00:00.123456789Z", 1500 * time.Millisecond},
		{"17m", "2026-10-21T00:00:00.123456789Z", 17 * time.Minute},
		{"large valid", "1900-01-01T00:00:00.000000001Z", time.Duration(math.MaxInt64)},
		{"pre Unix epoch", "1969-12-31T23:59:59.999999999Z", 7},
		{"exact lattice", "2026-10-21T00:00:00Z", 1500 * time.Millisecond},
		{"off lattice 1ns", "2026-10-21T00:00:00.000000001Z", 1500 * time.Millisecond},
		{"UTC normalization", "2026-10-21T07:00:00.000000001+07:00", 1500 * time.Millisecond},
		{"last nanosecond", "9999-12-31T23:59:59.999999999Z", time.Nanosecond},
		{"round overflow 7ns", "9999-12-31T23:59:59.999999999Z", 7},
		{"round overflow second", "9999-12-31T23:59:59.999999999Z", time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := time.Parse(time.RFC3339Nano, tc.raw)
			if err != nil {
				t.Fatal(err)
			}
			minimum, valid := oracleProtectionCeil(raw, tc.tick)
			for _, oldOffset := range []time.Duration{-1, 0, 1} {
				i := protectionFixture(t)
				i.Precision = tc.tick
				i.UTC.Upper = raw.Add(-16 * time.Minute)
				i.UTC.Lower = i.UTC.Upper
				i.Pack.ProtectedUntil = raw.Add(oldOffset)
				expectedTarget, validTarget := oracleProtectionCeil(i.Pack.ProtectedUntil, tc.tick)
				if valid && validTarget && minimum.After(expectedTarget) {
					expectedTarget = minimum
				}
				// Snapshot includes all value fields; no recovery pointer in this fixture.
				before := i
				var first ProtectionDecision
				for n := 0; n < 5; n++ {
					got, e := EvaluateProtection(i)
					if !valid || !validTarget || i.Pack.ProtectedUntil.UTC().Year() > 9999 {
						if !errors.Is(e, ErrEvidence) || got.Verdict != Indeterminate || got.Reason != InvalidEvidence || !got.Minimum.IsZero() || !got.Target.IsZero() || got.ExtensionRequired {
							t.Fatal("overflow did not fail closed", got, e)
						}
					} else {
						if e != nil || got.Verdict != Eligible || got.Reason != Ready || !got.Minimum.Equal(minimum) || !got.Target.Equal(expectedTarget) || got.Minimum.Before(raw) || got.Target.Before(got.Minimum) || got.Target.Before(i.Pack.ProtectedUntil) || got.Minimum.Location() != time.UTC || got.Target.Location() != time.UTC || got.ExtensionRequired != i.Pack.ProtectedUntil.Before(expectedTarget) {
							t.Fatal(got, e, minimum, expectedTarget)
						}
					}
					if n == 0 {
						first = got
					} else if !reflect.DeepEqual(first, got) {
						t.Fatal("nondeterministic", first, got)
					}
					if !reflect.DeepEqual(i, before) {
						t.Fatal("input mutated")
					}
				}
			}
		})
	}
}
