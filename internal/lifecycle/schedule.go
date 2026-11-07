package lifecycle

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/jobs"
	s "github.com/KinasTomes/miles/internal/storage"
)

func scanTiering(raw json.RawMessage) bool { return bytes.Equal(bytes.TrimSpace(raw), []byte(`{}`)) }

type tieringCheckpoint struct {
	Cursor           string `json:"cursor"`
	Examined, Queued uint64
}

// scheduleStep only queues exact-generation work. Observational discovery never
// authorizes COPY; each child revalidates policy under native R1 maintenance.
// Bounded pages and stable child IDs make replay after lost ACK safe.
func (w *Worker) scheduleStep(ctx context.Context, j jobs.Job, snapshot config.GlobalSnapshot) (jobs.Step, error) {
	if w.Jobs == nil || w.Jobs.Store == nil || snapshot.Revision() != j.PolicyRevision {
		return jobs.Step{}, s.ErrInvalid
	}
	var checkpoint tieringCheckpoint
	if len(j.Checkpoint) != 0 && json.Unmarshal(j.Checkpoint, &checkpoint) != nil {
		return jobs.Step{}, s.ErrInvalid
	}
	result := func(done bool) (jobs.Step, error) {
		b, e := json.Marshal(checkpoint)
		return jobs.Step{Settled: true, Done: done, Checkpoint: b, Result: b, Progress: map[string]uint64{"examined": checkpoint.Examined, "queued": checkpoint.Queued}}, e
	}
	if j.State == jobs.Cancelling || !snapshot.Policy().Tiering.Enabled {
		return result(true)
	}
	page, err := w.Store.ObservePage(ctx, s.ObservationRequest{Kind: "packs", Limit: 32, Cursor: checkpoint.Cursor})
	if err != nil {
		return jobs.Step{Settled: true}, err
	}
	if page.Next != "" && page.Next == checkpoint.Cursor {
		return jobs.Step{Settled: true}, s.ErrUnavailable
	}
	_, live, err := w.Catalog.Collect(ctx)
	if err != nil {
		return jobs.Step{Settled: true}, err
	}
	active := map[s.PackRef]bool{}
	cursor := ""
	for pages := 0; ; pages++ {
		if pages >= 10 {
			return jobs.Step{Settled: true}, s.ErrUnavailable
		}
		p, e := w.Jobs.Store.ListJobs(ctx, jobs.PageRequest{Limit: 100, Cursor: cursor, ActiveOnly: true})
		if e != nil {
			return jobs.Step{Settled: true}, e
		}
		for _, job := range p.Jobs {
			if job.Type != "tiering" || scanTiering(job.Params) || job.State.Terminal() {
				continue
			}
			params, e := Decode(job.Params, job.Type)
			if e != nil {
				return jobs.Step{Settled: true}, e
			}
			active[params.Ref()] = true
		}
		if p.Next == "" {
			break
		}
		if p.Next == cursor {
			return jobs.Step{Settled: true}, s.ErrUnavailable
		}
		cursor = p.Next
	}
	record, err := snapshot.StorageSnapshot()
	if err != nil {
		return jobs.Step{Settled: true}, err
	}
	for _, p := range page.Packs {
		if err := ctx.Err(); err != nil {
			return jobs.Step{Settled: true}, err
		}
		checkpoint.Examined++
		if active[p.Ref] || p.State != "Warm" {
			continue
		}
		var stored int64
		for _, c := range live[p.Ref] {
			if c.Location.Range.Length > p.Size-stored {
				return jobs.Step{Settled: true}, s.ErrInvalid
			}
			stored += c.Location.Range.Length
		}
		bounds, e := w.UTC.Bounds(ctx)
		if e != nil {
			return jobs.Step{Settled: true}, e
		}
		target, e := TieringCandidate(record, p, stored, bounds)
		if e != nil {
			return jobs.Step{Settled: true}, e
		}
		if target == "" {
			continue
		}
		identity, _ := json.Marshal(struct {
			Job string
			Ref s.PackRef
		}{j.ID, p.Ref})
		sum := sha256.Sum256(identity)
		id := "child_tiering_" + hex.EncodeToString(sum[:16])
		params, _ := json.Marshal(Params{PackID: p.Ref.ID, Generation: p.Ref.Generation})
		child, e := w.Jobs.CreateNamed(jobs.WithCreationFence(ctx, j.Worker), jobs.Create{Type: "tiering", Params: params, PolicyRevision: j.PolicyRevision, DryRun: j.DryRun}, id)
		if e != nil {
			if !errors.Is(e, s.ErrConflict) {
				return jobs.Step{}, e
			} // unknown enqueue retains the parent identity
			child, e = w.Jobs.Store.GetJob(ctx, id)
			if e != nil {
				return jobs.Step{}, e
			}
			if child.Type != "tiering" || child.PolicyRevision != j.PolicyRevision || child.DryRun != j.DryRun || !bytes.Equal(child.Params, params) {
				return jobs.Step{}, s.ErrConflict
			}
		}
		checkpoint.Queued++
		active[p.Ref] = true
	}
	checkpoint.Cursor = page.Next
	return result(page.Next == "")
}
