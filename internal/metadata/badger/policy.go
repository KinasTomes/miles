package badger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"reflect"

	s "github.com/KinasTomes/miles/internal/storage"
	db "github.com/dgraph-io/badger/v4"
)

// Policy semantic comparison preserves exact JSON numbers, including >2^53.
func policyValue(v any) (any, error) {
	switch x := v.(type) {
	case json.Number:
		r, ok := new(big.Rat).SetString(string(x))
		if !ok {
			return nil, s.ErrInvalid
		}
		return r, nil
	case map[string]any:
		for k, c := range x {
			v, err := policyValue(c)
			if err != nil {
				return nil, err
			}
			x[k] = v
		}
	case []any:
		for i, c := range x {
			v, err := policyValue(c)
			if err != nil {
				return nil, err
			}
			x[i] = v
		}
	}
	return v, nil
}
func policyDocument(b []byte) (any, error) {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var v any
	if d.Decode(&v) != nil {
		return nil, s.ErrInvalid
	}
	if _, ok := v.(map[string]any); !ok {
		return nil, s.ErrInvalid
	}
	if d.Decode(new(any)) != io.EOF {
		return nil, s.ErrInvalid
	}
	return policyValue(v)
}
func policyKey(revision uint64) string { return fmt.Sprintf("policies/%016x", revision) }
func (m *Store) GetPolicy(ctx context.Context, revision uint64) (s.PolicySnapshot, error) {
	var out s.PolicySnapshot
	err := m.view(ctx, func(t *db.Txn) error {
		var err error
		if revision == 0 {
			revision, err = get[uint64](t, "policy-current")
			if err != nil {
				return err
			}
		}
		out, err = get[s.PolicySnapshot](t, policyKey(revision))
		return err
	})
	return out, err
}
func (m *Store) PutPolicy(ctx context.Context, r s.PolicyUpdate) (s.PolicySnapshot, error) {
	var out s.PolicySnapshot
	published := false
	err := m.update(ctx, func(t *db.Txn) error {
		if err := m.fence(t, r.Writer); err != nil {
			return err
		}
		c, err := optional[s.Credential](t, key("credentials", r.CredentialID))
		if err != nil {
			return err
		}
		if !c.Active || !c.SystemAdmin {
			return s.ErrAccessDenied
		}
		if err := activeUser(t, c.UserID); err != nil {
			return err
		}
		revision, err := optional[uint64](t, "policy-current")
		if err != nil {
			return err
		}
		if revision != r.ExpectedRevision {
			return s.ErrConflict
		}
		if r.Candidate.Version != 1 || r.Candidate.Revision != 0 {
			return s.ErrInvalid
		}
		candidate, err := policyDocument(r.Candidate.Document)
		if err != nil {
			return err
		}
		current, err := get[s.PolicySnapshot](t, policyKey(revision))
		if err == nil {
			old, err := policyDocument(current.Document)
			if err != nil {
				return err
			}
			if reflect.DeepEqual(candidate, old) {
				out = current
				return nil
			}
		} else if !errors.Is(err, s.ErrNotFound) {
			return err
		} else if revision != 0 {
			return s.ErrUnavailable
		}
		if revision >= s.MaxPolicyRevision {
			return s.ErrConflict
		}
		e := r.Event
		// PutPolicy only publishes a put event; a valid delete identity would
		// misrepresent the authoritative mutation to recovery consumers.
		if e == nil || e.Validate() != nil || e.Entity != "SystemConfig" || e.Action != "entity_put" || e.Revision != revision+1 {
			return s.ErrInvalid
		}
		found, err := exists(t, key("events", e.ID))
		if err != nil {
			return err
		}
		if found {
			return s.ErrConflict
		}
		found, err = exists(t, key("event-ops", e.OpID))
		if err != nil {
			return err
		}
		if found {
			return s.ErrConflict
		}
		out = r.Candidate
		out.Document = bytes.Clone(r.Candidate.Document)
		out.Revision = revision + 1
		if err := put(t, policyKey(out.Revision), out); err != nil {
			return err
		}
		if err := put(t, "policy-current", out.Revision); err != nil {
			return err
		}
		if err := put(t, key("events", e.ID), *e); err != nil {
			return err
		}
		if err := put(t, key("event-ops", e.OpID), e.ID); err != nil {
			return err
		}
		if m.beforePublish != nil {
			if err := m.beforePublish(); err != nil {
				return err
			}
		}
		published = true
		return nil
	})
	if err != nil {
		return s.PolicySnapshot{}, err
	}
	if published && m.afterPublish != nil {
		if err := m.afterPublish(); err != nil {
			return s.PolicySnapshot{}, err
		}
	}
	return out, nil
}
