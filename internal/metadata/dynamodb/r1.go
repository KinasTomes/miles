package dynamodb

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"time"

	s "github.com/KinasTomes/miles/internal/storage"
)

// SharedAuthority consumes durable gate/lease rows via Observe so metadata
// publication serializes with incarnation changes and explicit lease closure.
// Expiry is dispatch permission, not a condition on the DynamoDB server clock.
type SharedAuthority struct {
	Time        s.R1TimeSource
	Lifetime    time.Duration
	Maintenance R1MaintenanceGuard
}

func r1GateKey() Key { return stringKey("SystemConfig", "config_id", "coordination/r1/current") }
func r1LeaseKey(id string) Key {
	k := stringKey("Leases", "resource_id", "r1/producer")
	k.Attributes["lease_id"] = text(id)
	return k
}
func r1PinKey(p s.PackRef) Key {
	k := stringKey("Leases", "resource_id", "r1/pack/"+p.ID)
	k.Attributes["lease_id"] = text(p.Generation)
	return k
}

type r1Pins struct {
	Version uint32
	Pack    s.PackRef
	Count   uint64
}

func (p r1Pins) Validate() error {
	if p.Version != s.ModelVersion || p.Pack.Validate() != nil || p.Count > s.MaxPolicyRevision {
		return s.ErrInvalid
	}
	return nil
}
func r1Decode[T any](item Item) (T, error) {
	v, err := decodeItem[T](item)
	if err != nil {
		return v, err
	}
	if checked, ok := any(v).(interface{ Validate() error }); ok {
		if checked.Validate() != nil {
			return v, s.ErrUnavailable
		}
	}
	return v, nil
}

func r1Read[T any](t *transaction, k Key) (T, error) {
	var zero T
	row, err := t.observe(k)
	if err != nil {
		return zero, err
	}
	if row.after == nil {
		return zero, s.ErrNotFound
	}
	return r1Decode[T](row.after)
}
func r1Put(t *transaction, k Key, v any) error {
	if checked, ok := v.(interface{ Validate() error }); ok {
		if err := checked.Validate(); err != nil {
			return err
		}
	}
	row, err := t.observe(k)
	if err != nil {
		return err
	}
	item, err := nativeItem(v)
	if err != nil {
		return err
	}
	for n, a := range k.Attributes {
		item[n] = a
	}
	item["record_kind"] = text("r1_coordination_v1")
	if err = ValidateItem(item); err != nil {
		return err
	}
	row.after = item
	row.dirty = true
	return nil
}
func (a *SharedAuthority) bounds(ctx context.Context) (s.R1Bounds, error) {
	if a == nil || a.Time == nil || a.Lifetime <= 0 || a.Lifetime > time.Hour {
		return s.R1Bounds{}, s.ErrFenced
	}
	b, err := a.Time.Bounds(ctx)
	if err != nil {
		return b, err
	}
	if b.Lower.IsZero() || b.Upper.IsZero() || b.Lower.After(b.Upper) || !b.Upper.Before(b.Horizon) {
		return b, s.ErrFenced
	}
	return b, nil
}
func r1Observed[T any](observe Observe, k Key) (T, error) {
	var zero T
	item, err := observe(k)
	if err != nil {
		return zero, err
	}
	if item == nil {
		return zero, s.ErrNotFound
	}
	return r1Decode[T](item)
}
func (a *SharedAuthority) checkLease(ctx context.Context, f s.WriterFence, observe Observe) error {
	g, err := r1Observed[s.R1Gate](observe, r1GateKey())
	if err != nil {
		return err
	}
	l, err := r1Observed[s.R1Lease](observe, r1LeaseKey(f.LeaseID))
	if err != nil {
		return err
	}
	// Reads may cross expiry. Sample dispatch time only after authority I/O.
	b, err := a.bounds(ctx)
	if err != nil {
		return err
	}
	if l.Fence != f || g.Incarnation != f.ServiceIncarnation || l.State != "Active" || !b.Upper.Before(l.ExpiresAt) || !b.Upper.Before(l.Deadline) || l.Deadline.After(b.Horizon) {
		return s.ErrFenced
	}
	return nil
}
func (a *SharedAuthority) Check(ctx context.Context, f s.WriterFence, observe Observe) error {
	if err := a.checkLease(ctx, f, observe); err != nil {
		return err
	}
	l, err := r1Observed[s.R1Lease](observe, r1LeaseKey(f.LeaseID))
	if err != nil {
		return err
	}
	if l.Kind != "writer" && l.Kind != "control" {
		return s.ErrFenced
	}
	return nil
}
func (a *SharedAuthority) CheckManifest(ctx context.Context, f s.WriterFence, id string, observe Observe) error {
	if err := a.Check(ctx, f, observe); err != nil {
		return err
	}
	l, err := r1Observed[s.R1Lease](observe, r1LeaseKey(f.LeaseID))
	if err != nil {
		return err
	}
	if l.Kind != "writer" || !slices.Contains(l.Roots, id) || id == "" {
		return s.ErrFenced
	}
	return nil
}
func (a *SharedAuthority) CheckMultipart(ctx context.Context, f s.WriterFence, id string, observe Observe) error {
	return a.CheckManifest(ctx, f, id, observe)
}
func (a *SharedAuthority) CheckProducer(ctx context.Context, c s.MutationContext, observe Observe) error {
	if c.Validate() != nil {
		return s.ErrInvalid
	}
	if err := a.Check(ctx, c.Writer, observe); err != nil {
		return err
	}
	g, err := r1Observed[s.R1Gate](observe, r1GateKey())
	if err != nil {
		return err
	}
	if c.HistoryID != g.HistoryID || c.Epoch != g.Epoch {
		return s.ErrFenced
	}
	l, err := r1Observed[s.R1Lease](observe, r1LeaseKey(c.Writer.LeaseID))
	if err != nil {
		return err
	}
	if l.OpID != c.LogicalOpID || l.Object != c.Object {
		return s.ErrFenced
	}
	return nil
}
func (m *Store) r1Authority() (*SharedAuthority, error) {
	a, ok := m.authority.(*SharedAuthority)
	if !ok || a == nil {
		return nil, s.ErrFenced
	}
	return a, nil
}
func (m *Store) r1Bounds(ctx context.Context) (s.R1Bounds, error) {
	a, err := m.r1Authority()
	if err != nil {
		return s.R1Bounds{}, err
	}
	return a.bounds(ctx)
}
func (m *Store) r1Check(t *transaction, f s.WriterFence) (s.R1Lease, error) {
	a, err := m.r1Authority()
	if err != nil {
		return s.R1Lease{}, err
	}
	if err := a.checkLease(t.ctx, f, r1Observe(t)); err != nil {
		return s.R1Lease{}, err
	}
	return r1Read[s.R1Lease](t, r1LeaseKey(f.LeaseID))
}

