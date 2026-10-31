package storage

import (
	"context"
	"fmt"
	"time"
)

var ErrR1Busy = fmt.Errorf("%w: R1 admission or maintenance gate busy", ErrUnavailable)

// R1Bounds is supplied only by the trusted UTC adapter. Wall time alone must
// never be upgraded to this proof. Lower/Upper include error through Horizon.
type R1Bounds struct{ Lower, Upper, Horizon time.Time }
type R1TimeSource interface {
	Bounds(context.Context) (R1Bounds, error)
}
type R1Gate struct {
	Version                            uint32
	Incarnation, HistoryID, State      string
	Epoch, Revision, Active, Resolving uint64
	RecoveryVersion                    uint32
	RecoveryEnabled                    bool
	RecoveryCutID                      string
}
type R1Lease struct {
	Version             uint32
	Fence               WriterFence
	Owner, Kind, State  string
	OpID                string
	Object              ObjectKey
	Root                string
	Roots               []string
	RootRevision        uint64
	Deadline, ExpiresAt time.Time
	Revision            uint64
	Resolving           bool
	PinCount            uint64
	PinCursor           string
	Pending             []string
	HistoryID           string
	RecoveryEpoch       uint64
}
type R1Acquire struct {
	Fence       WriterFence
	Owner, Kind string
	OpID        string
	Object      ObjectKey
	Deadline    time.Time
}
type R1Maintenance struct {
	Version                        uint32
	ID, Incarnation, Action, State string
	Pack                           PackRef
	StateRevision                  uint64
	Target                         StorageClass
}

func (g R1Gate) Validate() error {
	if g.Version != ModelVersion || g.Incarnation == "" || len(g.Incarnation) > 128 || !hex128(g.HistoryID) || g.Epoch > MaxPolicyRevision || g.Revision == 0 || g.Revision > MaxPolicyRevision || g.Active > MaxPolicyRevision || g.Resolving > g.Active || g.RecoveryVersion > 1 || (g.State != "Open" && g.State != "Closing" && g.State != "Maintenance" && g.State != "RecoveryCut") {
		return ErrInvalid
	}
	if (g.State == "RecoveryCut") != (g.RecoveryCutID != "") || g.RecoveryCutID != "" && !hex128(g.RecoveryCutID) {
		return ErrInvalid
	}
	if g.RecoveryEnabled && g.RecoveryVersion != 1 || g.State == "RecoveryCut" && !g.RecoveryEnabled {
		return ErrInvalid
	}
	return nil
}
func (l R1Lease) Validate() error {
	if l.Version != ModelVersion || l.Fence.LeaseID == "" || l.Fence.ServiceIncarnation == "" || l.Fence.Epoch == 0 || l.Fence.Epoch > MaxPolicyRevision || l.Owner == "" || l.Revision == 0 || l.Revision > MaxPolicyRevision || l.Deadline.IsZero() || l.ExpiresAt.IsZero() || l.ExpiresAt.After(l.Deadline) || l.Deadline.Year() > 9999 || (l.Kind != "reader" && l.Kind != "writer" && l.Kind != "control" && l.Kind != "recovery") || (l.State != "Active" && l.State != "Closed" && l.State != "Released") || l.PinCount > MaxPolicyRevision || len(l.PinCursor) > 128 || len(l.Pending) > 32 || len(l.Roots) > 64 {
		return ErrInvalid
	}
	if l.Kind != "control" && l.Kind != "recovery" && l.Object.Validate() != nil {
		return ErrInvalid
	}
	if l.State == "Released" && (l.Resolving || len(l.Pending) > 0 || l.PinCount > 0 || l.PinCursor != "") {
		return ErrInvalid
	}
	return nil
}
func (r R1Maintenance) Validate() error {
	if r.Version != ModelVersion || r.ID == "" || r.Incarnation == "" || r.Pack.Validate() != nil || r.StateRevision == 0 || r.StateRevision >= MaxPolicyRevision-1 || (r.Action != "delete" && r.Action != "transition") || (r.State != "Pending" && r.State != "Unknown" && r.State != "Done") {
		return ErrInvalid
	}
	return nil
}

// R1Store owns shared CAS, not physical S3 I/O. Close removes publication rights
// but retains membership/pins. Release requires trusted caller quiescence and
// zero unresolved actions; it is never an expiry cleanup operation.
type R1Store interface {
	AcquireR1(context.Context, R1Acquire) (R1Lease, error)
	CheckR1(context.Context, WriterFence) (R1Lease, error)
	RenewR1(context.Context, WriterFence) (R1Lease, error)
	CloseR1(context.Context, WriterFence) error
	ReleaseR1(context.Context, WriterFence) error
	BindR1(context.Context, WriterFence, ObjectHead) error
	PinR1(context.Context, WriterFence, []PackRef) error
	ResolvedR1(context.Context, WriterFence) error
	BeginR1Action(context.Context, WriterFence, string) error
	SettleR1Action(context.Context, WriterFence, string) error
}
