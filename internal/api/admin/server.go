package admin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/KinasTomes/miles/internal/auth"
	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/storage"
)

// Store is the metadata the Admin server reads and writes. Identity records
// change only through MutateControl; policies through PutPolicy and
// PutBucketPolicy, with the caller's token as actor.
type Store interface {
	storage.IdentityStore
	storage.BucketPolicyStore
	GetBucket(context.Context, string) (storage.Bucket, error)
	GetCredential(context.Context, string) (storage.Credential, error)
	GetQuota(context.Context, string) (storage.LogicalQuota, error)
	GetPolicy(context.Context, uint64) (storage.PolicySnapshot, error)
	PutPolicy(context.Context, storage.PolicyUpdate) (storage.PolicySnapshot, error)
	MutateControl(context.Context, storage.ControlMutation) (storage.MutationResult, error)
}

// Admit returns a fresh trusted mutation context for one write made by tokenID.
type Admit func(ctx context.Context, tokenID string) (storage.MutationContext, error)

type Options struct {
	Store   Store
	Admit   Admit
	Sealer  Sealer
	Cursors Keyring
	Clock   clock.Clock
	Logger  *slog.Logger // audit log; never bearer/secret/hash
}

type server struct {
	Options
	policy config.PolicyValidator
	tokens *auth.Cache[string, bearer]
}

// bearer is the authorization snapshot of a token (API §6.4).
type bearer struct {
	token storage.AdminToken
	user  storage.User
}

// principal is the authenticated caller of one request.
type principal struct {
	TokenID, UserID string
	Admin           bool
}

// NewServer serves the identity, grant and policy routes of API §6.2 under
// /admin/v1. Any other path is a plain 404; a wrong method on a served path
// is 405.
func NewServer(o Options) (http.Handler, error) {
	if o.Store == nil || o.Admit == nil || o.Sealer == nil || o.Clock == nil {
		return nil, errors.New("admin server: store, admit, sealer and clock are required")
	}
	if err := o.Cursors.Validate(); err != nil {
		return nil, err
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	v, err := config.NewPolicyValidator(config.DSGPolicyLimits())
	if err != nil {
		return nil, err
	}
	s := &server{Options: o, policy: v}
	s.tokens = auth.NewCache(o.Clock, func(ctx context.Context, id string) (bearer, time.Time, error) {
		t, err := o.Store.GetAdminToken(ctx, id)
		if err != nil {
			return bearer{}, time.Time{}, err
		}
		u, err := o.Store.GetUser(ctx, t.UserID)
		if err != nil {
			return bearer{}, time.Time{}, err
		}
		return bearer{t, u}, t.ExpiresAt, nil
	})
	mux := http.NewServeMux()
	for _, r := range []struct {
		pattern  string
		mutation bool
		h        func(*call) error
	}{
		{"POST /admin/v1/users", true, s.createUser},
		{"GET /admin/v1/users", false, s.listUsers},
		{"GET /admin/v1/users/{id}", false, s.getUser},
		{"PATCH /admin/v1/users/{id}", true, s.patchUser},
		{"GET /admin/v1/users/{id}/usage", false, s.usage},
		{"GET /admin/v1/buckets/{b}", false, s.getBucket},
		{"GET /admin/v1/buckets/{b}/grants", false, s.listGrants},
		{"PUT /admin/v1/buckets/{b}/grants/{user_id}", true, s.putGrant},
		{"DELETE /admin/v1/buckets/{b}/grants/{user_id}", true, s.deleteGrant},
		{"GET /admin/v1/policy", false, s.getPolicy},
		{"PUT /admin/v1/policy", true, s.putPolicy},
		{"GET /admin/v1/buckets/{b}/policy", false, s.getBucketPolicy},
		{"PUT /admin/v1/buckets/{b}/policy", true, s.putBucketPolicy},
		{"POST /admin/v1/credentials", true, s.createCredential},
		{"GET /admin/v1/credentials", false, s.listCredentials},
		{"DELETE /admin/v1/credentials/{access_key_id}", true, s.deleteCredential},
		{"POST /admin/v1/admin-tokens", true, s.createToken},
		{"GET /admin/v1/admin-tokens", false, s.listTokens},
		{"DELETE /admin/v1/admin-tokens/{token_id}", true, s.deleteToken},
	} {
		mux.Handle(r.pattern, s.handler(r.mutation, r.h))
	}
	return mux, nil
}

func (s *server) now() time.Time { return s.Clock.Now().WallUTC }

// call is one request in flight.
type call struct {
	w      http.ResponseWriter
	r      *http.Request
	id     string
	p      principal
	status int
}

func (s *server) handler(mutation bool, h func(*call) error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := &call{w: w, r: r, id: newRequestID()}
		w.Header().Set("X-Request-Id", c.id)
		err := s.authenticate(c)
		if err == nil {
			err = h(c)
		}
		if err != nil {
			s.fail(c, err)
		}
		if mutation {
			// The path names users, buckets, keys or token ids, never secrets.
			s.Logger.InfoContext(r.Context(), "admin mutation", "request_id", c.id, "principal", c.p.UserID,
				"action", r.Pattern, "resource", r.URL.Path, "outcome", c.status)
		}
	})
}

