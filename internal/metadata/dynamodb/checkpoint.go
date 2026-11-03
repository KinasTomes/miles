package dynamodb

import (
	"context"
	"errors"
	"reflect"

	s "github.com/KinasTomes/miles/internal/storage"
)

func recoveryCatalogKey(history string) Key {
	return stringKey("SystemConfig", "config_id", "recovery/catalog/"+history)
}
func (m *Store) GetRecoveryCatalog(ctx context.Context, history string) (out s.RecoveryCatalogHead, err error) {
	if !mutationID.MatchString(history) {
		return out, s.ErrInvalid
	}
	err = m.view(ctx, func(t *transaction) error {
		var e error
		out, e = r1Read[s.RecoveryCatalogHead](t, recoveryCatalogKey(history))
		return e
	})
	return
}
func (m *Store) CompareRecoveryCatalog(ctx context.Context, old, next s.RecoveryCatalogHead, worker s.WriterFence) error {
	if next.Version != 1 || !mutationID.MatchString(next.History) || next.Incarnation != worker.ServiceIncarnation || next.Revision != old.Revision+1 || next.Revision > s.MaxPolicyRevision || next.Length <= 0 || next.Length > 256<<10 || next.Key == "" || !validRecoveryDigest(next.SHA256) {
		return s.ErrInvalid
	}
	return m.update(ctx, func(t *transaction) error {
		if err := m.recoveryFence(t, worker, true); err != nil {
			return err
		}
		g, e := r1Read[s.R1Gate](t, r1GateKey())
		if e != nil {
			return e
		}
		if g.HistoryID != next.History || g.Incarnation != next.Incarnation {
			return s.ErrFenced
		}
		current, e := r1Read[s.RecoveryCatalogHead](t, recoveryCatalogKey(next.History))
		if errors.Is(e, s.ErrNotFound) {
			current = s.RecoveryCatalogHead{}
		} else if e != nil {
			return e
		}
		if reflect.DeepEqual(current, next) {
			return nil
		}
		if !reflect.DeepEqual(current, old) {
			return s.ErrConflict
		}
		return r1Put(t, recoveryCatalogKey(next.History), next)
	})
}

var _ s.RecoveryCatalogStore = (*Store)(nil)
