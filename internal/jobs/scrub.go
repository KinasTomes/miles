package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/pack"
	"github.com/KinasTomes/miles/internal/storage"
)

type ScrubParams struct {
	Limit int `json:"limit,omitempty"`
}

type ScrubResult struct {
	Candidates  uint64            `json:"candidates"`
	Checked     uint64            `json:"checked"`
	Corrupt     uint64            `json:"corrupt"`
	Skipped     uint64            `json:"skipped"`
	SkipReasons map[string]uint64 `json:"skip_reasons"`
	Errors      []string          `json:"errors,omitempty"`
}

type ScrubWorker struct {
	Store storage.MetadataStore
	Packs storage.PackStore
}

func (w *ScrubWorker) Executor() Executor {
	return Executor{
		Kind: "scrub",
		Validate: func(p json.RawMessage) error {
			if len(p) == 0 {
				return nil
			}
			var params ScrubParams
			if err := json.Unmarshal(p, &params); err != nil {
				return storage.ErrInvalid
			}
			if params.Limit < 0 || params.Limit > 1000 {
				return storage.ErrInvalid
			}
			return nil
		},
		Step: w.Step,
	}
}

func (w *ScrubWorker) Step(ctx context.Context, j Job, _ config.GlobalSnapshot) (Step, error) {
	if j.DryRun || j.State == Cancelling {
		res, _ := json.Marshal(ScrubResult{
			SkipReasons: map[string]uint64{"dry_run_or_cancelled": 1},
		})
		return Step{Settled: true, Done: true, Result: res}, nil
	}

	limit := 100
	if len(j.Params) > 0 {
		var params ScrubParams
		if err := json.Unmarshal(j.Params, &params); err == nil && params.Limit > 0 {
			limit = params.Limit
		}
	}

	res := ScrubResult{
		SkipReasons: make(map[string]uint64),
		Errors:      []string{},
	}

	if w.Store == nil || w.Packs == nil {
		res.SkipReasons["missing_store_or_packs"] = 1
		data, _ := json.Marshal(res)
		return Step{Settled: true, Done: true, Result: data}, nil
	}

	// List candidate packs across states
	for _, state := range []string{"Warm", "Cold"} {
		if ctx.Err() != nil {
			return Step{}, ctx.Err()
		}

		page, err := w.Store.ListPacksByState(ctx, storage.PackListRequest{
			State: state,
			Limit: limit,
		})
		if err != nil {
			if !errors.Is(err, storage.ErrNotFound) {
				res.Errors = append(res.Errors, fmt.Sprintf("list packs %s: %v", state, err))
			}
			continue
		}

		for _, p := range page.Packs {
			if ctx.Err() != nil {
				return Step{}, ctx.Err()
			}
			res.Candidates++

			// Do not attempt to auto-restore GLACIER or cold unreadable packs
			if p.Class == storage.Glacier || p.Class == storage.GlacierIR {
				// If not restored, skip
				res.Skipped++
				res.SkipReasons["glacier_not_restored"]++
				continue
			}

			// Read pack footer and index
			entries, _, err := pack.ReadFooter(ctx, w.Packs, p.Ref)
			if err != nil {
				res.Corrupt++
				res.Errors = append(res.Errors, fmt.Sprintf("pack %s footer read error: %v", p.Ref.ID, err))
				continue
			}

			// Verify chunks in the pack
			packCorrupt := false
			for _, entry := range entries {
				if ctx.Err() != nil {
					return Step{}, ctx.Err()
				}

				blob, err := pack.ReadBlobRange(ctx, w.Packs, p.Ref, entry)
				if err != nil {
					packCorrupt = true
					res.Corrupt++
					res.Errors = append(res.Errors, fmt.Sprintf("pack %s chunk %s read error: %v", p.Ref.ID, entry.Key.Hash, err))
					break
				}

				sum := sha256.Sum256(blob)
				hexHash := hex.EncodeToString(sum[:])
				if hexHash != entry.Key.Hash {
					packCorrupt = true
					res.Corrupt++
					res.Errors = append(res.Errors, fmt.Sprintf("pack %s chunk %s hash mismatch (got %s)", p.Ref.ID, entry.Key.Hash, hexHash))
					break
				}
			}

			if !packCorrupt {
				res.Checked++
			}

			if int(res.Candidates) >= limit {
				break
			}
		}

		if int(res.Candidates) >= limit {
			break
		}
	}

	data, err := json.Marshal(res)
	if err != nil {
		return Step{}, err
	}

	return Step{
		Settled: true,
		Done:    true,
		Progress: map[string]uint64{
			"candidates": res.Candidates,
			"checked":    res.Checked,
			"corrupt":    res.Corrupt,
			"skipped":    res.Skipped,
		},
		Result: data,
	}, nil
}