// authenticate checks the bearer (API §6.4) on every request: the secret
// against the snapshot hash, then token status, expiry and user status.
func (s *server) authenticate(c *call) error {
	id, secret, ok := ParseBearer(c.r.Header.Values("Authorization"))
	if !ok {
		return errUnauthorized
	}
	b, err := s.tokens.Get(c.r.Context(), id)
	switch {
	case errors.Is(err, storage.ErrNotFound) || errors.Is(err, auth.ErrExpiredDuringLoad):
		return errUnauthorized
	case err != nil:
		return err
	}
	if !VerifySecret(hex.EncodeToString(b.token.Hash), secret) || !b.token.Usable(s.now()) || !b.user.Active || b.token.UserID != b.user.ID {
		return errUnauthorized
	}
	c.p = principal{TokenID: id, UserID: b.user.ID, Admin: b.token.Scope == storage.TokenScopeSystem}
	return nil
}

func newRequestID() string {
	var b [16]byte
	rand.Read(b[:]) // crypto/rand never fails on supported platforms
	return hex.EncodeToString(b[:])
}

// apiError is an Admin error response (API §6.5.1).
type apiError struct {
	status        int
	code, message string
}

func (e *apiError) Error() string { return e.code + ": " + e.message }

func invalid(format string, args ...any) *apiError {
	return &apiError{http.StatusBadRequest, "invalid_request", fmt.Sprintf(format, args...)}
}

func notFound(code, message string) *apiError {
	return &apiError{http.StatusNotFound, code, message}
}

func conflict(code, message string) *apiError {
	return &apiError{http.StatusConflict, code, message}
}

var (
	errUnauthorized     = &apiError{http.StatusUnauthorized, "unauthorized", "bearer token is missing or not valid"}
	errForbidden        = &apiError{http.StatusForbidden, "forbidden", "caller lacks the role this route requires"}
	errBucketNotFound   = notFound("bucket_not_found", "bucket does not exist")
	errUserNotFound     = notFound("user_not_found", "user does not exist")
	errRevisionConflict = conflict("revision_conflict", "current revision differs from the precondition")
	errRevisionMax      = conflict("revision_exhausted", "revision is already the maximum")
	errBucketMismatch   = conflict("bucket_id_mismatch", "expected_bucket_id is not the current bucket_id of this bucket")
	errInvalidCursor    = &apiError{http.StatusBadRequest, "invalid_cursor", "cursor is not valid for this request"}
)

