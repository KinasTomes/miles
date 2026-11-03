package lifecycle

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/engine/write"
	"github.com/KinasTomes/miles/internal/jobs"
	"github.com/KinasTomes/miles/internal/pack"
	"github.com/KinasTomes/miles/internal/policy"
	s "github.com/KinasTomes/miles/internal/storage"
	"io"
	"time"
)

type Store interface {
	Inventory
	write.CanonicalStore
	s.R1Store
	GetLifecycle(context.Context, string) (Action, error)
	ResolveLifecycle(context.Context, jobs.Job) (Action, error)
	PrepareLifecycle(context.Context, jobs.Job, Params, []s.Chunk, string) (Action, error)
	DispatchLifecycle(context.Context, jobs.Job, s.StorageClass, int64) (Action, error)
	ClaimLifecycleSend(context.Context, jobs.Job) (Action, bool, error)
	CheckLifecycle(context.Context, jobs.Job) error
	LifecycleProducer(context.Context, jobs.Job) (s.MutationContext, error)
	UnknownLifecycle(context.Context, jobs.Job) error
	AppliedLifecycle(context.Context, jobs.Job) error
	FinishLifecycle(context.Context, jobs.Job, s.PackInfo, bool) (Action, error)
	SkipLifecycle(context.Context, jobs.Job) (Action, error)
}
type Worker struct {
	Store   Store
	Packs   s.PackStore
	Catalog *Catalog
	UTC     s.R1TimeSource
}

