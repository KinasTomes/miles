package jobs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/storage"
)

var ErrNodeNotFound = fmt.Errorf("%w: target node does not exist", storage.ErrNotFound)
var ErrTooManyTargets = fmt.Errorf("%w: cluster fan-out supports at most 64 active targets", storage.ErrInvalid)

type target struct {
	ID          string `json:"node_id"`
	Incarnation string `json:"incarnation"`
}
type command struct {
	Scope       string   `json:"scope"`
	NodeID      string   `json:"node_id,omitempty"`
	Incarnation string   `json:"incarnation,omitempty"`
	Targets     []target `json:"targets,omitempty"`
	Child       bool     `json:"child,omitempty"`
}

// Cluster freezes membership in durable job parameters. A target consumes its
// own command from shared metadata; no bearer is forwarded to a peer or to Edge.
type Cluster struct {
	Runner   *Runner
	Registry *Registry
	Gate     Gate
}

func (c *Cluster) GetJob(ctx context.Context, id string) (Job, error) {
	return c.Runner.Service.Store.GetJob(ctx, id)
}
func (c *Cluster) ListJobs(ctx context.Context, p PageRequest) (JobPage, error) {
	return c.Runner.Service.Store.ListJobs(ctx, p)
}
func (c *Cluster) ListNodes(ctx context.Context, p PageRequest) (NodePage, error) {
	return c.Runner.Service.Store.ListNodes(ctx, p)
}
func (c *Cluster) Cancel(ctx context.Context, id string) (Job, error) {
	return c.Runner.Service.Cancel(ctx, id)
}
func decodeCommand(b json.RawMessage) (command, error) {
	var p command
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if d.Decode(&p) != nil {
		return p, storage.ErrInvalid
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return p, storage.ErrInvalid
	}
	return p, nil
}
func (c *Cluster) Create(ctx context.Context, q Create) (Job, error) {
	if q.Type != "cache_purge" && q.Type != "drain" {
		return c.Runner.Create(ctx, q)
	}
	if len(q.Params) == 0 {
		q.Params = json.RawMessage(`{}`)
	}
	p, e := decodeCommand(q.Params)
	if e != nil {
		return Job{}, e
	}
	// Membership and internal child flags are never accepted from the API.
	if p.Child || p.Incarnation != "" || len(p.Targets) > 0 {
		return Job{}, storage.ErrInvalid
	}
	if p.Scope == "" {
		p.Scope = "node"
	}
	if q.Type == "drain" {
		if p.Scope != "node" || !ValidID(p.NodeID) {
			return Job{}, storage.ErrInvalid
		}
		return c.drain(ctx, p.NodeID, q.DryRun, q.PolicyRevision)
	}
	switch p.Scope {
	case "node":
		if p.NodeID == "" {
			p.NodeID = c.Registry.Identity.ID
		}
		n, e := c.Runner.Service.Store.GetNode(ctx, p.NodeID)
		if e != nil {
			if errors.Is(e, storage.ErrNotFound) {
				return Job{}, ErrNodeNotFound
			}
			return Job{}, e
		}
		p.Incarnation = n.Incarnation
		p.Targets = []target{{n.ID, n.Incarnation}}
	case "cluster":
		if p.NodeID != "" {
			return Job{}, storage.ErrInvalid
		}
		cursor := ""
		for pages := 0; pages < 100; pages++ {
			page, e := c.ListNodes(ctx, PageRequest{Limit: 100, Cursor: cursor})
			if e != nil {
				return Job{}, e
			}
			for _, n := range page.Nodes {
				// Nodes rows outlive processes. Retain recently missing peers for
				// partial results, without including every historical boot forever.
				if n.ServiceIncarnation != c.Registry.Identity.ServiceIncarnation || c.Registry.Clock.Now().WallUTC.Sub(n.HeartbeatAt) > 2*time.Minute {
					continue
				}
				p.Targets = append(p.Targets, target{n.ID, n.Incarnation})
			}
			if len(p.Targets) > 64 {
				return Job{}, ErrTooManyTargets
			}
			if page.Next == "" {
				break
			}
			if page.Next == cursor || pages == 99 {
				return Job{}, storage.ErrUnavailable
			}
			cursor = page.Next
		}
		if len(p.Targets) == 0 {
			return Job{}, storage.ErrUnavailable
		}
		sort.Slice(p.Targets, func(i, j int) bool { return p.Targets[i].ID < p.Targets[j].ID })
	default:
		return Job{}, storage.ErrInvalid
	}
	q.Params, _ = json.Marshal(p)
	return c.Runner.Create(ctx, q)
}
func (c *Cluster) Drain(ctx context.Context, id string) (Job, error) {
	return c.drain(ctx, id, false, 0)
}
func (c *Cluster) drain(ctx context.Context, id string, dry bool, revision uint64) (Job, error) {
	n, e := c.Runner.Service.Store.GetNode(ctx, id)
	if e != nil {
		if errors.Is(e, storage.ErrNotFound) {
			return Job{}, ErrNodeNotFound
		}
		return Job{}, e
	}
	p, _ := json.Marshal(command{Scope: "node", NodeID: id, Incarnation: n.Incarnation})
	j, e := c.Runner.Create(ctx, Create{Type: "drain", Params: p, DryRun: dry, PolicyRevision: revision})
	if e != nil || dry {
		return j, e
	}
	gate := c.Gate
	if id != c.Registry.Identity.ID {
		gate = nil
	}
	_, e = c.Registry.RequestDrain(ctx, id, gate)
	if e != nil {
		return j, e
	}
	// A successful response acknowledges the actual target gate, rather than
	// equating the durable request marker with a closed admission gate.
	expected := n.Incarnation
	for {
		n, e = c.Runner.Service.Store.GetNode(ctx, id)
		if e != nil {
			return j, e
		}
		if n.Incarnation != c.Registry.Identity.Incarnation && id == c.Registry.Identity.ID {
			return j, storage.ErrFenced
		}
		if n.Incarnation != expected {
			return j, storage.ErrFenced
		}
		if n.DrainApplied {
			return j, nil
		}
		if e = c.Registry.Clock.Sleep(ctx, 100*time.Millisecond); e != nil {
			return j, e
		}
	}
}

