package lifecycle

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/KinasTomes/miles/internal/jobs"
	s "github.com/KinasTomes/miles/internal/storage"
	"io"
	"time"
)

type Params struct {
	PackID     string `json:"pack_id"`
	Generation string `json:"generation"`
	Days       int    `json:"days,omitempty"`
	Tier       string `json:"tier,omitempty"`
	// RequiredUntil is set only by trusted read admission, not the Admin body.
	RequiredUntil time.Time `json:"required_until,omitzero"`
}

func (p Params) Ref() s.PackRef { return s.PackRef{ID: p.PackID, Generation: p.Generation} }
func (p Params) Validate(kind jobs.Type) error {
	if p.Ref().Validate() != nil {
		return s.ErrInvalid
	}
	if kind == "pack_restore" {
		if (s.RestoreRequest{Days: p.Days, Tier: p.Tier}).Validate() != nil {
			return s.ErrInvalid
		}
		if !p.RequiredUntil.IsZero() && (p.RequiredUntil.Year() < 1 || p.RequiredUntil.Year() > 9999) {
			return s.ErrInvalid
		}
	} else if (kind != "tiering" && kind != "gc" && !Repack(kind)) || p.Days != 0 || p.Tier != "" || !p.RequiredUntil.IsZero() {
		return s.ErrInvalid
	}
	return nil
}

// Repack jobs share immutable destination publication and canonical CAS.
func Repack(kind jobs.Type) bool {
	return kind == "pack_promote" || kind == "compaction"
}

// RetirableData excludes pending restore/transition and terminal generations.
// Eligibility still requires stable roots, pins, protection and retention proof.
func RetirableData(p s.PackInfo) bool {
	return p.State == "Warm" && (p.Class == s.Standard || p.Class == s.StandardIA) || p.State == "Cold" && (p.Class == s.GlacierIR || p.Class == s.Glacier) || p.State == "Restored" && p.Class == s.Glacier
}

func Destination(kind jobs.Type, id string, source s.PackRef) s.PackRef {
	prefix := "promote/"
	if kind == "compaction" {
		prefix = "compact/"
	}
	sum := sha256.Sum256([]byte(prefix + id))
	return s.PackRef{ID: source.ID, Generation: hex.EncodeToString(sum[:16])}
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
	SweepVersions []s.SweepVersion `json:",omitempty"`
	RetiredAt     time.Time        `json:",omitzero"`
	PlanPages     uint32
	PlanSHA256    string
	KnownApplied  bool
	Version       uint32
	ID            string
	JobID         string
	Kind          jobs.Type
	State         string
	Revision      uint64
	Fence         s.WriterFence
	Source        s.PackInfo
	Target        s.StorageClass
	ResultPack    s.PackInfo
	Destination   s.PackRef
	Chunks        []s.Chunk
	LiveBytes     int64
	Digest        string
	GateRevision  uint64
	Days          int
	Tier          string
	RequiredUntil time.Time `json:",omitzero"`
}

func (a Action) Validate() error {
	if !a.RequiredUntil.IsZero() && (a.Kind != "pack_restore" || a.RequiredUntil.Year() < 1 || a.RequiredUntil.Year() > 9999) {
		return s.ErrInvalid
	}
	if len(a.SweepVersions) > 0 && (a.Kind != "gc" || !s.ValidSweepVersions(a.SweepVersions)) {
		return s.ErrInvalid
	}

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
