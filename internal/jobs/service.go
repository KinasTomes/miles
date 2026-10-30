package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/storage"
)

type PolicyReader interface {
	GetPolicy(context.Context, uint64) (storage.PolicySnapshot, error)
}
type Service struct {
	Store    Store
	Policies PolicyReader
	Now      func() time.Time
	Observe  func(Type, State)
	// Renew is the R1 provider's job-lease heartbeat, bound to LeaseID=job_id.
	// It must reject expired/closed authority; HeartbeatAt never renews a lease.
	Renew func(context.Context, storage.WriterFence) error
}
type Create struct {
	Type           Type            `json:"type"`
	DryRun         bool            `json:"dry_run"`
	Params         json.RawMessage `json:"params"`
	PolicyRevision uint64          `json:"policy_revision,omitempty"`
}

// Create captures one validated exact policy revision. Resume re-loads that
// history; missing history never falls back to the current policy.
func (s Service) Create(ctx context.Context, request Create) (Job, error) {
	if s.Store == nil || s.Policies == nil || !request.Type.Valid() {
		return Job{}, storage.ErrInvalid
	}
	v, e := config.NewPolicyValidator(config.DSGPolicyLimits())
	if e != nil {
		return Job{}, e
	}
	record, readErr := s.Policies.GetPolicy(ctx, request.PolicyRevision)
	p, e := v.ResolveGlobal(request.PolicyRevision, record, readErr)
	if e != nil {
		return Job{}, e
	}
	if len(request.Params) == 0 {
		request.Params = json.RawMessage(`{}`)
	}
	var id [16]byte
	if _, e = rand.Read(id[:]); e != nil {
		return Job{}, e
	}
	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now().UTC()
	}
	j := Job{ID: "job_" + hex.EncodeToString(id[:]), Type: request.Type, State: Queued, DryRun: request.DryRun, Params: append(json.RawMessage(nil), request.Params...), PolicyRevision: p.Revision(), Revision: 1, CreatedAt: now, Progress: map[string]uint64{}}
	if e = j.Validate(); e != nil {
		return Job{}, e
	}
	if e = s.Store.CreateJob(ctx, j); e != nil {
		// A backend error may follow commit. Retain the generated identity so
		// callers can reconcile this job rather than creating another blindly.
		return j, e
	}
	if s.Observe != nil {
		s.Observe(j.Type, j.State)
	}
	return j, nil
}

func (s Service) Change(ctx context.Context, c Change) (Job, error) {
	if s.Store == nil {
		return Job{}, storage.ErrInvalid
	}
	j, e := s.Store.ChangeJob(ctx, c)
	if e == nil && j.Revision != c.ExpectedRevision && s.Observe != nil {
		s.Observe(j.Type, j.State)
	}
	return j, e
}

// Cancel is a monotone request flag, so a known CAS conflict can retry against
// the new record. Unknown outcomes are returned for reconciliation, not replayed.
func (s Service) Cancel(ctx context.Context, id string) (Job, error) {
	if s.Store == nil || !ValidID(id) {
		return Job{}, storage.ErrInvalid
	}
	for range 4 {
		j, e := s.Store.GetJob(ctx, id)
		if e != nil {
			return Job{}, e
		}
		out, e := s.Change(ctx, Change{JobID: id, ExpectedRevision: j.Revision, Kind: "cancel"})
		if errors.Is(e, storage.ErrConflict) {
			continue
		}
		return out, e
	}
	return Job{}, storage.ErrConflict
}

func (s Service) Heartbeat(ctx context.Context, c Change) (Job, error) {
	if s.Store == nil || s.Renew == nil {
		return Job{}, storage.ErrUnavailable
	}
	j, e := s.Store.GetJob(ctx, c.JobID)
	if e != nil {
		return Job{}, e
	}
	if j.Revision != c.ExpectedRevision {
		return Job{}, storage.ErrConflict
	}
	if j.State.Terminal() || j.Worker != c.Fence || j.WorkerNodeID != c.NodeID || j.WorkerNodeIncarnation != c.NodeIncarnation {
		return Job{}, storage.ErrFenced
	}
	if e = s.Renew(ctx, c.Fence); e != nil {
		return Job{}, e
	}
	c.Kind = "heartbeat"
	return s.Change(ctx, c)
}

func (s Service) ResumePolicy(ctx context.Context, j Job) (config.GlobalSnapshot, error) {
	if s.Policies == nil || j.Validate() != nil {
		return config.GlobalSnapshot{}, storage.ErrInvalid
	}
	v, e := config.NewPolicyValidator(config.DSGPolicyLimits())
	if e != nil {
		return config.GlobalSnapshot{}, e
	}
	r, e := s.Policies.GetPolicy(ctx, j.PolicyRevision)
	return v.ResolveGlobal(j.PolicyRevision, r, e)
}
