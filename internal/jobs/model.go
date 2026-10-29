// Package jobs owns durable operational records, not GC/tiering executors or R1 authority.
package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"regexp"
	"time"

	"github.com/KinasTomes/miles/internal/storage"
)

const MaxRecordBytes = 128 << 10

var ErrReconciliation = errors.New("job has unsettled work; reconcile before takeover")
var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,127}$`)

func ValidID(s string) bool { return idPattern.MatchString(s) }

type Type string
type State string

const (
	Queued     State = "queued"
	Running    State = "running"
	Cancelling State = "cancelling"
	Succeeded  State = "succeeded"
	Failed     State = "failed"
	Cancelled  State = "cancelled"
)

func Types() []Type {
	return []Type{"tiering", "gc", "compaction", "scrub", "purge_uploads", "recover_uploads", "journal_flush", "recovery_checkpoint", "cache_purge", "drain"}
}
func States() []State          { return []State{Queued, Running, Cancelling, Succeeded, Failed, Cancelled} }
func (s State) Terminal() bool { return s == Succeeded || s == Failed || s == Cancelled }
func (t Type) Valid() bool {
	for _, v := range Types() {
		if t == v {
			return true
		}
	}
	return false
}
func (s State) Valid() bool {
	for _, v := range States() {
		if s == v {
			return true
		}
	}
	return false
}

// Worker/checkpoint fields are internal. HTTP handlers must use View, not encode Job.
// Lease acquisition/renewal/closure belongs to the supplied R1 authority. A Job
// timestamp or an alive Nodes entry never grants permission to dispatch S3 work.
type Job struct {
	ID                                            string
	Type                                          Type
	State                                         State
	DryRun                                        bool
	Params                                        json.RawMessage
	PolicyRevision                                uint64
	Revision                                      uint64
	WorkerNodeID                                  string
	WorkerNodeIncarnation                         string
	Worker                                        storage.WriterFence
	PendingStep                                   string
	Checkpoint                                    json.RawMessage
	Progress                                      map[string]uint64
	Result                                        json.RawMessage
	ErrorCode                                     string
	CreatedAt, StartedAt, FinishedAt, HeartbeatAt time.Time
}

// Native DynamoDB decodes an empty binary into an owned non-nil slice. Optional
// JSON documents still encode as null, rather than an invalid empty JSON token.
func (j Job) MarshalJSON() ([]byte, error) {
	type record Job
	r := record(j)
	if len(r.Checkpoint) == 0 {
		r.Checkpoint = nil
	}
	if len(r.Result) == 0 {
		r.Result = nil
	}
	return json.Marshal(r)
}

type Detail struct {
	ID             string            `json:"job_id"`
	Type           Type              `json:"type"`
	State          State             `json:"state"`
	DryRun         bool              `json:"dry_run"`
	PolicyRevision uint64            `json:"policy_revision"`
	WorkerNodeID   string            `json:"worker_node_id,omitempty"`
	Progress       map[string]uint64 `json:"progress"`
	CreatedAt      time.Time         `json:"created_at"`
	StartedAt      *time.Time        `json:"started_at"`
	FinishedAt     *time.Time        `json:"finished_at"`
	Result         json.RawMessage   `json:"result"`
	Error          *Failure          `json:"error"`
}
type Failure struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (j Job) View() Detail {
	d := Detail{ID: j.ID, Type: j.Type, State: j.State, DryRun: j.DryRun, PolicyRevision: j.PolicyRevision, WorkerNodeID: j.WorkerNodeID, Progress: map[string]uint64{}, CreatedAt: j.CreatedAt, Result: append(json.RawMessage(nil), j.Result...)}
	for k, v := range j.Progress {
		d.Progress[k] = v
	}
	if !j.StartedAt.IsZero() {
		t := j.StartedAt
		d.StartedAt = &t
	}
	if !j.FinishedAt.IsZero() {
		t := j.FinishedAt
		d.FinishedAt = &t
	}
	if j.ErrorCode != "" {
		d.Error = &Failure{j.ErrorCode, j.ErrorCode}
	}
	return d
}

func document(p json.RawMessage, limit int, optional bool) bool {
	if len(p) == 0 {
		return optional
	}
	if optional && bytes.Equal(bytes.TrimSpace(p), []byte("null")) {
		return true
	}
	var object map[string]json.RawMessage
	return len(p) <= limit && json.Unmarshal(p, &object) == nil && object != nil
}
func bounded(v any) bool { b, e := json.Marshal(v); return e == nil && len(b) <= MaxRecordBytes }
func (j Job) Validate() error {
	if !ValidID(j.ID) || !j.Type.Valid() || !j.State.Valid() || j.PolicyRevision == 0 || j.PolicyRevision > storage.MaxPolicyRevision || j.Revision == 0 || j.Revision > storage.MaxPolicyRevision || j.CreatedAt.IsZero() || !document(j.Params, 8192, false) || !document(j.Checkpoint, 8192, true) || !document(j.Result, 64<<10, true) || len(j.Progress) > 64 || !bounded(j) {
		return storage.ErrInvalid
	}
	for key := range j.Progress {
		if !ValidID(key) {
			return storage.ErrInvalid
		}
	}
	if j.ErrorCode != "" && !ValidID(j.ErrorCode) {
		return storage.ErrInvalid
	}
	if j.State.Terminal() != !j.FinishedAt.IsZero() {
		return storage.ErrInvalid
	}
	if j.State == Queued && (j.WorkerNodeID != "" || j.Worker != (storage.WriterFence{}) || j.PendingStep != "") {
		return storage.ErrInvalid
	}
	if j.State != Queued && j.State != Cancelled && (!ValidID(j.WorkerNodeID) || !ValidID(j.WorkerNodeIncarnation) || j.Worker.LeaseID != j.ID || j.Worker.ServiceIncarnation == "" || j.Worker.Epoch == 0 || j.StartedAt.IsZero()) {
		return storage.ErrInvalid
	}
	if j.PendingStep != "" && (!ValidID(j.PendingStep) || j.State.Terminal()) {
		return storage.ErrInvalid
	}
	return nil
}

type Change struct {
	JobID                   string
	ExpectedRevision        uint64
	Kind                    string // claim, heartbeat, begin_step, checkpoint, finish, cancel
	NodeID, NodeIncarnation string
	Fence                   storage.WriterFence
	StepID                  string
	Checkpoint              json.RawMessage
	Progress                map[string]uint64
	Result                  json.RawMessage
	ErrorCode               string
}

func (c Change) WorkerChange() bool { return c.Kind != "cancel" }

// Apply is shared by all adapters and runs inside their CAS transaction, after
// checking the supplied worker fence in that SAME transaction. It performs no I/O.
func Apply(old Job, c Change, now time.Time) (Job, error) {
	if old.Validate() != nil || now.IsZero() {
		return Job{}, storage.ErrInvalid
	}
	if c.JobID != old.ID || c.ExpectedRevision != old.Revision {
		return Job{}, storage.ErrConflict
	}
	if c.Kind == "cancel" && old.State.Terminal() {
		return old, nil
	}
	if old.State.Terminal() {
		return Job{}, storage.ErrConflict
	}
	if c.WorkerChange() && (c.Fence.LeaseID != old.ID || c.Fence.ServiceIncarnation == "" || c.Fence.Epoch == 0 || !ValidID(c.NodeID) || !ValidID(c.NodeIncarnation)) {
		return Job{}, storage.ErrFenced
	}
	j := old
	switch c.Kind {
	case "claim":
		if j.State != Queued {
			if j.PendingStep != "" {
				return Job{}, ErrReconciliation
			}
			if c.Fence == j.Worker && c.NodeID == j.WorkerNodeID && c.NodeIncarnation == j.WorkerNodeIncarnation {
				return j, nil
			}
			if c.Fence.ServiceIncarnation != j.Worker.ServiceIncarnation || c.Fence.Epoch <= j.Worker.Epoch {
				return Job{}, storage.ErrFenced
			}
		}
		j.Worker, j.WorkerNodeID, j.WorkerNodeIncarnation = c.Fence, c.NodeID, c.NodeIncarnation
		if j.State == Queued {
			j.State = Running
			j.StartedAt = now.UTC()
		}
	case "heartbeat", "begin_step", "checkpoint", "finish":
		if j.Worker != c.Fence || j.WorkerNodeID != c.NodeID || j.WorkerNodeIncarnation != c.NodeIncarnation {
			return Job{}, storage.ErrFenced
		}
		switch c.Kind {
		case "heartbeat":
			j.HeartbeatAt = now.UTC()
		case "begin_step":
			if j.State != Running || j.PendingStep != "" || !ValidID(c.StepID) {
				return Job{}, storage.ErrConflict
			}
			j.PendingStep = c.StepID
		case "checkpoint":
			// The worker calls only AFTER its R1/R2 side effects are settled and
			// quiescent. Clearing this marker is not itself a physical flush proof.
			if c.StepID != j.PendingStep {
				return Job{}, storage.ErrConflict
			}
			if !document(c.Checkpoint, 8192, false) {
				return Job{}, storage.ErrInvalid
			}
			j.PendingStep = ""
			j.Checkpoint = append(json.RawMessage(nil), c.Checkpoint...)
			j.Progress = c.Progress
			if j.State == Cancelling {
				j.State = Cancelled
				j.FinishedAt = now.UTC()
			}
		case "finish":
			if j.PendingStep != "" {
				return Job{}, ErrReconciliation
			}
			if j.State == Cancelling {
				j.State = Cancelled
			} else if c.ErrorCode != "" {
				j.State = Failed
			} else {
				j.State = Succeeded
			}
			j.Result = append(json.RawMessage(nil), c.Result...)
			j.ErrorCode = c.ErrorCode
			j.FinishedAt = now.UTC()
		}
	case "cancel":
		if j.State == Cancelling {
			return j, nil
		}
		if j.State == Queued {
			j.State = Cancelled
			j.FinishedAt = now.UTC()
		} else {
			j.State = Cancelling
		}
	default:
		return Job{}, storage.ErrInvalid
	}
	if j.Revision == storage.MaxPolicyRevision {
		return Job{}, storage.ErrConflict
	}
	j.Revision++
	if j.Validate() != nil {
		return Job{}, storage.ErrInvalid
	}
	return j, nil
}

type Node struct {
	ID                    string    `json:"node_id"`
	Incarnation           string    `json:"incarnation"`
	ServiceIncarnation    string    `json:"service_incarnation"`
	Revision              uint64    `json:"revision"`
	Profile               string    `json:"profile"`
	Address               string    `json:"address"`
	HeartbeatAt           time.Time `json:"heartbeat_at"`
	Ready                 bool      `json:"ready"`
	Draining              bool      `json:"draining"`
	ActiveRequests        int64     `json:"active_requests"`
	AppliedPolicyRevision uint64    `json:"applied_policy_revision"`
}

func (n Node) Validate() error {
	a, e := netip.ParseAddrPort(n.Address)
	if !ValidID(n.ID) || !ValidID(n.Incarnation) || !ValidID(n.ServiceIncarnation) || n.Revision == 0 || n.Revision > storage.MaxPolicyRevision || (n.Profile != "cloud" && n.Profile != "edge") || e != nil || a.Port() != 9000 || (!a.Addr().IsPrivate() && !a.Addr().IsLoopback()) || n.HeartbeatAt.IsZero() || n.ActiveRequests < 0 || n.AppliedPolicyRevision > storage.MaxPolicyRevision || (n.Draining && n.Ready) || !bounded(n) {
		return storage.ErrInvalid
	}
	return nil
}

// Result is the bounded wire result for node/cluster operations. A missing,
// failed or timed-out node is explicit; membership never grows during a job.
type NodeResult struct {
	NodeID    string          `json:"node_id"`
	State     string          `json:"state"`
	ErrorCode string          `json:"error_code,omitempty"`
	Result    json.RawMessage `json:"result"`
}
type Result struct {
	Scope string       `json:"scope"`
	Nodes []NodeResult `json:"nodes"`
}

func (r Result) Validate() error {
	if (r.Scope != "node" && r.Scope != "cluster") || len(r.Nodes) == 0 || len(r.Nodes) > 1000 || (r.Scope == "node" && len(r.Nodes) != 1) || !bounded(r) {
		return storage.ErrInvalid
	}
	seen := map[string]bool{}
	for _, n := range r.Nodes {
		if !ValidID(n.NodeID) || seen[n.NodeID] || (n.State != "succeeded" && n.State != "failed" && n.State != "timeout" && n.State != "unavailable") || !document(n.Result, 8192, true) || (n.ErrorCode != "" && !ValidID(n.ErrorCode)) {
			return storage.ErrInvalid
		}
		seen[n.NodeID] = true
	}
	return nil
}

type PageRequest struct {
	Limit  int
	Cursor string
}

func (p PageRequest) Validate() error {
	if p.Limit < 1 || p.Limit > 1000 || (p.Cursor != "" && !ValidID(p.Cursor)) {
		return storage.ErrInvalid
	}
	return nil
}

type JobPage struct {
	Jobs []Job
	Next string
}
type NodePage struct {
	Nodes []Node
	Next  string
}

// Store is operational shared metadata, excluded from namespace recovery replay.
// Worker changes condition-check current R1 authority in the same transaction;
// cancellation never needs (or impersonates) the worker's authority.
type Store interface {
	CreateJob(context.Context, Job) error
	GetJob(context.Context, string) (Job, error)
	ChangeJob(context.Context, Change) (Job, error)
	ListJobs(context.Context, PageRequest) (JobPage, error)
	GetNode(context.Context, string) (Node, error)
	PutNode(context.Context, uint64, Node) (Node, error)
	ListNodes(context.Context, PageRequest) (NodePage, error)
}
