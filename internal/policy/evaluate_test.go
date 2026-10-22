package policy

import (
	"errors"
	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/storage"
	"math"
	"reflect"
	"testing"
	"time"
)

func fixture(t *testing.T) ReadInput {
	t.Helper()
	// Test dependencies only, not approved production caps or restore allowlist.
	v, e := config.NewPolicyValidator(config.PolicyLimits{MaxJSONBytes: 8192, MaxDecimalDigits: 128, MaxDecimalExponent: 256, MaxColdAfterDays: 1000, MaxAutoRestoreDays: 30, MaxReadaheadChunks: 64, RestoreTiers: []config.RestoreTierRule{{Class: storage.GlacierIR, Tier: "Standard"}}})
	if e != nil {
		t.Fatal(e)
	}
	s, e := v.NewGlobalSnapshot(3, config.DefaultGlobalPolicy())
	if e != nil {
		t.Fatal(e)
	}
	wall := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	c, e := clock.NewVirtual(wall, 10)
	if e != nil {
		t.Fatal(e)
	}
	now := c.Now().Instant
	deadline, e := now.Add(time.Minute)
	if e != nil {
		t.Fatal(e)
	}
	r := Request{Now: now, Deadline: deadline}
	return ReadInput{Policy: PinnedPolicy{Snapshot: s, RequiredRevision: 3}, Request: r, Pack: storage.PackInfo{Version: storage.ModelVersion, Ref: storage.PackRef{ID: "a", Generation: "g"}, Size: 100, StateRevision: 1, State: "Cold", Class: storage.GlacierIR}, Facts: ReadFacts{Valid, Valid, Valid, Valid, Valid}, UTC: testUTC(t, r, wall, wall), RestoreVerified: Valid}
}

