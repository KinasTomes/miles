package lifecycle

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/KinasTomes/miles/internal/jobs"
	s "github.com/KinasTomes/miles/internal/storage"
	"io"
)

type Params struct {
	PackID     string `json:"pack_id"`
	Generation string `json:"generation"`
	Days       int    `json:"days,omitempty"`
	Tier       string `json:"tier,omitempty"`
}

func (p Params) Ref() s.PackRef { return s.PackRef{ID: p.PackID, Generation: p.Generation} }
func (p Params) Validate(kind jobs.Type) error {
	if p.Ref().Validate() != nil {
		return s.ErrInvalid
	}
	if kind == "pack_restore" {
		if p.Days < 1 || p.Days > 30 || p.Tier != "Standard" && p.Tier != "Bulk" && p.Tier != "Expedited" {
			return s.ErrInvalid
		}
	} else if (kind != "tiering" && kind != "pack_promote") || p.Days != 0 || p.Tier != "" {
		return s.ErrInvalid
	}
	return nil
}
func Decode(b json.RawMessage, kind jobs.Type) (Params, error) {
	var p Params
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if d.Decode(&p) != nil {
		return p, s.ErrInvalid
	}
	var tail any
	if d.Decode(&tail) != io.EOF {
		return p, s.ErrInvalid
	}
	return p, p.Validate(kind)
}

// Action is the durable write-ahead plan. Unknown is not permission to retry a
// mutable copy or to delete an old generation. Plan is bounded, never truncated.
type Action struct {
	PlanPages    uint32
	PlanSHA256   string
	KnownApplied bool
	Version      uint32
	ID           string
	JobID        string
	Kind         jobs.Type
	State        string
	Revision     uint64
	Fence        s.WriterFence
	Source       s.PackInfo
	Target       s.StorageClass
	ResultPack   s.PackInfo
	Destination  s.PackRef
	Chunks       []s.Chunk
	LiveBytes    int64
	Digest       string
	GateRevision uint64
	Days         int
	Tier         string
}

func (a Action) Validate() error {
	if a.Version != 1 || !jobs.ValidID(a.JobID) || a.ID != a.JobID || a.Revision < 1 || a.Revision >= s.MaxPolicyRevision || a.Source.Ref.Validate() != nil || a.Fence.LeaseID != a.JobID || len(a.Chunks) > 16384 || a.PlanPages > 512 {
		return s.ErrInvalid
	}
	switch a.State {
	case "Prepared", "Ready", "Dispatched", "Applied", "Unknown", "Waiting", "Done", "Skipped", "NotApplied":
	default:
		return s.ErrInvalid
	}
	return nil
}

type jobKey struct{}

func WithJob(ctx context.Context, j jobs.Job) context.Context {
	return context.WithValue(ctx, jobKey{}, j)
}
func JobFromContext(ctx context.Context) (jobs.Job, bool) {
	j, ok := ctx.Value(jobKey{}).(jobs.Job)
	return j, ok
}
