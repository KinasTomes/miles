package app

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/jobs"
	"github.com/KinasTomes/miles/internal/metadata/dynamodb"
	"github.com/KinasTomes/miles/internal/object"
	"github.com/KinasTomes/miles/internal/storage"
)

type uploadCheckpoint struct {
	After     string `json:"after"`
	Pages     uint64 `json:"pages"`
	Examined  uint64 `json:"examined"`
	Completed uint64 `json:"completed"`
	Aborted   uint64 `json:"aborted"`
	Skipped   uint64 `json:"skipped"`
}

type uploadJobs struct {
	store   *dynamodb.Store
	objects func(context.Context, string) (*object.Service, error)
	utc     storage.R1TimeSource
}

func (w uploadJobs) executor(kind jobs.Type) jobs.Executor {
	return jobs.Executor{
		Periodic: time.Minute,
		Validate: func(raw json.RawMessage) error {
			var params map[string]json.RawMessage
			if json.Unmarshal(raw, &params) != nil || params == nil || len(params) != 0 {
				return storage.ErrInvalid
			}
			return nil
		},
		Step: func(ctx context.Context, j jobs.Job, _ config.GlobalSnapshot) (jobs.Step, error) {
			return w.step(ctx, j, kind)
		},
	}
}

func (w uploadJobs) step(ctx context.Context, j jobs.Job, kind jobs.Type) (jobs.Step, error) {
	var checkpoint uploadCheckpoint
	if len(j.Checkpoint) > 0 && json.Unmarshal(j.Checkpoint, &checkpoint) != nil {
		return jobs.Step{}, storage.ErrInvalid
	}
	settled := func(done bool, code string) (jobs.Step, error) {
		encoded, err := json.Marshal(checkpoint)
		return jobs.Step{Settled: true, Done: done, Checkpoint: encoded, Result: encoded, ErrorCode: code, Progress: map[string]uint64{"examined": checkpoint.Examined, "completed": checkpoint.Completed, "aborted": checkpoint.Aborted, "skipped": checkpoint.Skipped}}, err
	}
	// Each step bounds discovery and then checkpoints. CAS/receipts make replay
	// safe after a crash between a completed upload and its job checkpoint.
	for range 32 {
		current, err := w.store.GetJob(ctx, j.ID)
		if err != nil {
			return jobs.Step{}, err
		}
		if current.State == jobs.Cancelling {
			return settled(false, "")
		}
		upload, next, err := w.store.ScanUploadCandidate(ctx, checkpoint.After)
		if err != nil {
			return jobs.Step{}, err
		}
		checkpoint.Pages++
		if upload != nil {
			checkpoint.Examined++
			bounds, err := w.utc.Bounds(ctx)
			if err != nil {
				return jobs.Step{}, err
			}
			due := upload.State == "Completing" && !bounds.Lower.Before(upload.LeaseUntil.Add(5*time.Second))
			if kind == "purge_uploads" {
				due = upload.State == "Aborting" || upload.State == "Active" && !bounds.Lower.Before(upload.ExpiresAt)
			}
			if !due || j.DryRun {
				checkpoint.Skipped++
			} else {
				svc, err := w.objects(ctx, upload.Object.Bucket)
				if err != nil {
					return jobs.Step{}, err
				}
				if kind == "recover_uploads" {
					_, err = svc.RecoverUpload(ctx, upload.ID)
				} else {
					_, err = svc.PurgeExpiredUpload(ctx, upload.ID)
				}
				// A negative authorization decision can have already aborted a Completing
				// upload. Read state independently before deciding whether work settled.
				observed, readErr := w.store.GetUpload(ctx, upload.ID)
				if readErr != nil {
					return jobs.Step{}, errors.Join(err, readErr)
				}
				switch observed.State {
				case "Completed":
					checkpoint.Completed++
				case "Aborted":
					checkpoint.Aborted++
				case "Active":
					if err != nil && !errors.Is(err, storage.ErrConflict) && !errors.Is(err, storage.ErrAccessDenied) {
						return jobs.Step{}, err
					}
					checkpoint.Skipped++
				default:
					if err != nil {
						return jobs.Step{}, err
					}
					return jobs.Step{}, storage.ErrUnavailable
				}
			}
		}
		checkpoint.After = next
		if next == "" {
			return settled(true, "")
		}
		if checkpoint.Pages >= 10000 {
			return settled(true, "scan_limit")
		}
	}
	return settled(false, "")
}
