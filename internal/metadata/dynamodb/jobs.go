package dynamodb

import (
	"context"
	"errors"
	"reflect"

	"github.com/KinasTomes/miles/internal/jobs"
	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

var _ jobs.Store = (*Store)(nil)

func (m *Store) CreateJob(ctx context.Context, j jobs.Job) error {
	if j.Validate() != nil || j.State != jobs.Queued || j.Revision != 1 {
		return s.ErrInvalid
	}
	return m.update(ctx, func(t *transaction) error {
		if _, e := get[s.PolicySnapshot](t, policyKey(j.PolicyRevision)); e != nil {
			return e
		}
		found, e := exists(t, key("jobs", j.ID))
		if e != nil {
			return e
		}
		if found {
			return s.ErrConflict
		}
		return put(t, key("jobs", j.ID), j)
	})
}
func (m *Store) GetJob(ctx context.Context, id string) (jobs.Job, error) {
	if !jobs.ValidID(id) {
		return jobs.Job{}, s.ErrInvalid
	}
	j, e := read[jobs.Job](m, ctx, key("jobs", id))
	if e == nil && (j.ID != id || j.Validate() != nil) {
		e = s.ErrUnavailable
	}
	return j, e
}
func (m *Store) ChangeJob(ctx context.Context, c jobs.Change) (jobs.Job, error) {
	var out jobs.Job
	if !jobs.ValidID(c.JobID) {
		return out, s.ErrInvalid
	}
	e := m.update(ctx, func(t *transaction) error {
		old, e := get[jobs.Job](t, key("jobs", c.JobID))
		if e != nil {
			return e
		}
		if c.WorkerChange() {
			if e = m.fence(t, c.Fence); e != nil {
				return e
			}
			n, e := get[jobs.Node](t, key("nodes", c.NodeID))
			if e != nil {
				return e
			}
			if n.Incarnation != c.NodeIncarnation || n.ServiceIncarnation != c.Fence.ServiceIncarnation {
				return s.ErrFenced
			}
			if (c.Kind == "claim" && !n.Ready) || ((c.Kind == "claim" || c.Kind == "begin_step") && n.Draining) {
				return s.ErrConflict
			}
			if _, e = get[s.PolicySnapshot](t, policyKey(old.PolicyRevision)); e != nil {
				return e
			}
		}
		out, e = jobs.Apply(old, c, m.now())
		if e != nil {
			return e
		}
		if out.Revision == old.Revision {
			return nil
		}
		return put(t, key("jobs", c.JobID), out)
	})
	return out, e
}
func (m *Store) GetNode(ctx context.Context, id string) (jobs.Node, error) {
	if !jobs.ValidID(id) {
		return jobs.Node{}, s.ErrInvalid
	}
	n, e := read[jobs.Node](m, ctx, key("nodes", id))
	if e == nil && (n.ID != id || n.Validate() != nil) {
		e = s.ErrUnavailable
	}
	return n, e
}
func (m *Store) PutNode(ctx context.Context, expected uint64, n jobs.Node) (jobs.Node, error) {
	if expected >= s.MaxPolicyRevision {
		return jobs.Node{}, s.ErrConflict
	}
	n.Revision = expected + 1
	if n.Validate() != nil {
		return jobs.Node{}, s.ErrInvalid
	}
	e := m.update(ctx, func(t *transaction) error {
		old, e := get[jobs.Node](t, key("nodes", n.ID))
		if e != nil && !errors.Is(e, s.ErrNotFound) {
			return e
		}
		if old.Revision != expected {
			return s.ErrConflict
		}
		if expected > 0 {
			if old.Incarnation != n.Incarnation || old.ServiceIncarnation != n.ServiceIncarnation || old.Profile != n.Profile {
				return s.ErrFenced
			}
			n.Draining = n.Draining || old.Draining
			if n.Draining {
				n.Ready = false
			}
		}
		return put(t, key("nodes", n.ID), n)
	})
	return n, e
}

// A base-table Scan page is discovery, not a cluster snapshot or liveness proof.
// It retains native continuation keys and never materializes/sorts the whole table.
func listOperational[T any](m *Store, ctx context.Context, table, pk string, p jobs.PageRequest) ([]T, string, error) {
	if e := p.Validate(); e != nil {
		return nil, "", e
	}
	input := &ddb.ScanInput{TableName: aws.String(m.tables[table]), ConsistentRead: aws.Bool(true), Limit: aws.Int32(int32(p.Limit))}
	if p.Cursor != "" {
		input.ExclusiveStartKey = Item{pk: text(p.Cursor)}
	}
	page, e := m.api.Scan(ctx, input)
	if e != nil {
		return nil, "", backend(e)
	}
	if page == nil {
		return nil, "", s.ErrUnavailable
	}
	out := []T{}
	for _, item := range page.Items {
		var v T
		if e = unmarshalNative(&types.AttributeValueMemberM{Value: item}, reflect.ValueOf(&v).Elem()); e != nil {
			return nil, "", e
		}
		id := ""
		switch x := any(v).(type) {
		case jobs.Job:
			if x.Validate() != nil {
				return nil, "", s.ErrUnavailable
			}
			id = x.ID
		case jobs.Node:
			if x.Validate() != nil {
				return nil, "", s.ErrUnavailable
			}
			id = x.ID
		}
		key, ok := item[pk].(*types.AttributeValueMemberS)
		if !ok || key.Value != id {
			return nil, "", s.ErrUnavailable
		}
		out = append(out, v)
	}
	next := ""
	if len(page.LastEvaluatedKey) > 0 {
		input.ExclusiveStartKey = page.LastEvaluatedKey
		input.Limit = aws.Int32(1)
		look, e := m.api.Scan(ctx, input)
		if e != nil {
			return nil, "", backend(e)
		}
		if look == nil {
			return nil, "", s.ErrUnavailable
		}
		if len(look.Items) > 0 {
			last, ok := page.LastEvaluatedKey[pk].(*types.AttributeValueMemberS)
			if !ok || !jobs.ValidID(last.Value) {
				return nil, "", s.ErrUnavailable
			}
			next = last.Value
		}
	}
	return out, next, nil
}
func (m *Store) ListJobs(ctx context.Context, p jobs.PageRequest) (jobs.JobPage, error) {
	v, n, e := listOperational[jobs.Job](m, ctx, "Jobs", "job_id", p)
	return jobs.JobPage{Jobs: v, Next: n}, e
}
func (m *Store) ListNodes(ctx context.Context, p jobs.PageRequest) (jobs.NodePage, error) {
	v, n, e := listOperational[jobs.Node](m, ctx, "Nodes", "node_id", p)
	return jobs.NodePage{Nodes: v, Next: n}, e
}