// InitializeR1 is provisioning-only. Never overwrite a running incarnation.
func (m *Store) InitializeR1(ctx context.Context, incarnation, history string, epoch uint64) error {
	if incarnation == "" || !mutationID.MatchString(history) || epoch > s.MaxPolicyRevision {
		return s.ErrInvalid
	}
	return m.update(ctx, func(t *transaction) error {
		g, err := r1Read[s.R1Gate](t, r1GateKey())
		if err == nil {
			if g.Incarnation == incarnation && g.HistoryID == history && g.Epoch == epoch {
				return nil
			}
			return s.ErrConflict
		}
		if !errors.Is(err, s.ErrNotFound) {
			return err
		}
		return r1Put(t, r1GateKey(), s.R1Gate{Version: s.ModelVersion, Incarnation: incarnation, HistoryID: history, Epoch: epoch, State: "Open", Revision: 1})
	})
}
func (m *Store) GetR1Gate(ctx context.Context) (s.R1Gate, error) {
	var g s.R1Gate
	err := m.view(ctx, func(t *transaction) error { var err error; g, err = r1Read[s.R1Gate](t, r1GateKey()); return err })
	return g, err
}
func (m *Store) GetR1Lease(ctx context.Context, id string) (s.R1Lease, error) {
	var l s.R1Lease
	err := m.view(ctx, func(t *transaction) error { var err error; l, err = r1Read[s.R1Lease](t, r1LeaseKey(id)); return err })
	return l, err
}
func (m *Store) SetR1Gate(ctx context.Context, revision uint64, state string) error {
	if state != "Open" && state != "Closing" {
		return s.ErrInvalid
	}
	return m.update(ctx, func(t *transaction) error {
		g, err := r1Read[s.R1Gate](t, r1GateKey())
		if err != nil {
			return err
		}
		if g.Revision != revision || g.State == "Maintenance" {
			return s.ErrConflict
		}
		g.State = state
		g.Revision++
		return r1Put(t, r1GateKey(), g)
	})
}
func (m *Store) RotateR1(ctx context.Context, revision uint64, next string) error {
	if next == "" {
		return s.ErrInvalid
	}
	return m.update(ctx, func(t *transaction) error {
		g, err := r1Read[s.R1Gate](t, r1GateKey())
		if err != nil {
			return err
		}
		if g.Revision != revision || g.Active != 0 || g.State != "Closing" || next == g.Incarnation {
			return s.ErrR1Busy
		}
		g.Incarnation = next
		g.Revision++
		return r1Put(t, r1GateKey(), g)
	})
}
func (m *Store) AcquireR1(ctx context.Context, r s.R1Acquire) (s.R1Lease, error) {
	var out s.R1Lease
	if r.Owner == "" || len(r.Owner) > 128 || len(r.OpID) > 64 || r.Fence.LeaseID == "" || len(r.Fence.LeaseID) > 128 || r.Fence.Epoch == 0 || r.Fence.Epoch > s.MaxPolicyRevision || r.Deadline.IsZero() || (r.Kind != "reader" && r.Kind != "writer" && r.Kind != "control") {
		return out, s.ErrInvalid
	}
	if r.Kind != "control" && r.Object.Validate() != nil {
		return out, s.ErrInvalid
	}
	err := m.update(ctx, func(t *transaction) error {
		b, err := m.r1Bounds(ctx)
		if err != nil {
			return err
		}
		g, err := r1Read[s.R1Gate](t, r1GateKey())
		if err != nil {
			return err
		}
		if g.Incarnation != r.Fence.ServiceIncarnation {
			return s.ErrFenced
		}
		old, err := r1Read[s.R1Lease](t, r1LeaseKey(r.Fence.LeaseID))
		if err == nil {
			if old.Fence == r.Fence {
				if old.Owner != r.Owner || old.Kind != r.Kind || old.Object != r.Object || old.OpID != r.OpID || !old.Deadline.Equal(r.Deadline) {
					return s.ErrConflict
				}
				out, err = m.r1Check(t, r.Fence)
				return err
			}
			if old.State != "Released" || r.Fence.Epoch != old.Fence.Epoch+1 {
				return s.ErrFenced
			}
		} else if !errors.Is(err, s.ErrNotFound) {
			return err
		} else if r.Fence.Epoch != 1 {
			return s.ErrFenced
		}
		if g.State != "Open" {
			return s.ErrR1Busy
		}
		if !b.Upper.Before(r.Deadline) || r.Deadline.After(b.Horizon) {
			return s.ErrFenced
		}
		a, _ := m.r1Authority()
		expires := b.Upper.Add(a.Lifetime)
		if r.Deadline.Before(expires) {
			expires = r.Deadline
		}
		out = s.R1Lease{Version: s.ModelVersion, Fence: r.Fence, Owner: r.Owner, Kind: r.Kind, OpID: r.OpID, Object: r.Object, Deadline: r.Deadline, ExpiresAt: expires, Revision: 1, State: "Active", Resolving: true, Pending: []string{}, Roots: []string{}}
		g.Active++
		g.Resolving++
		g.Revision++
		if err = r1Put(t, r1GateKey(), g); err != nil {
			return err
		}
		return r1Put(t, r1LeaseKey(r.Fence.LeaseID), out)
	})
	return out, err
}
func (m *Store) CheckR1(ctx context.Context, f s.WriterFence) (s.R1Lease, error) {
	var out s.R1Lease
	err := m.view(ctx, func(t *transaction) error { var err error; out, err = m.r1Check(t, f); return err })
	return out, err
}
func (m *Store) RenewR1(ctx context.Context, f s.WriterFence) (s.R1Lease, error) {
	var out s.R1Lease
	err := m.update(ctx, func(t *transaction) error {
		l, err := m.r1Check(t, f)
		if err != nil {
			return err
		}
		b, err := m.r1Bounds(ctx)
		if err != nil {
			return err
		}
		if !b.Upper.Before(l.ExpiresAt) || !b.Upper.Before(l.Deadline) {
			return s.ErrFenced
		}
		a, _ := m.r1Authority()
		next := b.Upper.Add(a.Lifetime)
		if l.Deadline.Before(next) {
			next = l.Deadline
		}
		if next.After(l.ExpiresAt) {
			l.ExpiresAt = next
			l.Revision++
		}
		out = l
		return r1Put(t, r1LeaseKey(f.LeaseID), l)
	})
	return out, err
}
func (m *Store) CloseR1(ctx context.Context, f s.WriterFence) error {
	return m.update(ctx, func(t *transaction) error {
		l, err := r1Read[s.R1Lease](t, r1LeaseKey(f.LeaseID))
		if err != nil {
			return err
		}
		if l.Fence != f {
			return s.ErrFenced
		}
		if l.State == "Closed" || l.State == "Released" {
			return nil
		}
		l.State = "Closed"
		l.Revision++
		return r1Put(t, r1LeaseKey(f.LeaseID), l)
	})
}

