package dynamodb

import (
	"context"
	"errors"

	s "github.com/KinasTomes/miles/internal/storage"
)

// GC/R2 owns reachability, protected cuts and write-ahead destructive intent.
// Checks MUST observe their authoritative rows through Observe so they are
// condition-checked in the same native transaction. Nil guard disables I/O.
type R1MaintenanceGuard interface {
	Prepare(context.Context, s.R1Maintenance, Observe) error
	Terminal(context.Context, s.R1Maintenance, Observe) error
}

func r1MaintenanceKey(id string) Key {
	return stringKey("SystemConfig", "config_id", "coordination/r1/action/"+id)
}
func r1Observe(t *transaction) Observe {
	return func(k Key) (Item, error) {
		r, err := t.observe(k)
		if err != nil {
			return nil, err
		}
		if r.after == nil {
			return nil, s.ErrNotFound
		}
		return copyItem(r.after), nil
	}
}
func (m *Store) GetR1Maintenance(ctx context.Context, id string) (s.R1Maintenance, error) {
	var out s.R1Maintenance
	err := m.view(ctx, func(t *transaction) error {
		var err error
		out, err = r1Read[s.R1Maintenance](t, r1MaintenanceKey(id))
		return err
	})
	return out, err
}
func (m *Store) BeginR1Maintenance(ctx context.Context, r s.R1Maintenance) (s.R1Maintenance, error) {
	var out s.R1Maintenance
	if r.ID == "" || len(r.ID) > 128 || r.Incarnation == "" || r.Pack.Validate() != nil || r.StateRevision == 0 || r.State != "" || (r.Action != "delete" && r.Action != "transition") {
		return out, s.ErrInvalid
	}
	if r.Action == "transition" && r.Target.Validate() != nil {
		return out, s.ErrInvalid
	}
	err := m.update(ctx, func(t *transaction) error {
		a, err := m.r1Authority()
		if err != nil {
			return err
		}
		if a.Maintenance == nil {
			return s.ErrR1Busy
		}
		if _, err = m.r1Bounds(ctx); err != nil {
			return err
		}
		g, err := r1Read[s.R1Gate](t, r1GateKey())
		if err != nil {
			return err
		}
		if g.Incarnation != r.Incarnation {
			return s.ErrFenced
		}
		if g.State != "Closing" || g.Active != 0 || g.Resolving != 0 {
			return s.ErrR1Busy
		}
		if _, err = r1Read[s.R1Maintenance](t, r1MaintenanceKey(r.ID)); err == nil {
			return s.ErrR1Busy
		} else if !errors.Is(err, s.ErrNotFound) {
			return err
		}
		pins, err := r1Read[r1Pins](t, r1PinKey(r.Pack))
		if err != nil && !errors.Is(err, s.ErrNotFound) {
			return err
		}
		if pins.Count != 0 {
			return s.ErrR1Busy
		}
		if err == nil && pins.Pack != r.Pack {
			return s.ErrUnavailable
		}
		p, err := get[s.PackInfo](t, packKey(r.Pack))
		if err != nil {
			return err
		}
		if p.StateRevision != r.StateRevision || (p.State != "Warm" && p.State != "Cold") {
			return s.ErrConflict
		}
		if r.Action == "transition" {
			if err := p.Class.ValidateTransition(r.Target); err != nil {
				return err
			}
		}
		r.Version = s.ModelVersion
		r.State = "Pending"
		if err = a.Maintenance.Prepare(ctx, r, r1Observe(t)); err != nil {
			return err
		}
		if r.Action == "delete" {
			p.State = "Deleting"
		} else {
			p.State = "Transitioning"
		}
		p.StateRevision++
		r.Version = s.ModelVersion
		r.State = "Pending"
		out = r
		g.State = "Maintenance"
		g.Revision++
		if err = put(t, packKey(r.Pack), p); err != nil {
			return err
		}
		if err = r1Put(t, r1GateKey(), g); err != nil {
			return err
		}
		return r1Put(t, r1MaintenanceKey(r.ID), r)
	})
	return out, err
}
func (m *Store) UnknownR1Maintenance(ctx context.Context, id string) error {
	return m.update(ctx, func(t *transaction) error {
		r, err := r1Read[s.R1Maintenance](t, r1MaintenanceKey(id))
		if err != nil {
			return err
		}
		if r.State == "Done" {
			return nil
		}
		if r.State != "Pending" && r.State != "Unknown" {
			return s.ErrInvalid
		}
		r.State = "Unknown"
		return r1Put(t, r1MaintenanceKey(id), r)
	})
}

// Finish does not equate HEAD existence/absence or timeout with terminal I/O.
// The trusted guard must prove original action settlement/isolation, not submit.
func (m *Store) FinishR1Maintenance(ctx context.Context, id string) error {
	return m.update(ctx, func(t *transaction) error {
		a, err := m.r1Authority()
		if err != nil {
			return err
		}
		if a.Maintenance == nil {
			return s.ErrR1Busy
		}
		r, err := r1Read[s.R1Maintenance](t, r1MaintenanceKey(id))
		if err != nil {
			return err
		}
		if r.State == "Done" {
			return nil
		}
		g, err := r1Read[s.R1Gate](t, r1GateKey())
		if err != nil {
			return err
		}
		if g.Incarnation != r.Incarnation || g.State != "Maintenance" {
			return s.ErrFenced
		}
		if err = a.Maintenance.Terminal(ctx, r, r1Observe(t)); err != nil {
			return err
		}
		p, err := get[s.PackInfo](t, packKey(r.Pack))
		if err != nil {
			return err
		}
		if p.StateRevision != r.StateRevision+1 {
			return s.ErrConflict
		}
		if r.Action == "delete" {
			if p.State != "Deleting" {
				return s.ErrConflict
			}
			p.State = "Retired"
		} else {
			if p.State != "Transitioning" {
				return s.ErrConflict
			}
			p.Class = r.Target
			p.State = "Cold"
		}
		p.StateRevision++
		r.State = "Done"
		g.State = "Closing"
		g.Revision++
		if err = put(t, packKey(r.Pack), p); err != nil {
			return err
		}
		if err = r1Put(t, r1GateKey(), g); err != nil {
			return err
		}
		return r1Put(t, r1MaintenanceKey(id), r)
	})
}