func commandEligible(j Job, n Node) bool {
	p, e := decodeCommand(j.Params)
	if e != nil {
		return false
	}
	return !p.Child || (p.NodeID == n.ID && p.Incarnation == n.Incarnation)
}

// CacheExecutor uses the already implemented local cache primitive. Public
// cache HTTP routes and metric aggregation remain owned by the cache API layer.
func (c *Cluster) CacheExecutor(purge func(context.Context, string) error) Executor {
	return Executor{Eligible: commandEligible, Step: func(ctx context.Context, j Job, _ config.GlobalSnapshot) (Step, error) {
		p, e := decodeCommand(j.Params)
		if e != nil {
			return Step{Settled: true}, e
		}
		if p.Child {
			if p.NodeID != c.Registry.Identity.ID || p.Incarnation != c.Registry.Identity.Incarnation {
				return Step{Settled: true}, storage.ErrFenced
			}
			if j.State != Cancelling && !j.DryRun {
				e = purge(ctx, p.NodeID)
			}
			result, _ := json.Marshal(map[string]any{"node_id": p.NodeID, "purged": e == nil && !j.DryRun && j.State != Cancelling, "dry_run": j.DryRun})
			return Step{Settled: true, Done: true, Result: result}, e
		}
		return c.fanout(ctx, j, p)
	}}
}
func (c *Cluster) fanout(ctx context.Context, j Job, p command) (Step, error) {
	if (p.Scope != "cluster" && p.Scope != "node") || len(p.Targets) == 0 {
		return Step{Settled: true}, storage.ErrInvalid
	}
	result := Result{Scope: p.Scope, Nodes: []NodeResult{}}
	pending, failed := false, false
	for _, target := range p.Targets {
		sum := sha256.Sum256([]byte(j.ID + "/" + target.ID + "/" + target.Incarnation))
		id := "child_" + hex.EncodeToString(sum[:16])
		params, _ := json.Marshal(command{Scope: "node", NodeID: target.ID, Incarnation: target.Incarnation, Child: true})
		child, e := c.GetJob(ctx, id)
		if errors.Is(e, storage.ErrNotFound) {
			if j.State == Cancelling {
				result.Nodes = append(result.Nodes, NodeResult{NodeID: target.ID, State: "unavailable", ErrorCode: "cancelled_before_dispatch"})
				continue
			}
			child, e = c.Runner.Service.CreateNamed(WithCreationFence(ctx, j.Worker), Create{Type: "cache_purge", DryRun: j.DryRun, Params: params, PolicyRevision: j.PolicyRevision}, id)
			if e != nil {
				child, e = c.GetJob(ctx, id)
			} // exact read reconciles both conflict and lost ACK
		}
		if e != nil {
			return Step{}, e
		}
		if child.Type != j.Type || child.PolicyRevision != j.PolicyRevision || child.DryRun != j.DryRun || !bytes.Equal(child.Params, params) {
			return Step{}, storage.ErrFenced
		}
		expired := !c.Registry.Clock.Now().WallUTC.Before(j.CreatedAt.Add(60 * time.Second))
		node, readErr := c.Runner.Service.Store.GetNode(ctx, target.ID)
		unavailable := readErr != nil || node.Incarnation != target.Incarnation || node.ServiceIncarnation != c.Registry.Identity.ServiceIncarnation || node.HeartbeatAt.After(c.Registry.Clock.Now().WallUTC.Add(2*time.Second)) || c.Registry.Clock.Now().WallUTC.Sub(node.HeartbeatAt) >= HeartbeatMaxAge
		if j.State == Cancelling || expired || unavailable {
			if !child.State.Terminal() {
				child, e = c.Cancel(ctx, id)
				if e != nil {
					return Step{}, e
				}
			}
		}
		nr := NodeResult{NodeID: target.ID, State: "succeeded", Result: child.Result}
		if !child.State.Terminal() {
			if j.State == Cancelling || (!expired && !unavailable) {
				pending = true
				continue
			}
			nr.State = "timeout"
			nr.ErrorCode = "outcome_unknown"
			failed = true
			result.Nodes = append(result.Nodes, nr)
			continue
		}
		switch {
		case unavailable:
			nr.State = "unavailable"
			nr.ErrorCode = "node_unavailable"
			failed = true
		case expired && child.State != Succeeded:
			nr.State = "timeout"
			nr.ErrorCode = "node_timeout"
			failed = true
		case child.State != Succeeded:
			nr.State = "failed"
			nr.ErrorCode = "child_failed"
			failed = true
		}
		result.Nodes = append(result.Nodes, nr)
	}
	if pending {
		return Step{}, nil
	} // cancellation waits for every active child safe point
	if e := result.Validate(); e != nil {
		return Step{}, e
	}
	b, e := json.Marshal(result)
	if e != nil {
		return Step{}, e
	}
	out := Step{Settled: true, Done: true, Result: b, Progress: map[string]uint64{"targets": uint64(len(p.Targets))}}
	if failed {
		out.ErrorCode = "partial_cluster"
	}
	return out, nil
}
func (c *Cluster) DrainExecutor() Executor {
	return Executor{Step: func(ctx context.Context, j Job, _ config.GlobalSnapshot) (Step, error) {
		p, e := decodeCommand(j.Params)
		if e != nil {
			return Step{Settled: true}, e
		}
		n, e := c.Runner.Service.Store.GetNode(ctx, p.NodeID)
		if e != nil {
			return Step{}, e
		}
		if n.Incarnation != p.Incarnation {
			return Step{Settled: true}, storage.ErrFenced
		}
		if j.DryRun {
			return Step{Settled: true, Done: true, Result: json.RawMessage(`{"dry_run":true,"flush_complete":false}`)}, nil
		}
		if j.State == Cancelling {
			return Step{Settled: true, Result: json.RawMessage(`{"drain_remains_requested":true}`)}, nil
		}
		if n.DrainState == "incomplete" || !c.Registry.Clock.Now().WallUTC.Before(j.CreatedAt.Add(35*time.Minute)) {
			return Step{Settled: true, Done: true, ErrorCode: "drain_incomplete", Result: json.RawMessage(`{"flush_complete":false}`)}, nil
		}
		if n.Profile == "edge" && n.DrainApplied {
			return Step{Settled: true, Done: true, Result: json.RawMessage(`{"admission_closed":true,"flush_complete":false,"profile":"edge"}`)}, nil
		}
		if n.DrainState == "complete" {
			return Step{Settled: true, Done: true, Result: json.RawMessage(`{"admission_closed":true,"flush_complete":true,"alb_unused":true,"profile":"cloud"}`)}, nil
		}
		return Step{}, nil
	}}
}