// Nguồn modeled explicit cho tests; không thay provenance adapter production.
func testUTC(t *testing.T, r Request, lower, upper time.Time) UTCProof {
	t.Helper()
	p, e := ValidateUTCSample(UTCSource{ID: "test", Provenance: "modeled", RestartID: "run", Verified: Valid}, UTCSample{SourceID: "test", Provenance: "modeled", RestartID: "run", Observed: r.Now, ValidThrough: r.Deadline, Lower: lower, Upper: upper, Precision: DurationBound{Known: true}, MeasurementError: DurationBound{Known: true}, NodeSkew: DurationBound{Known: true}, Drift: DriftBound{Known: true}, Sync: Valid}, r)
	if e != nil {
		t.Fatal(e)
	}
	return p
}
func assertDecision(t *testing.T, in ReadInput, v Verdict, r Reason, wantErr bool) {
	t.Helper()
	d, e := EvaluateRead(in)
	if d.Verdict != v || d.Reason != r || (e != nil) != wantErr || d.Actions != nil || d.PolicyRevision != in.Policy.RequiredRevision {
		t.Fatalf("got %+v %v", d, e)
	}
}
func TestReadEvidence(t *testing.T) {
	cases := []struct {
		name   string
		change func(*ReadInput)
		v      Verdict
		r      Reason
		err    bool
	}{
		{"IR", func(*ReadInput) {}, Eligible, Ready, false},
		{"warm", func(i *ReadInput) { i.Pack.State = "Warm"; i.Pack.Class = storage.StandardIA }, Eligible, Ready, false},
		{"missing", func(i *ReadInput) { i.Facts.Metadata = Unknown }, Indeterminate, MissingEvidence, false},
		{"fenced", func(i *ReadInput) { i.Facts.Authority = Invalid }, Denied, NotPermitted, false},
		{"logical", func(i *ReadInput) { i.Facts.Logical = Invalid }, Denied, NotPermitted, false},
		{"physical", func(i *ReadInput) { i.Facts.Physical = Invalid }, Denied, NotPermitted, false},
		{"no pin", func(i *ReadInput) { i.Facts.PinCoverage = Unknown }, Indeterminate, MissingEvidence, false},
		{"bad fact", func(i *ReadInput) { i.Facts.Authority = Fact(9) }, Indeterminate, InvalidEvidence, true},
		{"contradiction", func(i *ReadInput) { i.Pack.State = "Warm" }, Indeterminate, InvalidEvidence, true},
		{"missing history", func(i *ReadInput) { i.Policy.Snapshot = config.GlobalSnapshot{} }, Indeterminate, MissingPolicy, false},
		{"wrong history", func(i *ReadInput) { i.Policy.RequiredRevision = 2 }, Indeterminate, RevisionMismatch, false},
		{"zero selector", func(i *ReadInput) { i.Policy.RequiredRevision = 0 }, Indeterminate, InvalidEvidence, true},
		{"revision overflow", func(i *ReadInput) { i.Policy.RequiredRevision = storage.MaxPolicyRevision + 1 }, Indeterminate, InvalidEvidence, true},
		{"cancel", func(i *ReadInput) { i.Request.Cancelled = true }, Denied, Cancelled, false},
		{"deadline equal", func(i *ReadInput) { i.Request.Now = i.Request.Deadline }, Denied, Expired, false},
		{"deadline after", func(i *ReadInput) { i.Request.Now, _ = i.Request.Deadline.Add(1) }, Denied, Expired, false},
		{"bad domain", func(i *ReadInput) { i.Request.Now = clock.Instant{} }, Indeterminate, InvalidEvidence, true},
		{"bad version", func(i *ReadInput) { i.Pack.Version = 0 }, Indeterminate, InvalidEvidence, true},
		{"bad identity", func(i *ReadInput) { i.Pack.Ref.Generation = "" }, Indeterminate, InvalidEvidence, true},
		{"archive", func(i *ReadInput) { i.Pack.Class = storage.Glacier }, Denied, RestoreRequired, false},
		{"restoring", func(i *ReadInput) { i.Pack.Class = storage.Glacier; i.Pack.State = "Restoring" }, Denied, RestoreRequired, false},
		{"retiring", func(i *ReadInput) { i.Pack.State = "Retiring" }, Denied, Unavailable, false},
		{"unknown state", func(i *ReadInput) { i.Pack.State = "other" }, Indeterminate, InvalidEvidence, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { i := fixture(t); tc.change(&i); assertDecision(t, i, tc.v, tc.r, tc.err) })
	}
}
func restored(t *testing.T) ReadInput {
	i := fixture(t)
	i.Pack.Class = storage.Glacier
	i.Pack.State = "Restored"
	i.Pack.RestoredUntil = i.UTC.Upper.Add(3 * time.Minute)
	return i
}
func TestRestoreBoundaries(t *testing.T) {
	for _, delta := range []time.Duration{-1, 0, 1, 120*time.Second - 1, 120 * time.Second, 120*time.Second + 1} {
		t.Run(delta.String(), func(t *testing.T) {
			i := restored(t)
			D := i.UTC.Upper.Add(time.Minute)
			i.Pack.RestoredUntil = D.Add(delta)
			v, r := Denied, RestoreRequired
			if delta >= 120*time.Second {
				v, r = Eligible, Ready
			}
			assertDecision(t, i, v, r, false)
		})
	}
}
func TestRestoreProofFailures(t *testing.T) {
	cases := []struct {
		name   string
		change func(*ReadInput)
		r      Reason
		err    bool
	}{
		{"untrusted", func(i *ReadInput) { i.UTC.Trusted = false }, MissingEvidence, false},
		{"missing HEAD", func(i *ReadInput) { i.RestoreVerified = Unknown }, MissingEvidence, false},
		{"stale horizon", func(i *ReadInput) { i.UTC.Horizon = i.Request.Now }, MissingEvidence, false},
		{"bad horizon", func(i *ReadInput) { i.UTC.Horizon = clock.Instant{} }, InvalidEvidence, true},
		{"inverted", func(i *ReadInput) { i.UTC.Lower = i.UTC.Upper.Add(1) }, InvalidEvidence, true},
		{"zero UTC", func(i *ReadInput) { i.UTC.Upper = time.Time{} }, InvalidEvidence, true},
		{"zero expiry", func(i *ReadInput) { i.Pack.RestoredUntil = time.Time{} }, InvalidEvidence, true},
		{"overflow UTC", func(i *ReadInput) { i.UTC.Upper = time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC) }, InvalidEvidence, true},
		{"overflow duration", func(i *ReadInput) {
			i.Request.Now, _ = i.Request.Now.Add(-1)
			i.Request.Deadline, _ = i.UTC.Horizon.Add(time.Duration(math.MaxInt64) - time.Minute)
			i.UTC.Horizon = i.Request.Deadline
		}, InvalidEvidence, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { i := restored(t); tc.change(&i); assertDecision(t, i, Indeterminate, tc.r, tc.err) })
	}
	// Point proof passes equality; uncertainty across the boundary does not prove
	// backend expiry, but it does prevent admission until extension/revalidation.
	i := restored(t)
	i.UTC.Lower = i.UTC.Lower.Add(-time.Second)
	i.UTC.Upper = i.UTC.Upper.Add(time.Second)
	i.UTC = testUTC(t, i.Request, i.UTC.Lower, i.UTC.Upper)
	assertDecision(t, i, Denied, RestoreRequired, false)
}
func writeFixture(t *testing.T) WriteInput {
	i := fixture(t)
	return WriteInput{Policy: i.Policy, Request: i.Request, Operation: PUT, Authority: Valid, Metadata: Valid, ReusePins: Valid, Packs: []storage.PackInfo{i.Pack}}
}
func TestReviveAllAndPermutation(t *testing.T) {
	for _, op := range []WriteOperation{PUT, UploadPart} {
		i := writeFixture(t)
		i.Operation = op
		a := restored(t).Pack
		b := a
		b.Ref.ID = "b"
		b.RestoredUntil = b.RestoredUntil.Add(100 * time.Hour)
		ir := i.Packs[0]
		ir.Ref.ID = "ir"
		warm := ir
		warm.Ref.ID = "warm"
		warm.State = "Warm"
		warm.Class = storage.Standard
		cold := a
		cold.Ref.ID = "cold"
		cold.State = "Cold"
		i.Packs = []storage.PackInfo{a, b, ir, warm, cold, a}
		before := append([]storage.PackInfo(nil), i.Packs...)
		d, e := EvaluateWrite(i)
		if e != nil || d.Verdict != Eligible || len(d.Actions) != 5 {
			t.Fatalf("%+v %v", d, e)
		}
		for _, act := range d.Actions {
			want := ReviveFromBody
			if act.Pack.ID == "ir" || act.Pack.ID == "warm" {
				want = Reuse
			}
			if act.Kind != want {
				t.Fatal(act)
			}
		}
		if !reflect.DeepEqual(i.Packs, before) {
			t.Fatal("input mutated")
		}
		for n := 0; n < 30; n++ {
			again, e := EvaluateWrite(i)
			if e != nil || !reflect.DeepEqual(d, again) {
				t.Fatal("nondeterministic")
			}
			i.Packs = append(i.Packs[1:], i.Packs[0])
		}
	}
}
func TestWriteFailuresAndStates(t *testing.T) {
	for _, state := range []string{"Restored", "Restoring", "Uploading", "Transitioning", "Compacting", "Retiring", "Retired", "Deleting", "Deleted"} {
		i := writeFixture(t)
		p := restored(t).Pack
		p.State = state
		i.Packs = []storage.PackInfo{p}
		d, e := EvaluateWrite(i)
		if e != nil || d.Verdict != Eligible || len(d.Actions) != 1 || d.Actions[0].Kind != ReviveFromBody {
			t.Fatalf("%s %+v %v", state, d, e)
		}
	}
	for _, change := range []func(*WriteInput){
		func(i *WriteInput) { i.Operation = "Copy" }, func(i *WriteInput) { i.Packs[0].Version = 0 }, func(i *WriteInput) { i.Packs[0].State = "??" }, func(i *WriteInput) { i.Packs[0].State = "Warm" }, func(i *WriteInput) { i.ReusePins = Fact(99) },
		func(i *WriteInput) { p := i.Packs[0]; p.StateRevision++; i.Packs = append(i.Packs, p) },
	} {
		i := writeFixture(t)
		change(&i)
		d, e := EvaluateWrite(i)
		if !errors.Is(e, ErrEvidence) || d.Actions != nil {
			t.Fatalf("%+v %v", d, e)
		}
	}
	i := writeFixture(t)
	i.ReusePins = Unknown
	d, e := EvaluateWrite(i)
	if e != nil || d.Reason != MissingEvidence || d.Actions != nil {
		t.Fatal(d, e)
	}
	i.ReusePins = Invalid
	d, e = EvaluateWrite(i)
	if e != nil || d.Actions[0].Kind != ReviveFromBody {
		t.Fatal(d, e)
	}
	i.Authority = Invalid
	d, e = EvaluateWrite(i)
	if e != nil || d.Verdict != Denied || d.Actions != nil {
		t.Fatal(d, e)
	}
	i = writeFixture(t)
	i.Policy.RequiredRevision = 2
	d, e = EvaluateWrite(i)
	if e != nil || d.Reason != RevisionMismatch {
		t.Fatal(d, e)
	}
}
func TestRuntimeSimulatorSameEvaluator(t *testing.T) {
	i := fixture(t)
	real := clock.NewReal()
	virtual, e := clock.NewVirtual(i.UTC.Upper, 10)
	if e != nil {
		t.Fatal(e)
	}
	var first Decision
	for n, c := range []clock.Clock{real, virtual} {
		now := c.Now().Instant
		end, e := now.Add(time.Minute)
		if e != nil {
			t.Fatal(e)
		}
		i.Request = Request{Now: now, Deadline: end}
		before := i.Pack
		d, e := EvaluateRead(i)
		if e != nil || d.Verdict != Eligible {
			t.Fatal(d, e)
		}
		if n == 0 {
			first = d
		} else if !reflect.DeepEqual(first, d) {
			t.Fatal("adapter parity")
		}
		if !reflect.DeepEqual(before, i.Pack) {
			t.Fatal("pack mutated")
		}
	}
	// Wall jumps do not change elapsed request authority, even for archive input
	// with separately supplied, trusted UTC proof.
	i = restored(t)
	i.Request.Now = virtual.Now().Instant
	i.Request.Deadline, _ = i.Request.Now.Add(time.Minute)
	i.UTC.Horizon = i.Request.Deadline
	i.UTC = testUTC(t, i.Request, i.UTC.Lower, i.UTC.Upper)
	baseline, e := EvaluateRead(i)
	if e != nil {
		t.Fatal(e)
	}
	for _, jump := range []time.Duration{-time.Hour, time.Hour} {
		if e := virtual.SetWall(i.UTC.Upper.Add(jump)); e != nil {
			t.Fatal(e)
		}
		i.Request.Now = virtual.Now().Instant
		d, e := EvaluateRead(i)
		if e != nil || !reflect.DeepEqual(d, baseline) {
			t.Fatal(d, e)
		}
	}
}