func (w *Worker) Executor(kind jobs.Type) jobs.Executor {
	return jobs.Executor{ResumePending: true, Validate: func(b json.RawMessage) error { _, e := Decode(b, kind); return e }, Step: w.Step}
}
func TieringCandidate(record s.PolicySnapshot, p s.PackInfo, live int64, b s.R1Bounds) (s.StorageClass, error) {
	validator, e := config.NewPolicyValidator(config.DSGPolicyLimits())
	if e != nil {
		return "", e
	}
	snapshot, e := validator.DecodeGlobal(record)
	if e != nil {
		return "", e
	}
	// Bounds already include uncertainty from the trusted R1 time adapter. This
	// bridges that explicit port to the pure selector; it grants no authority.
	c, e := clock.NewVirtual(b.Upper, 16)
	if e != nil {
		return "", e
	}
	now := c.Now().Instant
	deadline, _ := now.Add(time.Second)
	if !b.Upper.Add(time.Second).Before(b.Horizon) {
		return "", s.ErrFenced
	}
	request := policy.Request{Now: now, Deadline: deadline}
	proof, e := policy.ValidateUTCSample(policy.UTCSource{ID: "r1-bounds", Provenance: "trusted-r1-adapter", RestartID: "selection", Verified: policy.Valid}, policy.UTCSample{SourceID: "r1-bounds", Provenance: "trusted-r1-adapter", RestartID: "selection", Observed: now, ValidThrough: deadline, Lower: b.Lower, Upper: b.Upper, Precision: policy.DurationBound{Known: true}, MeasurementError: policy.DurationBound{Known: true}, NodeSkew: policy.DurationBound{Known: true}, Drift: policy.DriftBound{Known: true}, Sync: policy.Valid}, request)
	if e != nil {
		return "", e
	}
	d, e := policy.EvaluateTiering(policy.TieringInput{Policy: policy.PinnedPolicy{Snapshot: snapshot, RequiredRevision: record.Revision}, Request: request, UTC: proof, Pack: p, LiveBytes: live, TotalBytes: p.Size, Metadata: policy.Valid, Authority: policy.Valid, SafeToTransition: policy.Valid})
	if e != nil {
		return "", e
	}
	if d.Verdict != policy.Eligible {
		return "", nil
	}
	return d.Destination, nil
}
func (w *Worker) Step(ctx context.Context, j jobs.Job, snapshot config.GlobalSnapshot) (jobs.Step, error) {
	if w == nil || w.Store == nil || w.Packs == nil || w.Catalog == nil || w.UTC == nil {
		return jobs.Step{}, s.ErrInvalid
	}
	p, e := Decode(j.Params, j.Type)
	if e != nil {
		return jobs.Step{Settled: true}, e
	}
	result := func(a Action, done bool) jobs.Step {
		report := map[string]any{"action": a.State, "pack_id": p.PackID, "generation": p.Generation, "source_retained": true}
		if a.Target != "" {
			report["storage_class"] = a.Target
		}
		if a.Kind == "pack_promote" && a.State == "Done" {
			report["destination"] = map[string]string{"pack_id": a.Destination.ID, "generation": a.Destination.Generation}
		}
		body, _ := json.Marshal(report)
		return jobs.Step{Settled: true, Done: done, ErrorCode: func() string {
			if a.State == "NotApplied" {
				return "transition_not_applied"
			}
			return ""
		}(), Checkpoint: json.RawMessage(`{"lifecycle":true}`), Result: body}
	}
	rows, e := w.Store.GetPacks(ctx, []s.PackRef{p.Ref()})
	if e != nil {
		return jobs.Step{Settled: true}, e
	}
	source, ok := rows[p.Ref()]
	if !ok {
		return jobs.Step{Settled: true}, s.ErrNotFound
	}
	if j.DryRun {
		body, _ := json.Marshal(map[string]any{"dry_run": true, "pack": source, "physical_dispatch": false})
		return jobs.Step{Settled: true, Done: true, Result: body}, nil
	}
	a, e := w.Store.GetLifecycle(ctx, j.ID)
	if e != nil && !errors.Is(e, s.ErrNotFound) {
		return jobs.Step{}, e
	}
	var data, sidecar []byte
	var entries []pack.Entry
	if errors.Is(e, s.ErrNotFound) {
		if j.State == jobs.Cancelling {
			return jobs.Step{Settled: true, Done: true}, nil
		}
		var plan []s.Chunk
		digest := ""
		if j.Type == "pack_promote" {
			if e = w.Store.PinR1(ctx, j.Worker, []s.PackRef{p.Ref()}); e != nil {
				return jobs.Step{Settled: true}, e
			}
			_, live, e := w.Catalog.Collect(ctx)
			if e != nil {
				return jobs.Step{Settled: true}, e
			}
			plan = live[p.Ref()]
			if len(plan) == 0 || len(plan) > pack.MaxEntries {
				return jobs.Step{Settled: true}, fmt.Errorf("%w: promotion plan exceeds the pack format entry bound", s.ErrInvalid)
			}
			sum := sha256.Sum256([]byte("promote/" + j.ID))
			dest := s.PackRef{ID: p.PackID, Generation: hex.EncodeToString(sum[:16])}
			data, sidecar, entries, e = w.build(ctx, dest, plan)
			if e != nil {
				return jobs.Step{Settled: true}, e
			}
			hash := sha256.Sum256(data)
			digest = hex.EncodeToString(hash[:])
		}
		a, e = w.Store.PrepareLifecycle(ctx, j, p, plan, digest)
		if e != nil {
			if errors.Is(e, s.ErrR1Busy) {
				return jobs.Step{Settled: true, Checkpoint: json.RawMessage(`{"deferred":"r1_busy_or_retained"}`)}, nil
			}
			return jobs.Step{Settled: !errors.Is(e, s.ErrUnavailable) && ctx.Err() == nil}, e
		}
	} else {
		// Only a checkpointed Waiting restore may be adopted by a newer worker.
		a, e = w.Store.PrepareLifecycle(ctx, j, p, a.Chunks, a.Digest)
		if e != nil {
			return jobs.Step{}, e
		}
	}
	if a.State == "Done" || a.State == "Skipped" || a.State == "NotApplied" {
		return result(a, true), nil
	}
	plannedChunks := a.Chunks
	if a.State == "Prepared" {
		if j.State == jobs.Cancelling {
			a, e = w.Store.SkipLifecycle(ctx, j)
			if e != nil {
				return jobs.Step{}, e
			}
			return result(a, true), nil
		}
		target := s.StorageClass("")
		liveBytes := int64(0)
		if a.Kind == "tiering" {
			reports, _, err := w.Catalog.Collect(ctx)
			if err != nil {
				return w.abortPrepared(ctx, j, err)
			}
			for _, r := range reports {
				if r.PackID == p.PackID && r.Generation == p.Generation {
					liveBytes = r.LiveBytes
				}
			}
			bounds, err := w.UTC.Bounds(ctx)
			if err != nil {
				return w.abortPrepared(ctx, j, err)
			}
			record, err := snapshot.StorageSnapshot()
			if err != nil {
				return w.abortPrepared(ctx, j, err)
			}
			target, err = TieringCandidate(record, a.Source, liveBytes, bounds)
			if err != nil {
				return w.abortPrepared(ctx, j, err)
			}
			if target == "" {
				a, err = w.Store.SkipLifecycle(ctx, j)
				if err != nil {
					return jobs.Step{}, err
				}
				return result(a, true), nil
			}
		}
		a, e = w.Store.DispatchLifecycle(ctx, j, target, liveBytes)
		if e != nil {
			return jobs.Step{}, e
		}
	}
	freshSend := false
	if a.State == "Ready" {
		a, freshSend, e = w.Store.ClaimLifecycleSend(ctx, j)
		if e != nil {
			if errors.Is(e, s.ErrFenced) {
				skipped, cleanupErr := w.Store.SkipLifecycle(ctx, j)
				if cleanupErr == nil {
					return result(skipped, true), nil
				}
			}
			return jobs.Step{}, e
		}
	}
	a.Chunks = plannedChunks
	guarded := s.WithPhysicalDispatch(ctx, func(call context.Context) error { return w.Store.CheckLifecycle(call, j) })
	switch a.Kind {
	case "tiering":
		if a.State == "Dispatched" && !freshSend {
			if e = w.Store.UnknownLifecycle(ctx, j); e != nil {
				return jobs.Step{}, e
			}
			a.State = "Unknown"
		}
		if a.State == "Unknown" {
			a, e = w.Store.ResolveLifecycle(ctx, j)
			if e != nil {
				return jobs.Step{}, e
			}
		}
		if a.State == "Dispatched" {
			e = w.Packs.ChangeStorageClass(guarded, a.Source.Ref, a.Target)
			if e != nil {
				return w.unknown(ctx, j, e)
			}
			if e = w.Store.AppliedLifecycle(ctx, j); e != nil {
				return jobs.Step{}, e
			}
		}
		physical, e := w.Packs.HeadPack(ctx, a.Source.Ref)
		if e != nil {
			return jobs.Step{}, e
		}
		a, e = w.Store.FinishLifecycle(ctx, j, physical, false)
		if e != nil {
			return jobs.Step{}, e
		}
		return result(a, true), nil
	case "pack_restore":
		if a.State == "Dispatched" && freshSend {
			e = w.Packs.Restore(guarded, a.Source.Ref, s.RestoreRequest{Days: a.Days, Tier: a.Tier})
			if e != nil {
				return w.unknown(ctx, j, e)
			}
			if e = w.Store.AppliedLifecycle(ctx, j); e != nil {
				return jobs.Step{}, e
			}
		}
		physical, e := w.Packs.HeadPack(ctx, a.Source.Ref)
		if e != nil {
			return jobs.Step{}, e
		}
		bounds, e := w.UTC.Bounds(ctx)
		if e != nil {
			return jobs.Step{}, e
		}
		ready := physical.Class == s.Glacier && bounds.Upper.Before(physical.RestoredUntil)
		if (a.State == "Unknown" || a.State == "Dispatched" && !freshSend) && !ready && physical.State != "Restoring" {
			return jobs.Step{}, jobs.ErrReconciliation
		}
		a, e = w.Store.FinishLifecycle(ctx, j, physical, !ready)
		if e != nil {
			return jobs.Step{}, e
		}
		return result(a, ready), nil
	case "pack_promote":
		if len(data) == 0 {
			data, sidecar, entries, e = w.build(ctx, a.Destination, a.Chunks)
			if e != nil {
				return jobs.Step{}, e
			}
		}
		hash := sha256.Sum256(data)
		if hex.EncodeToString(hash[:]) != a.Digest {
			return jobs.Step{}, s.ErrConflict
		}
		mutation, e := w.Store.LifecycleProducer(ctx, j)
		if e != nil {
			return jobs.Step{}, e
		}
		publish := s.WithMutation(WithJob(guarded, j), mutation)
		if e = w.promote(publish, j, a, data, sidecar, entries); e != nil {
			return w.unknown(ctx, j, e)
		}
		a, e = w.Store.FinishLifecycle(ctx, j, s.PackInfo{}, false)
		if e != nil {
			return jobs.Step{}, e
		}
		return result(a, true), nil
	}
	return jobs.Step{}, s.ErrInvalid
}
func (w *Worker) abortPrepared(ctx context.Context, j jobs.Job, cause error) (jobs.Step, error) {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_, e := w.Store.SkipLifecycle(cleanup, j)
	return jobs.Step{Settled: e == nil}, errors.Join(cause, e)
}
func (w *Worker) unknown(ctx context.Context, j jobs.Job, cause error) (jobs.Step, error) {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return jobs.Step{}, errors.Join(s.ErrUnavailable, cause, w.Store.UnknownLifecycle(cleanup, j))
}
func (w *Worker) build(ctx context.Context, dest s.PackRef, plan []s.Chunk) ([]byte, []byte, []pack.Entry, error) {
	builder, e := pack.NewBuilder(dest)
	if e != nil {
		return nil, nil, nil, e
	}
	for _, chunk := range plan {
		if chunk.Validate() != nil || chunk.RawBytes > 4<<20 || chunk.Location.Range.Validate(pack.MaxPackBytes) != nil {
			return nil, nil, nil, s.ErrInvalid
		}
		entry := pack.Entry{Key: chunk.Key, Codec: chunk.Location.Codec, Offset: uint64(chunk.Location.Range.Offset), StoredLen: uint32(chunk.Location.Range.Length), RawLength: uint32(chunk.RawBytes), CRC32C: chunk.Location.CRC32C}
		r, e := w.Packs.RangeGet(ctx, chunk.Location.Pack, chunk.Location.Range)
		if e != nil {
			return nil, nil, nil, e
		}
		stored, e := io.ReadAll(io.LimitReader(r, chunk.Location.Range.Length+1))
		closeErr := r.Close()
		if e != nil || closeErr != nil {
			return nil, nil, nil, errors.Join(e, closeErr)
		}
		if _, e = pack.ReadBlob(entry, stored); e != nil {
			return nil, nil, nil, e
		}
		if e = builder.Add(pack.Blob{Key: chunk.Key, RawLength: uint32(chunk.RawBytes), Codec: chunk.Location.Codec, Stored: stored}); e != nil {
			return nil, nil, nil, e
		}
	}
	return builder.Finish()
}
func (w *Worker) promote(ctx context.Context, j jobs.Job, a Action, data, index []byte, entries []pack.Entry) error {
	rows, e := w.Store.GetPacks(ctx, []s.PackRef{a.Destination})
	if e != nil {
		return e
	}
	dest, found := rows[a.Destination]
	if !found {
		b, e := w.UTC.Bounds(ctx)
		if e != nil {
			return e
		}
		dest = s.PackInfo{Version: 1, Ref: a.Destination, State: "Uploading", StateRevision: 1, Class: s.Standard, Size: int64(len(data)), Writer: j.Worker, ProtectedUntil: b.Upper.Add(24*time.Hour + 10*time.Minute)}
		if e = w.Store.RegisterUploadingPack(ctx, dest, j.Worker); e != nil {
			return e
		}
	}
	if dest.Writer != j.Worker || dest.Size != int64(len(data)) || dest.Class != s.Standard {
		return s.ErrConflict
	}
	if dest.State == "Uploading" {
		physical := dest
		physical.State = "Warm"
		if e = w.Packs.PutPack(ctx, physical, bytes.NewReader(data)); e != nil {
			return e
		}
		if e = w.Packs.PutSidecar(ctx, dest.Ref, int64(len(index)), bytes.NewReader(index)); e != nil {
			return e
		}
	}
	// Verify the immutable exact destination before any canonical CAS, including
	// retry after a lost PUT acknowledgement. Source generations are never deleted.
	got, e := w.Packs.HeadPack(ctx, dest.Ref)
	if e != nil {
		return e
	}
	if got.Size != dest.Size || got.Class != s.Standard {
		return s.ErrConflict
	}
	r, e := w.Packs.RangeGet(ctx, dest.Ref, s.ByteRange{Length: dest.Size})
	if e != nil {
		return e
	}
	actual, e := io.ReadAll(io.LimitReader(r, dest.Size+1))
	closeErr := r.Close()
	if e != nil || closeErr != nil {
		return errors.Join(e, closeErr)
	}
	if !bytes.Equal(actual, data) {
		return s.ErrConflict
	}
	if reader, ok := w.Packs.(interface {
		ReadSidecar(context.Context, s.PackRef) ([]byte, error)
	}); ok {
		side, e := reader.ReadSidecar(ctx, dest.Ref)
		if e != nil {
			return e
		}
		if !bytes.Equal(side, index) {
			return s.ErrConflict
		}
	} else {
		return s.ErrInvalid
	}
	if dest.State == "Uploading" {
		if _, e = w.Store.MarkPackWarm(ctx, dest.Ref, 1, j.Worker); e != nil {
			return e
		}
	} else if dest.State != "Warm" {
		return s.ErrConflict
	}
	prior := map[s.ChunkKey]s.Chunk{}
	for _, c := range a.Chunks {
		prior[c.Key] = c
	}
	for _, entry := range entries {
		if e = w.Store.CheckLifecycle(ctx, j); e != nil {
			return e
		}
		old := prior[entry.Key]
		current, e := w.Store.GetChunks(ctx, []s.ChunkKey{entry.Key}, true)
		if e != nil {
			return e
		}
		row, ok := current[entry.Key]
		if !ok {
			return s.ErrConflict
		}
		if row.Location == entry.Location(dest.Ref) {
			continue
		}
		if row != old {
			continue
		} // a concurrent canonical winner is never rebased over
		if _, e = w.Store.CompareAndSwapChunkLocation(ctx, entry.Key, old.LocationRevision, entry.Location(dest.Ref), j.Worker); e != nil {
			return e
		}
	}
	return nil
}
