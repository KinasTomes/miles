package jobs_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/jobs"
	opstest "github.com/KinasTomes/miles/internal/jobs/contracttest"
	"github.com/KinasTomes/miles/internal/storage"
)

type lostJobACK struct {
	jobs.Store
	committed jobs.Job
}

func (s *lostJobACK) CreateJob(_ context.Context, j jobs.Job) error {
	s.committed = j
	return storage.ErrUnavailable
}

type policyFixture struct{}

func (policyFixture) GetPolicy(_ context.Context, r uint64) (storage.PolicySnapshot, error) {
	v, e := config.NewPolicyValidator(config.DSGPolicyLimits())
	if e != nil {
		return storage.PolicySnapshot{}, e
	}
	p, e := v.NewGlobalSnapshot(1, config.DefaultGlobalPolicy())
	if e != nil {
		return storage.PolicySnapshot{}, e
	}
	return p.StorageSnapshot()
}
func TestUnknownCreateOutcomeRetainsIdentityWithoutSuccessObservation(t *testing.T) {
	store := new(lostJobACK)
	observed := 0
	svc := jobs.Service{Store: store, Policies: policyFixture{}, Observe: func(jobs.Type, jobs.State) { observed++ }, Now: func() time.Time { return opstest.Now }}
	j, e := svc.Create(context.Background(), jobs.Create{Type: "gc"})
	if !errors.Is(e, storage.ErrUnavailable) || j.ID == "" || j.ID != store.committed.ID || observed != 0 {
		t.Fatal(j, e, observed)
	}
}
