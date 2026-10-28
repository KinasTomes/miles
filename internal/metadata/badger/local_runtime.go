package badger

import (
	"context"
	"errors"
	"strings"
	"time"

	s "github.com/KinasTomes/miles/internal/storage"
	db "github.com/dgraph-io/badger/v4"
)

type localRuntimeState struct {
	History, Incarnation, Owner string
}

// LocalAuthority relies on Badger's exclusive process lock. It is development
// admission, never a shared UTC lease, distributed epoch or recovery coordinator.
type LocalAuthority struct {
	store *Store
	state localRuntimeState
}

// StartLocalRuntime preserves history/quota and fences the previous process.
// An older bucket-only installation can be upgraded only while metadata is empty
// of object/upload records; unknown namespace/accounting must not become zero.
func (m *Store) StartLocalRuntime(ctx context.Context, owner string, quotaLimit int64) (*LocalAuthority, error) {
	if owner == "" || quotaLimit <= 0 || !m.requireProducers {
		return nil, s.ErrInvalid
	}
	incarnation, err := s.NewBucketID()
	if err != nil {
		return nil, err
	}
	var state localRuntimeState
	err = m.update(ctx, func(t *db.Txn) error {
		user, e := get[s.User](t, key("users", owner))
		if e != nil || !user.Active {
			return s.ErrAccessDenied
		}
		state, e = get[localRuntimeState](t, "local-runtime")
		if errors.Is(e, s.ErrNotFound) {
			for _, table := range []string{"heads", "uploads", "upload-record"} {
				if e = scan(t, key(table)+"/", func(string) error { return s.ErrUnavailable }); e != nil {
					return e
				}
			}
			history, e := s.NewBucketID()
			if e != nil {
				return e
			}
			state = localRuntimeState{History: history, Owner: owner}
			if e = scan(t, key("buckets")+"/", func(k string) error {
				b, err := get[s.Bucket](t, k)
				if err != nil {
					return err
				}
				if _, err = get[s.BucketNamespace](t, key("bucket-namespace", b.Name)); errors.Is(err, s.ErrNotFound) {
					return put(t, key("bucket-namespace", b.Name), s.BucketNamespace{Version: 1})
				}
				return err
			}); e != nil {
				return e
			}
		} else if e != nil {
			return e
		}
		if len(state.History) != 32 || state.Owner != owner {
			return s.ErrUnavailable
		}
		q, e := get[s.LogicalQuota](t, key("quotas", owner))
		if errors.Is(e, s.ErrNotFound) {
			// Missing quota after this runtime already wrote data is corruption.
			if state.Incarnation != "" {
				return s.ErrUnavailable
			}
			if e = put(t, key("quotas", owner), s.LogicalQuota{OwnerID: owner, Limit: quotaLimit}); e != nil {
				return e
			}
		} else if e != nil || q.OwnerID != owner || q.Limit <= 0 || q.Used < 0 || q.Reserved < 0 || q.Staged < 0 || q.Used > q.Limit || q.Reserved > q.Limit-q.Used {
			return s.ErrUnavailable
		}
		state.Incarnation = "local-" + incarnation
		return put(t, "local-runtime", state)
	})
	if err != nil {
		return nil, err
	}
	return &LocalAuthority{store: m, state: state}, nil
}

func (a *LocalAuthority) Incarnation() string { return a.state.Incarnation }

// Admit installs a fresh per-operation fence atomically after current identity
// and bucket checks. A zero object is only for the local CreateBucket adapter.
func (a *LocalAuthority) Admit(ctx context.Context, credential string, object s.ObjectKey, opID string, deadline time.Time) (s.MutationContext, error) {
	c := s.MutationContext{HistoryID: a.state.History, LogicalOpID: opID, MutationID: opID, Object: object,
		Writer: s.WriterFence{LeaseID: "local-" + opID, ServiceIncarnation: a.state.Incarnation, Epoch: 1}}
	if c.Validate() != nil || (object != (s.ObjectKey{}) && object.Validate() != nil) {
		return s.MutationContext{}, s.ErrInvalid
	}
	err := a.store.update(ctx, func(t *db.Txn) error {
		now := a.store.now()
		if !deadline.After(now) || deadline.After(now.Add(30*time.Minute)) {
			return s.ErrFenced
		}
		current, err := get[localRuntimeState](t, "local-runtime")
		if err != nil || current != a.state {
			return s.ErrFenced
		}
		cred, err := get[s.Credential](t, key("credentials", credential))
		if err != nil || !cred.Active || cred.UserID != a.state.Owner {
			return s.ErrAccessDenied
		}
		user, err := get[s.User](t, key("users", cred.UserID))
		if err != nil || !user.Active {
			return s.ErrAccessDenied
		}
		if object != (s.ObjectKey{}) {
			auth, err := a.store.authorize(t, credential, object.Bucket)
			if err != nil {
				return err
			}
			if auth.Bucket.ID != object.BucketID {
				return s.ErrBucketNotFound
			}
			if !auth.Write {
				return s.ErrAccessDenied
			}
		}
		found, err := exists(t, key("writers", c.Writer.LeaseID))
		if err != nil {
			return err
		}
		if found {
			return s.ErrConflict
		}
		return put(t, key("writers", c.Writer.LeaseID), s.WriterLease{Fence: c.Writer, ExpiresAt: deadline})
	})
	if err != nil {
		return s.MutationContext{}, err
	}
	return c, nil
}

func (m *Store) localFence(t *db.Txn, w s.WriterFence) error {
	if !strings.HasPrefix(w.LeaseID, "local-") {
		return nil
	}
	state, err := get[localRuntimeState](t, "local-runtime")
	if err != nil || state.Incarnation != w.ServiceIncarnation {
		return s.ErrFenced
	}
	return nil
}