func TestWriteBoundaryRegression(t *testing.T) {
	ir := writeFixture(t).Packs[0]
	warm := ir
	warm.Ref = storage.PackRef{ID: "a", Generation: "other"}
	warm.State, warm.Class = "Warm", storage.Standard
	conflict := ir
	conflict.StateRevision++
	for _, tc := range []struct {
		name    string
		packs   []storage.PackInfo
		pins    Fact
		verdict Verdict
		reason  Reason
		kinds   []ActionKind
		wantErr bool
	}{
		{"nil", nil, Unknown, Eligible, Ready, nil, false},
		{"empty", []storage.PackInfo{}, Unknown, Eligible, Ready, nil, false},
		{"generations", []storage.PackInfo{warm, ir}, Valid, Eligible, Ready, []ActionKind{Reuse, Reuse}, false},
		// The API cannot express one successful and one refused pin independently.
		// An aggregate refusal conservatively revives BOTH candidates from body;
		// it never returns a partial reuse plan or grants missing pin rights.
		{"aggregate refused", []storage.PackInfo{warm, ir}, Invalid, Eligible, Ready, []ActionKind{ReviveFromBody, ReviveFromBody}, false},
		{"aggregate unknown", []storage.PackInfo{warm, ir}, Unknown, Indeterminate, MissingEvidence, nil, false},
		{"conflict AB", []storage.PackInfo{warm, ir, conflict}, Valid, Indeterminate, InvalidEvidence, nil, true},
		{"conflict BA", []storage.PackInfo{conflict, ir, warm}, Valid, Indeterminate, InvalidEvidence, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := writeFixture(t)
			in.Packs = tc.packs
			in.ReusePins = tc.pins
			before := make([]storage.PackInfo, len(in.Packs))
			copy(before, in.Packs)
			if in.Packs == nil {
				before = nil
			}
			d, err := EvaluateWrite(in)
			if d.Verdict != tc.verdict || d.Reason != tc.reason || d.PolicyRevision != 3 || errors.Is(err, ErrEvidence) != tc.wantErr || (err != nil) != tc.wantErr {
				t.Fatalf("%+v %v", d, err)
			}
			if len(d.Actions) != len(tc.kinds) {
				t.Fatalf("phantom or missing actions: %+v", d)
			}
			if tc.verdict != Eligible && d.Actions != nil {
				t.Fatal("partial plan", d)
			}
			for n, kind := range tc.kinds {
				if d.Actions[n].Kind != kind || d.Actions[n].StateRevision != 1 {
					t.Fatal(d)
				}
			}
			if len(tc.kinds) == 2 && (d.Actions[0].Pack != ir.Ref || d.Actions[1].Pack != warm.Ref) {
				t.Fatal("generation order/dedup", d)
			}
			if !reflect.DeepEqual(in.Packs, before) {
				t.Fatal("input mutated")
			}
		})
	}
	// Exhaust all permutations, including repeated ID with distinct generations
	// and a duplicate reference, rather than only cyclic rotations.
	restoredPack := restored(t).Pack
	restoredPack.Ref.ID = "z"
	packs := []storage.PackInfo{warm, ir, restoredPack, ir}
	in := writeFixture(t)
	in.Packs = append([]storage.PackInfo(nil), packs...)
	expected, err := EvaluateWrite(in)
	if err != nil {
		t.Fatal(err)
	}
	var visit func(int)
	visit = func(start int) {
		if start == len(packs) {
			in.Packs = append([]storage.PackInfo(nil), packs...)
			before := append([]storage.PackInfo(nil), in.Packs...)
			got, err := EvaluateWrite(in)
			if err != nil || !reflect.DeepEqual(got, expected) || !reflect.DeepEqual(in.Packs, before) {
				t.Fatalf("permutation %+v: %+v %v", packs, got, err)
			}
			return
		}
		for n := start; n < len(packs); n++ {
			packs[start], packs[n] = packs[n], packs[start]
			visit(start + 1)
			packs[start], packs[n] = packs[n], packs[start]
		}
	}
	visit(0)
}

