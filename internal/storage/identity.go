package storage

import (
	"context"
	"time"
)

// Admin token scopes (API v2 6.4). Bucket rights are never encoded in a token:
// they come from Buckets/BucketGrants at admission time.
const (
	TokenScopeSystem = "system-admin"
	TokenScopeUser   = "user"
)

// Usable reports an active, unexpired token. The caller still checks the user.
func (t AdminToken) Usable(now time.Time) bool { return t.Active && now.Before(t.ExpiresAt) }

// IdentityListRequest pages one identity table in byte order of its key:
// users by user_id, grants by user_id, credentials by access_key_id, tokens
// by token_id (API v2 6.5.3). After is the last key of the previous page
// ("" starts); UserID filters credentials and tokens ("" lists all).
type IdentityListRequest struct {
	After, UserID string
	Limit         int
}

func (r IdentityListRequest) Validate() error {
	if r.Limit < 1 || r.Limit > 1000 {
		return ErrInvalid
	}
	return nil
}

// IdentityPage holds at most Limit items; Next is the key of the last item
// and is set only when more items follow it.
type IdentityPage[T any] struct {
	Items []T
	Next  string
}

// IdentityStore reads the Admin identity records written by
// ProducerStore.MutateControl. Reads are consistent; lists are discovery
// pages, not snapshots. Missing records are ErrNotFound.
type IdentityStore interface {
	GetUser(context.Context, string) (User, error)
	ListUsers(context.Context, IdentityListRequest) (IdentityPage[User], error)
	// GetGrant returns revoked grants too: DELETE keeps a tombstone with
	// Read = Write = false so revisions never repeat. ListGrants skips them.
	GetGrant(ctx context.Context, bucketID, userID string) (Grant, error)
	ListGrants(ctx context.Context, bucketID string, r IdentityListRequest) (IdentityPage[Grant], error)
	ListCredentials(context.Context, IdentityListRequest) (IdentityPage[Credential], error)
	GetCredentialSecurity(context.Context, string) (CredentialSecurity, error)
	GetAdminToken(context.Context, string) (AdminToken, error)
	ListAdminTokens(context.Context, IdentityListRequest) (IdentityPage[AdminToken], error)
	// BootstrapAdmin is the offline first-install transaction (API v2 6.4).
	// It commits only while the installation marker is absent, otherwise it
	// writes nothing and returns ErrConflict.
	BootstrapAdmin(context.Context, AdminBootstrap) error
}

// AdminBootstrap installs the marker, the user (created active with
// QuotaLimit, or re-activated), the system token and, when no global policy
// exists yet, Policy as revision 1. Token.Revision must be zero.
type AdminBootstrap struct {
	Token      AdminToken
	QuotaLimit int64
	Policy     PolicySnapshot // complete validated document, Revision zero
}

func (b AdminBootstrap) Validate(now time.Time) error {
	t := b.Token
	if !identifier.MatchString(t.UserID) || !hex128(t.ID) || len(t.Hash) != 32 || t.Scope != TokenScopeSystem || !t.Active || t.Revision != 0 || !t.Usable(now) || t.IssuedAt.IsZero() || b.QuotaLimit < 0 || b.Policy.Version != ModelVersion || b.Policy.Revision != 0 || len(b.Policy.Document) == 0 {
		return ErrInvalid
	}
	return nil
}
