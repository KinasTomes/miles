package contracttest

import (
	"reflect"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/clock"
	i "github.com/KinasTomes/miles/internal/config/integration"
	"github.com/KinasTomes/miles/internal/policy"
	s "github.com/KinasTomes/miles/internal/storage"
)

// Invoked with a complete snapshot actually loaded through EACH backend. Real
// and Virtual have different domains and identical injected source evidence;
// this proves evaluator parity, not OS/chrony attestation or a full simulator.
func checkPersistedEligibility(t *testing.T, context i.Context, wall time.Time) {
	t.Helper()
	virtual, _ := clock.NewVirtual(wall, 32)
	var results [][]policy.Decision
	for _, c := range []clock.Clock{clock.NewReal(), virtual} {
		r := policy.Request{Now: c.Now().Instant}
		r.Deadline, _ = r.Now.Add(time.Minute)
		source := policy.UTCSource{ID: "model", Provenance: "fixture-bound", RestartID: "run", Verified: policy.Valid}
		u, err := policy.ValidateUTCSample(source, policy.UTCSample{SourceID: source.ID, Provenance: source.Provenance, RestartID: source.RestartID, Observed: r.Now, ValidThrough: r.Deadline, Lower: wall, Upper: wall, Precision: policy.DurationBound{Known: true}, MeasurementError: policy.DurationBound{Known: true}, NodeSkew: policy.DurationBound{Known: true}, Drift: policy.DriftBound{Known: true}, Sync: policy.Valid}, r)
		if err != nil {
			t.Fatal(err)
		}
		var decisions []policy.Decision
		pack := s.PackInfo{Version: 1, Ref: s.PackRef{ID: "pack", Generation: "gen"}, State: "Warm", Class: s.Standard, Size: 100, StateRevision: 1, ProtectedUntil: wall, LastAccess: wall.Add(-30 * 24 * time.Hour)}
		tier, err := context.EvaluateTiering(policy.TieringInput{Request: r, UTC: u, Pack: pack, LiveBytes: 80, TotalBytes: 100, Metadata: policy.Valid, Authority: policy.Valid, SafeToTransition: policy.Valid})
		if err != nil || tier.Verdict != policy.Eligible || tier.Destination != s.GlacierIR {
			t.Fatal("persisted tiering", tier, err)
		}
		decisions = append(decisions, tier.Decision)
		for _, tc := range []struct {
			state string
			class s.StorageClass
			read  policy.Verdict
			write policy.ActionKind
		}{
			{"Warm", s.Standard, policy.Eligible, policy.Reuse},
			{"Cold", s.GlacierIR, policy.Eligible, policy.Reuse},
			{"Cold", s.Glacier, policy.Denied, policy.ReviveFromBody},
			{"Restored", s.Glacier, policy.Eligible, policy.ReviveFromBody},
		} {
			pack.State, pack.Class = tc.state, tc.class
			pack.RestoredUntil = wall.Add(3 * time.Minute)
			for _, op := range []policy.WriteOperation{policy.PUT, policy.UploadPart} {
				d, e := context.EvaluateWrite(policy.WriteInput{Request: r, Operation: op, Authority: policy.Valid, Metadata: policy.Valid, Packs: []s.PackInfo{pack}, ReusePins: policy.Valid})
				if e != nil || d.Verdict != policy.Eligible || len(d.Actions) != 1 || d.Actions[0].Kind != tc.write {
					t.Fatal("write eligibility", tc, op, d, e)
				}
				decisions = append(decisions, d)
			}
			// EvaluateRead is shared by GET and payload Copy; neither has a cache
			// escape hatch. Authority/pins/restore evidence remain caller obligations.
			d, e := context.EvaluateRead(policy.ReadInput{Request: r, Pack: pack, Facts: policy.ReadFacts{Logical: policy.Valid, Metadata: policy.Valid, Authority: policy.Valid, PinCoverage: policy.Valid, Physical: policy.Valid}, UTC: u, RestoreVerified: policy.Valid})
			if e != nil || d.Verdict != tc.read {
				t.Fatal("read eligibility", tc, d, e)
			}
			decisions = append(decisions, d)
		}
		for _, delta := range []time.Duration{-1, 0, 1} {
			pack.State = "Restored"
			pack.Class = s.Glacier
			pack.RestoredUntil = wall.Add(3*time.Minute + delta)
			d, e := context.EvaluateRead(policy.ReadInput{Request: r, Pack: pack, Facts: policy.ReadFacts{Logical: policy.Valid, Metadata: policy.Valid, Authority: policy.Valid, PinCoverage: policy.Valid, Physical: policy.Valid}, UTC: u, RestoreVerified: policy.Valid})
			want := policy.Eligible
			if delta < 0 {
				want = policy.Denied
			}
			if e != nil || d.Verdict != want {
				t.Fatal("restore deadline +120s", delta, d, e)
			}
			decisions = append(decisions, d)
		}
		results = append(results, decisions)
	}
	if !reflect.DeepEqual(results[0], results[1]) {
		t.Fatal("backend Real/Virtual evaluator mismatch", results)
	}
}
