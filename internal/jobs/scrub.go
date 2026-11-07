package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/pack"
	"github.com/KinasTomes/miles/internal/storage"
)

type ScrubParams struct {
	Limit int              `json:"limit,omitempty"`
	State string           `json:"state,omitempty"`
	After *storage.PackRef `json:"after,omitempty"`
}

type ScrubResult struct {
	Scope       string            `json:"scope,omitempty"`
	Candidates  uint64            `json:"candidates"`
	Checked     uint64            `json:"checked"`
	Corrupt     uint64            `json:"corrupt"`
	Skipped     uint64            `json:"skipped"`
	SkipReasons map[string]uint64 `json:"skip_reasons"`
	Errors      []string          `json:"errors,omitempty"`
	Partial     bool              `json:"partial"`
	NextState   string            `json:"next_state,omitempty"`
	NextAfter   *storage.PackRef  `json:"next_after,omitempty"`
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
			if params.State != "" && params.State != "Warm" && params.State != "Cold" && params.State != "Restored" {
				return storage.ErrInvalid
			}
			if params.After != nil {
				if err := params.After.Validate(); err != nil {
					return storage.ErrInvalid
				}
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
	var startState string
	var afterCursor *storage.PackRef

	if len(j.Params) > 0 {
		var params ScrubParams
		if err := json.Unmarshal(j.Params, &params); err == nil {
			if params.Limit > 0 {
				limit = params.Limit
			}
			if params.State != "" {
				startState = params.State
			}
			if params.After != nil {
				afterCursor = params.After
			}
		}
	}

	res := ScrubResult{
		Scope:       "Warm,Cold,Restored",
		SkipReasons: make(map[string]uint64),
		Errors:      []string{},
	}

	if w.Store == nil || w.Packs == nil {
		res.SkipReasons["missing_store_or_packs"] = 1
		data, _ := json.Marshal(res)
		return Step{
			Settled:   true,
			Done:      true,
			ErrorCode: "store_unavailable",
			Result:    data,
		}, nil
	}

	allStates := []string{"Warm", "Cold", "Restored"}
	startIdx := 0
	if startState != "" {
		for i, s := range allStates {
			if s == startState {
				startIdx = i
				break
			}
		}
	}

	currentAfter := afterCursor
	hitLimit := false

	for stateIdx := startIdx; stateIdx < len(allStates); stateIdx++ {
		state := allStates[stateIdx]
		if stateIdx > startIdx {
			currentAfter = nil
		}

		for {
			if ctx.Err() != nil {
				return Step{}, ctx.Err()
			}

			pageLimit := limit - int(res.Candidates)
			if pageLimit > 100 {
				pageLimit = 100
			}
			if pageLimit <= 0 {
				hitLimit = true
				break
			}

			page, err := w.Store.ListPacksByState(ctx, storage.PackListRequest{
				State: state,
				After: currentAfter,
				Limit: pageLimit,
			})
			if err != nil {
				if !errors.Is(err, storage.ErrNotFound) {
					res.Errors = append(res.Errors, fmt.Sprintf("list packs %s: %v", state, err))
				}
				break
			}
			if len(page.Packs) == 0 {
				break
			}

			for _, p := range page.Packs {
				if ctx.Err() != nil {
					return Step{}, ctx.Err()
				}
				res.Candidates++
				currentAfter = &p.Ref

				// Check readability
				readable := false
				switch p.State {
				case "Warm":
					readable = true
				case "Restored":
					readable = true
				case "Cold":
					if p.Class == storage.GlacierIR {
						// Glacier Instant Retrieval is readable directly despite Cold state
						readable = true
					} else {
						res.Skipped++
						res.SkipReasons["glacier_not_restored"]++
					}
				default:
					res.Skipped++
					res.SkipReasons["unsupported_state_"+strings.ToLower(p.State)]++
				}

				if !readable {
					if int(res.Candidates) >= limit {
						break
					}
					continue
				}

				// Read pack footer and index
				entries, _, err := pack.ReadFooter(ctx, w.Packs, p.Ref)
				if err != nil {
					res.Corrupt++
					res.Errors = append(res.Errors, fmt.Sprintf("pack %s footer read error: %v", p.Ref.ID, err))
					if int(res.Candidates) >= limit {
						break
					}
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
				hitLimit = true
				if page.NextAfter != nil {
					res.Partial = true
					res.NextState = state
					res.NextAfter = page.NextAfter
				}
				break
			}

			if page.NextAfter == nil {
				break
			}
			currentAfter = page.NextAfter
		}

		if hitLimit {
			if !res.Partial && stateIdx+1 < len(allStates) {
				res.Partial = true
				res.NextState = allStates[stateIdx+1]
				res.NextAfter = nil
			}
			break
		}
	}

	data, err := json.Marshal(res)
	if err != nil {
		return Step{}, err
	}

	errorCode := ""
	if res.Corrupt > 0 {
		errorCode = "corruption_detected"
	} else if len(res.Errors) > 0 {
		errorCode = "io_error"
	}

	return Step{
		Settled:   true,
		Done:      true,
		ErrorCode: errorCode,
		Progress: map[string]uint64{
			"candidates": res.Candidates,
			"checked":    res.Checked,
			"corrupt":    res.Corrupt,
			"skipped":    res.Skipped,
		},
		Result: data,
	}, nil
}
