package recovery

import (
	"context"
	"encoding/json"
	"strconv"

	"github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/jobs"
	s "github.com/KinasTomes/miles/internal/storage"
)

type CheckpointStore interface {
	s.RecoveryStore
	s.RecoveryCatalogStore
}
type CheckpointWorker struct {
	Store   CheckpointStore
	Objects ImmutableStore
	Packs   s.PackStore
	Catalog Catalog
}
type checkpointParams struct {
	Target *uint64 `json:"target_epoch"`
}

func parseCheckpointParams(raw json.RawMessage) (checkpointParams, error) {
	var p checkpointParams
	if strictJSON(raw, &p) != nil || p.Target == nil || *p.Target >= maxInteger {
		return p, s.ErrInvalid
	}
	return p, nil
}
func (w CheckpointWorker) Executor() jobs.Executor {
	return jobs.Executor{Kind: "recovery", Validate: func(raw json.RawMessage) error { _, e := parseCheckpointParams(raw); return e }, Step: w.Step}
}
func (w CheckpointWorker) Step(ctx context.Context, j jobs.Job, _ config.GlobalSnapshot) (jobs.Step, error) {
	if j.State == jobs.Cancelling {
		return jobs.Step{Settled: true, Done: true}, nil
	}
	p, e := parseCheckpointParams(j.Params)
	if e != nil {
		return jobs.Step{}, e
	}
	if e = w.Store.CheckRecoveryWorker(ctx, j.Worker); e != nil {
		return jobs.Step{}, e
	}
	objects := guardedObjects{w.Objects, func(ctx context.Context) error { return w.Store.CheckRecoveryWorker(ctx, j.Worker) }}
	r := Retention{Checkpointer: Checkpointer{Objects: objects, Catalog: w.Catalog}, Store: w.Store, Packs: w.Packs, Worker: j.Worker}
	_, catalog, e := r.Read(ctx)
	if e != nil {
		return jobs.Step{}, e
	}
	base := AttachmentReference{Key: genesisKey(w.Catalog), Reference: w.Catalog.Genesis}
	for _, cut := range catalog.Cuts {
		if cut.Epoch <= *p.Target {
			base = cut.Checkpoint
		}
	}
	if j.DryRun {
		_, e = VerifyCut(ctx, objects, w.Catalog, *p.Target)
		if e != nil {
			return jobs.Step{}, e
		}
		return jobs.Step{Settled: true, Done: true, Result: json.RawMessage(`{"verified_chain":true,"published":false}`)}, nil
	}
	root, e := r.Build(ctx, base, *p.Target)
	if e != nil {
		return jobs.Step{}, e
	}
	if e = r.Publish(ctx, root); e != nil {
		return jobs.Step{}, e
	}
	result, _ := json.Marshal(map[string]string{"checkpoint_sha256": root.Reference.SHA256, "target_epoch": strconv.FormatUint(*p.Target, 10)})
	return jobs.Step{Settled: true, Done: true, Result: result}, nil
}