func TestRestoredClockParityRegression(t *testing.T) {
	template := restored(t)
	virtual, err := clock.NewVirtual(template.UTC.Upper, 10)
	if err != nil {
		t.Fatal(err)
	}
	real := clock.NewReal()
	// Controlled adapter observations: identical UTC bounds are explicitly
	// supplied, not derived from Real.Now's uncontrolled host wall sample.
	// Capture once per domain; evaluation never re-reads either clock.
	realNow, virtualNow := real.Now().Instant, virtual.Now().Instant
	budget := time.Minute
	for _, offset := range []time.Duration{-1, 0, 1} {
		t.Run(offset.String(), func(t *testing.T) {
			var expected Decision
			for n, now := range []clock.Instant{realNow, virtualNow} {
				in := template
				deadline, err := now.Add(budget)
				if err != nil {
					t.Fatal(err)
				}
				in.Request = Request{Now: now, Deadline: deadline}
				in.UTC.Horizon = deadline
				in.UTC.Lower = in.UTC.Upper.Add(-time.Second)
				in.UTC = testUTC(t, in.Request, in.UTC.Lower, in.UTC.Upper)
				in.Pack.RestoredUntil = in.UTC.Upper.Add(budget).Add(120 * time.Second).Add(offset)
				got, err := EvaluateRead(in)
				if err != nil {
					t.Fatal(err)
				}
				wantVerdict, wantReason := Eligible, Ready
				if offset < 0 {
					wantVerdict, wantReason = Denied, RestoreRequired
				}
				if got.Verdict != wantVerdict || got.Reason != wantReason || got.Actions != nil || got.PolicyRevision != 3 {
					t.Fatal(got)
				}
				if n == 0 {
					expected = got
				} else {
					if !reflect.DeepEqual(got, expected) {
						t.Fatal("restore parity", got, expected)
					}
					for _, jump := range []time.Duration{-time.Hour, time.Hour} {
						if err := virtual.SetWall(template.UTC.Upper.Add(jump)); err != nil {
							t.Fatal(err)
						}
						in.Request.Now = virtual.Now().Instant
						after, err := EvaluateRead(in)
						if err != nil || !reflect.DeepEqual(after, expected) {
							t.Fatal("wall jump", after, err)
						}
					}
				}
			}
		})
	}
	for _, tc := range []struct {
		name   string
		change func(*ReadInput)
	}{
		{"request deadline", func(in *ReadInput) { in.Request.Deadline, _ = virtualNow.Add(budget) }},
		{"proof horizon", func(in *ReadInput) { in.UTC.Horizon, _ = virtualNow.Add(budget) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := template
			deadline, err := realNow.Add(budget)
			if err != nil {
				t.Fatal(err)
			}
			in.Request = Request{Now: realNow, Deadline: deadline}
			in.UTC.Horizon = deadline
			in.UTC = testUTC(t, in.Request, in.UTC.Lower, in.UTC.Upper)
			tc.change(&in)
			got, err := EvaluateRead(in)
			if !errors.Is(err, clock.ErrDomain) || got.Verdict != Indeterminate || got.Reason != InvalidEvidence || got.Actions != nil {
				t.Fatal(got, err)
			}
		})
	}
}