// ReclaimR1 closes old publication only after conservative expiry+grace. It
// deliberately does NOT release roots/pins or transfer unknown work authority.
func (m *Store) ReclaimR1(ctx context.Context, f s.WriterFence, grace time.Duration) error {
	if grace < time.Second {
		return s.ErrInvalid
	}
	return m.update(ctx, func(t *transaction) error {
		l, err := r1Read[s.R1Lease](t, r1LeaseKey(f.LeaseID))
		if err != nil {
			return err
		}
		if l.Fence != f {
			return s.ErrFenced
		}
		b, err := m.r1Bounds(ctx)
		if err != nil {
			return err
		}
		if b.Lower.Before(l.ExpiresAt.Add(grace)) {
			return s.ErrR1Busy
		}
		if l.State == "Released" || l.State == "Closed" {
			return nil
		}
		l.State = "Closed"
		l.Revision++
		return r1Put(t, r1LeaseKey(f.LeaseID), l)
	})
}
func (m *Store) BindR1(ctx context.Context, f s.WriterFence, h s.ObjectHead) error {
	return m.update(ctx, func(t *transaction) error {
		l, err := m.r1Check(t, f)
		if err != nil {
			return err
		}
		if h.ManifestID == "" || len(h.ManifestID) > 128 || h.Object.Validate() != nil {
			return s.ErrInvalid
		}
		if l.Object != h.Object {
			return s.ErrFenced
		}
		if l.Kind == "reader" && l.Root != "" && (l.Root != h.ManifestID || l.RootRevision != h.Revision) {
			return s.ErrConflict
		}
		if l.Kind == "reader" {
			if l.Object != h.Object {
				return s.ErrFenced
			}
			current, err := get[s.ObjectHead](t, headKey(h.Object))
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(current, h) {
				return s.ErrConflict
			}
		}
		if !slices.Contains(l.Roots, h.ManifestID) {
			if len(l.Roots) >= 64 {
				return s.ErrInvalid
			}
			l.Roots = append(l.Roots, h.ManifestID)
		}
		l.Root = h.ManifestID
		l.RootRevision = h.Revision
		l.Revision++
		return r1Put(t, r1LeaseKey(f.LeaseID), l)
	})
}
func (m *Store) ResolvedR1(ctx context.Context, f s.WriterFence) error {
	return m.update(ctx, func(t *transaction) error {
		l, err := m.r1Check(t, f)
		if err != nil {
			return err
		}
		if l.Root == "" {
			return s.ErrInvalid
		}
		if !l.Resolving {
			return nil
		}
		g, err := r1Read[s.R1Gate](t, r1GateKey())
		if err != nil {
			return err
		}
		if g.Resolving == 0 {
			return s.ErrUnavailable
		}
		g.Resolving--
		g.Revision++
		l.Resolving = false
		l.Revision++
		if err = r1Put(t, r1GateKey(), g); err != nil {
			return err
		}
		return r1Put(t, r1LeaseKey(f.LeaseID), l)
	})
}
func (m *Store) BeginR1Action(ctx context.Context, f s.WriterFence, id string) error {
	if id == "" || len(id) > 256 {
		return s.ErrInvalid
	}
	return m.update(ctx, func(t *transaction) error {
		l, err := m.r1Check(t, f)
		if err != nil {
			return err
		}
		if slices.Contains(l.Pending, id) {
			return s.ErrR1Busy
		}
		if len(l.Pending) >= 32 {
			return s.ErrInvalid
		}
		l.Pending = append(l.Pending, id)
		l.Revision++
		return r1Put(t, r1LeaseKey(f.LeaseID), l)
	})
}

// Settle is an internal trusted coordinator call after synchronous terminal
// result or authoritative reconciliation, not a timeout/missing-event inference.
func (m *Store) SettleR1Action(ctx context.Context, f s.WriterFence, id string) error {
	return m.update(ctx, func(t *transaction) error {
		l, err := r1Read[s.R1Lease](t, r1LeaseKey(f.LeaseID))
		if err != nil {
			return err
		}
		if l.Fence != f || l.State == "Released" {
			return s.ErrFenced
		}
		at := slices.Index(l.Pending, id)
		if at < 0 {
			return nil
		}
		l.Pending = slices.Delete(l.Pending, at, at+1)
		l.Revision++
		return r1Put(t, r1LeaseKey(f.LeaseID), l)
	})
}

var _ s.R1Store = (*Store)(nil)

func (t *transaction) revalidateR1() error {
	a, ok := t.store.authority.(*SharedAuthority)
	if !ok {
		return nil
	}
	for f := range t.r1Fences {
		if err := a.Check(t.ctx, f, r1Observe(t)); err != nil {
			return err
		}
	}
	return nil
}
