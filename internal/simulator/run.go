package simulator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math/big"
	"sort"
	"time"

	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/cost"
	"github.com/KinasTomes/miles/internal/policy"
	"github.com/KinasTomes/miles/internal/pricing"
	"github.com/KinasTomes/miles/internal/storage"
)

type Decision struct {
	Event int       `json:"event"`
	At    time.Time `json:"at"`
	policy.TieringDecision
}

type Result struct {
	SchemaVersion       int                   `json:"schema_version"`
	MVPThresholdApplied bool                  `json:"mvp_threshold_applied"`
	Kind                string                `json:"kind"`
	TraceName           string                `json:"trace_name"`
	TraceKind           string                `json:"trace_kind"`
	Metadata            string                `json:"declared_metadata_backend"`
	Profile             string                `json:"profile"`
	Topology            string                `json:"topology"`
	Start               time.Time             `json:"start"`
	End                 time.Time             `json:"end"`
	BytesPerGB          uint64                `json:"bytes_per_gb"`
	MonthSeconds        int64                 `json:"storage_month_seconds"`
	PolicyRevision      uint64                `json:"policy_revision"`
	PolicySHA256        string                `json:"policy_sha256"`
	Snapshot            *cost.Snapshot        `json:"final_snapshot"`
	Ratios              *cost.Ratios          `json:"final_ratios"`
	Counters            cost.WriteCounters    `json:"write_counters"`
	Traffic             map[cost.Route]uint64 `json:"traffic_bytes"`
	Decisions           []Decision            `json:"tiering_decisions"`
	Cost                cost.Report           `json:"cost"`
	Omissions           []string              `json:"omissions"`
	Limitations         []string              `json:"limitations"`
}

// Run serializes equal-time events in trace order. No goroutines, wall timers,
// AWS calls or implicit lifecycle mutations are used. A policy decision alone
// never changes storage usage: the next explicit observation must account for it.
func Run(ctx context.Context, trace Trace, catalog *pricing.Catalog) (Result, error) {
	var out Result
	if ctx == nil || catalog == nil || trace.Region != catalog.Region {
		return out, errors.New("trace/catalog Region mismatch or missing context")
	}
	pinned, err := trace.snapshot()
	if err != nil {
		return out, err
	}
	clk, err := clock.NewVirtual(trace.Start, MaxEvents)
	if err != nil {
		return out, err
	}
	sum := sha256.Sum256(trace.Policy)
	out = Result{SchemaVersion: 1, Kind: "simulated-accounting-prototype", TraceName: trace.Name, TraceKind: trace.Kind, Metadata: trace.Metadata, Profile: trace.Profile, Topology: trace.Topology, Start: trace.Start.UTC(), BytesPerGB: trace.BytesPerGB, MonthSeconds: trace.MonthSeconds, PolicyRevision: pinned.Revision(), PolicySHA256: hex.EncodeToString(sum[:]), Traffic: map[cost.Route]uint64{}, Decisions: []Decision{}, Omissions: append([]string{}, trace.Omissions...), Limitations: []string{
		"Modelled supplied observations, not AWS measurements, a bill, execution permission or final MVP acceptance.",
		"Storage byte-time requires supplied billable inventory; minimum duration/size, archive metadata and temporary restored storage must be supplied or listed as omissions.",
		"Pricing tiers aggregate this trace's supplied usage from zero, not account-wide billing usage or credits/Free Tier.",
		"Tiering decisions use the shared evaluator and explicitly assumed evidence; physical transitions, four-policy comparisons and full cost coverage belong to W6.",
	}}
	usage := []cost.Usage{}
	storageBytes := map[storage.StorageClass]uint64{}
	storageTotals := map[string]*big.Rat{}
	var previous int64
	step := func(to int64) error {
		d := time.Duration(to - previous)
		for class, n := range storageBytes {
			u, e := cost.StorageUsage(class, n, d, trace.BytesPerGB, time.Duration(trace.MonthSeconds)*time.Second)
			if e != nil {
				return e
			}
			q, _ := new(big.Rat).SetString(u.Quantity)
			if storageTotals[u.RateID] == nil {
				storageTotals[u.RateID] = new(big.Rat)
			}
			storageTotals[u.RateID].Add(storageTotals[u.RateID], q)
		}
		if e := clk.Advance(d); e != nil {
			return e
		}
		previous = to
		return nil
	}
	for index, event := range trace.Events {
		if err = ctx.Err(); err != nil {
			return Result{}, err
		}
		if err = step(event.AtNS); err != nil {
			return Result{}, err
		}
		if event.Snapshot != nil {
			if err = event.Snapshot.Validate(); err != nil {
				return Result{}, err
			}
			copy := *event.Snapshot
			out.Snapshot = &copy
			ratios := copy.Ratios()
			out.Ratios = &ratios
		}
		if err = out.Counters.Add(event.Counters); err != nil {
			return Result{}, err
		}
		if err = cost.AddTraffic(out.Traffic, event.Traffic); err != nil {
			return Result{}, err
		}
		usage = append(usage, event.Usage...)
		if event.StorageBytes != nil {
			storageBytes = event.StorageBytes
			for class := range storageBytes {
				if class.Validate() != nil {
					return Result{}, errors.New("invalid storage inventory class")
				}
			}
		}
		if event.Tiering != nil {
			now := clk.Now()
			deadline, e := now.Instant.Add(time.Minute)
			if e != nil {
				return Result{}, e
			}
			r := policy.Request{Now: now.Instant, Deadline: deadline}
			source := policy.UTCSource{ID: "trace", Provenance: "simulated", RestartID: "run", Verified: policy.Valid}
			sample := policy.UTCSample{SourceID: source.ID, Provenance: source.Provenance, RestartID: source.RestartID, Observed: now.Instant, ValidThrough: deadline, Lower: now.WallUTC, Upper: now.WallUTC, Precision: policy.DurationBound{Known: true}, MeasurementError: policy.DurationBound{Known: true}, NodeSkew: policy.DurationBound{Known: true}, Drift: policy.DriftBound{Known: true}, Sync: policy.Valid}
			utc, e := policy.ValidateUTCSample(source, sample, r)
			if e != nil {
				return Result{}, e
			}
			t := event.Tiering
			d, e := policy.EvaluateTiering(policy.TieringInput{Policy: policy.PinnedPolicy{Snapshot: pinned, RequiredRevision: pinned.Revision()}, Request: r, UTC: utc, Pack: t.Pack, LiveBytes: t.LiveBytes, TotalBytes: t.TotalBytes, Metadata: t.Metadata, Authority: t.Authority, SafeToTransition: t.SafeToTransition})
			if e != nil {
				return Result{}, e
			}
			out.Decisions = append(out.Decisions, Decision{index, now.WallUTC, d})
		}
	}
	if err = ctx.Err(); err != nil {
		return Result{}, err
	}
	if err = step(trace.DurationNS); err != nil {
		return Result{}, err
	}
	ids := make([]string, 0, len(storageTotals))
	for id := range storageTotals {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		for _, u := range usage {
			if u.RateID == id {
				return Result{}, errors.New("storage supplied as both byte-time inventory and usage")
			}
		}
		usage = append(usage, cost.Usage{RateID: id, Unit: "GB-Mo", Quantity: storageTotals[id].RatString()})
	}
	out.Cost, err = cost.Evaluate(catalog, usage)
	if err != nil {
		return Result{}, err
	}
	out.End = clk.Now().WallUTC
	return out, nil
}
