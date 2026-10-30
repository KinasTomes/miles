package badger

import (
	"context"
	"encoding/hex"
	"errors"
	"time"

	s "github.com/KinasTomes/miles/internal/storage"
	db "github.com/dgraph-io/badger/v4"
)

var _ s.IdentityStore = (*Store)(nil)

const adminInstallationKey = "admin-installation"

type adminInstallation struct {
	TokenID, UserID string
	CreatedAt       time.Time
}

func (m *Store) GetUser(ctx context.Context, id string) (s.User, error) {
	v, err := read[s.User](m, ctx, key("users", id))
	if err == nil && v.ID != id {
		err = s.ErrUnavailable
	}
	return v, err
}

func (m *Store) GetGrant(ctx context.Context, bucketID, userID string) (s.Grant, error) {
	v, err := read[s.Grant](m, ctx, key("grants", bucketID, userID))
	if err == nil && (v.BucketID != bucketID || v.UserID != userID) {
		err = s.ErrUnavailable
	}
	return v, err
}

func (m *Store) GetCredentialSecurity(ctx context.Context, id string) (s.CredentialSecurity, error) {
	return read[s.CredentialSecurity](m, ctx, key("credential-security", id))
}

func (m *Store) GetAdminToken(ctx context.Context, id string) (s.AdminToken, error) {
	v, err := read[s.AdminToken](m, ctx, key("admin-tokens", id))
	if err == nil && v.ID != id {
		err = s.ErrUnavailable
	}
	return v, err
}

func (m *Store) ListUsers(ctx context.Context, r s.IdentityListRequest) (s.IdentityPage[s.User], error) {
	return listIdentity(m, ctx, key("users")+"/", r, func(v s.User) (string, bool) { return v.ID, true })
}

func (m *Store) ListGrants(ctx context.Context, bucketID string, r s.IdentityListRequest) (s.IdentityPage[s.Grant], error) {
	if bucketID == "" {
		return s.IdentityPage[s.Grant]{}, s.ErrInvalid
	}
	return listIdentity(m, ctx, key("grants", bucketID)+"/", r, func(v s.Grant) (string, bool) {
		if v.BucketID != bucketID {
			return "", true // fails the key check: corrupt record
		}
		return v.UserID, v.Read || v.Write
	})
}

func (m *Store) ListCredentials(ctx context.Context, r s.IdentityListRequest) (s.IdentityPage[s.Credential], error) {
	return listIdentity(m, ctx, key("credentials")+"/", r, func(v s.Credential) (string, bool) {
		return v.ID, r.UserID == "" || v.UserID == r.UserID
	})
}

func (m *Store) ListAdminTokens(ctx context.Context, r s.IdentityListRequest) (s.IdentityPage[s.AdminToken], error) {
	return listIdentity(m, ctx, key("admin-tokens")+"/", r, func(v s.AdminToken) (string, bool) {
		return v.ID, r.UserID == "" || v.UserID == r.UserID
	})
}

// listIdentity walks prefix+hex(id) keys in byte order. keep returns the
// record id (checked against its key) and whether the record is listed.
func listIdentity[T any](m *Store, ctx context.Context, prefix string, r s.IdentityListRequest, keep func(T) (string, bool)) (s.IdentityPage[T], error) {
	out := s.IdentityPage[T]{Items: []T{}}
	if err := r.Validate(); err != nil {
		return out, err
	}
	err := m.view(ctx, func(t *db.Txn) error {
		options := db.DefaultIteratorOptions
		options.Prefix = []byte(prefix)
		it := t.NewIterator(options)
		defer it.Close()
		start := prefix
		if r.After != "" {
			start += hex.EncodeToString([]byte(r.After))
		}
		last := ""
		for it.Seek([]byte(start)); it.ValidForPrefix(options.Prefix); it.Next() {
			if err := ctx.Err(); err != nil {
				return err
			}
			k := string(it.Item().Key())
			if r.After != "" && k == start {
				continue
			}
			v, err := get[T](t, k)
			if err != nil {
				return err
			}
			id, listed := keep(v)
			if prefix+hex.EncodeToString([]byte(id)) != k {
				return s.ErrUnavailable
			}
			if !listed {
				continue
			}
			if len(out.Items) == r.Limit {
				out.Next = last
				return nil
			}
			out.Items = append(out.Items, v)
			last = id
		}
		return nil
	})
	if err != nil {
		return s.IdentityPage[T]{}, err
	}
	return out, nil
}

// BootstrapAdmin commits marker, user, token and missing policy together.
// It emits no outbox event: there is no mutation context offline; the R2
// epoch-0 export (W04-PHONG-04) owns publishing this installation state.
func (m *Store) BootstrapAdmin(ctx context.Context, b s.AdminBootstrap) error {
	now := m.now()
	if err := b.Validate(now); err != nil {
		return err
	}
	return m.update(ctx, func(t *db.Txn) error {
		for _, k := range []string{adminInstallationKey, key("admin-tokens", b.Token.ID)} {
			found, err := exists(t, k)
			if err != nil {
				return err
			}
			if found {
				return s.ErrConflict
			}
		}
		user, err := get[s.User](t, key("users", b.Token.UserID))
		switch {
		case errors.Is(err, s.ErrNotFound):
			user = s.User{ID: b.Token.UserID, Active: true, Revision: 1, DefaultDomain: "default"}
			if err := put(t, key("quotas", user.ID), s.LogicalQuota{OwnerID: user.ID, Limit: b.QuotaLimit}); err != nil {
				return err
			}
		case err != nil:
			return err
		case user.ID != b.Token.UserID || user.Revision >= s.MaxPolicyRevision:
			return s.ErrUnavailable
		case !user.Active:
			user.Active = true
			user.Revision++
		}
		if err := put(t, key("users", user.ID), user); err != nil {
			return err
		}
		token := b.Token
		token.Revision = 1
		if err := put(t, key("admin-tokens", token.ID), token); err != nil {
			return err
		}
		current, err := optional[uint64](t, "policy-current")
		if err != nil {
			return err
		}
		if current == 0 {
			policy := b.Policy
			policy.Revision = 1
			if err := put(t, policyKey(1), policy); err != nil {
				return err
			}
			if err := put(t, "policy-current", uint64(1)); err != nil {
				return err
			}
		}
		return put(t, adminInstallationKey, adminInstallation{TokenID: token.ID, UserID: user.ID, CreatedAt: now})
	})
}