func (s *server) fail(c *call, err error) {
	var e *apiError
	switch {
	case errors.As(err, &e):
	case errors.Is(err, ErrPrecondition):
		e = invalid("%s", err.Error())
	case errors.Is(err, storage.ErrAccessDenied):
		// The token or its user changed after the snapshot; the write
		// transaction refused it.
		e = errUnauthorized
	case errors.Is(err, storage.ErrConflict):
		e = errRevisionConflict
	case errors.Is(err, storage.ErrInvalid):
		e = invalid("metadata rejected the request")
	case errors.Is(err, storage.ErrUnavailable), errors.Is(err, storage.ErrFenced),
		errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		e = &apiError{http.StatusServiceUnavailable, "unavailable", "metadata is unavailable; the outcome may be unknown"}
	default:
		s.Logger.ErrorContext(c.r.Context(), "admin request failed", "request_id", c.id, "err", err)
		e = &apiError{http.StatusInternalServerError, "internal_error", "internal error"}
	}
	if e.status == http.StatusUnauthorized {
		c.w.Header().Set("WWW-Authenticate", "Bearer")
	}
	type body struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
	}
	c.json(e.status, struct {
		Error body `json:"error"`
	}{body{e.code, e.message, c.id}})
}

func (c *call) json(status int, v any) {
	c.status = status
	c.w.Header().Set("Content-Type", "application/json")
	c.w.WriteHeader(status)
	_ = json.NewEncoder(c.w).Encode(v)
}

// ok answers 200 with v and the ETag of its revision.
func (c *call) ok(rev uint64, v any) error {
	c.w.Header().Set("ETag", ETag(rev))
	c.json(http.StatusOK, v)
	return nil
}

func (c *call) noContent() error {
	c.status = http.StatusNoContent
	c.w.WriteHeader(http.StatusNoContent)
	return nil
}

func (c *call) admin() error {
	if !c.p.Admin {
		return errForbidden
	}
	return nil
}

// admit returns the request context carrying a fresh mutation context.
func (s *server) admit(c *call) (context.Context, storage.MutationContext, error) {
	m, err := s.Admit(c.r.Context(), c.p.TokenID)
	if err != nil {
		return nil, m, err
	}
	return storage.WithMutation(c.r.Context(), m), m, nil
}

// mutate runs one control mutation as the caller and decodes the stored
// record into out.
func (s *server) mutate(c *call, m storage.ControlMutation, out any) error {
	_, mc, err := s.admit(c)
	if err != nil {
		return err
	}
	m.AdminTokenID = c.p.TokenID
	m.Context = mc
	res, err := s.Store.MutateControl(c.r.Context(), m)
	if err != nil {
		return err
	}
	if out != nil && json.Unmarshal(res.Value, out) != nil {
		return fmt.Errorf("%w: undecodable %s mutation result", storage.ErrUnavailable, m.Entity)
	}
	return nil
}

// page reads limit and cursor (API §6.5.3) next to the route's filters.
func (s *server) page(c *call, bucketID string, filters ...string) (storage.IdentityListRequest, url.Values, error) {
	q, err := query(c.r, append(filters, "limit", "cursor")...)
	if err != nil {
		return storage.IdentityListRequest{}, nil, err
	}
	req := storage.IdentityListRequest{Limit: 100}
	if v, ok := q["limit"]; ok {
		n, ok := jsonInteger([]byte(v[0]))
		if !ok || n < 1 || n > 1000 {
			return req, nil, invalid("query parameter \"limit\" must be an integer in 1..1000")
		}
		req.Limit = int(n)
	}
	if v, ok := q["cursor"]; ok {
		req.After, err = s.Cursors.Open(v[0], s.binding(c, bucketID, q), s.now())
		if err != nil {
			return req, nil, errInvalidCursor
		}
	}
	return req, q, nil
}

func (s *server) binding(c *call, bucketID string, q url.Values) Binding {
	return Binding{Principal: c.p.UserID, Resource: c.r.URL.Path, BucketID: bucketID, Filter: CanonicalFilter(q)}
}

// list answers a page; next_cursor only when the store has more.
func (s *server) list(c *call, bucketID string, q url.Values, items any, next string) error {
	out := struct {
		Items      any    `json:"items"`
		NextCursor string `json:"next_cursor,omitempty"`
	}{Items: items}
	if next != "" {
		cursor, err := s.Cursors.Seal(s.binding(c, bucketID, q), next, s.now())
		if err != nil {
			return err
		}
		out.NextCursor = cursor
	}
	c.json(http.StatusOK, out)
	return nil
}

func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339) }
