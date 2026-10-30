package admin

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"time"

	"github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/storage"
)

const (
	defaultLogicalLimit = 1 << 40 // 1 TiB (API §13.3)
	defaultTokenTTL     = 86400
	minTokenTTL         = 300
	maxTokenTTL         = 604800
)

var (
	accessKeyPattern = regexp.MustCompile(`^[A-Z0-9]{20}$`)
	statusPattern    = regexp.MustCompile(`^(active|disabled)$`)
)

// Users (API §6.5.4).

type userJSON struct {
	UserID        string `json:"user_id"`
	Status        string `json:"status"`
	DefaultDomain string `json:"default_domain"`
	LogicalLimit  int64  `json:"logical_limit"`
	Revision      uint64 `json:"revision"`
}

func newUserJSON(u storage.User, limit int64) userJSON {
	status, domain := "active", u.DefaultDomain
	if !u.Active {
		status = "disabled"
	}
	if domain == "" {
		domain = "default" // records seeded before default_domain existed
	}
	return userJSON{u.ID, status, domain, limit, u.Revision}
}

// quota reads the logical quota every user has since creation; a missing
// one is corrupt metadata, not a user without limit.
func (s *server) quota(c *call, id string) (storage.LogicalQuota, error) {
	q, err := s.Store.GetQuota(c.r.Context(), id)
	if errors.Is(err, storage.ErrNotFound) {
		return q, storage.ErrUnavailable
	}
	return q, err
}

// user reads user id with its quota; 404 user_not_found when missing. The
// two records are separate reads, but the limit only changes in a Users
// mutation that also bumps the user revision: re-reading the user after the
// quota and requiring the same revision makes the pair one coherent
// snapshot, so callers can decide a no-op on it.
func (s *server) user(c *call, id string) (storage.User, storage.LogicalQuota, error) {
	for range 3 {
		u, err := s.Store.GetUser(c.r.Context(), id)
		if errors.Is(err, storage.ErrNotFound) {
			return u, storage.LogicalQuota{}, errUserNotFound
		}
		if err != nil {
			return u, storage.LogicalQuota{}, err
		}
		q, err := s.quota(c, id)
		if err != nil {
			return u, q, err
		}
		again, err := s.Store.GetUser(c.r.Context(), id)
		if err != nil {
			return u, q, err
		}
		if again.Revision == u.Revision {
			return u, q, nil
		}
	}
	// ponytail: bounded retry under a burst of concurrent PATCHes; the caller retries.
	return storage.User{}, storage.LogicalQuota{}, storage.ErrUnavailable
}

func pathID(c *call, name string) (string, error) {
	id := c.r.PathValue(name)
	if !idPattern.MatchString(id) {
		return "", invalid("path parameter %q does not match %s", name, idPattern)
	}
	return id, nil
}

func (s *server) createUser(c *call) error {
	if err := c.admin(); err != nil {
		return err
	}
	o, err := decodeObject(c.w, c.r, "user_id", "default_domain", "logical_limit")
	if err != nil {
		return err
	}
	id, err := o.str("user_id", "", true, idPattern)
	if err != nil {
		return err
	}
	domain, err := o.str("default_domain", "default", false, idPattern)
	if err != nil {
		return err
	}
	limit, err := o.integer("logical_limit", defaultLogicalLimit, 0, MaxRevision)
	if err != nil {
		return err
	}
	exists := conflict("user_exists", "user already exists")
	if _, err := s.Store.GetUser(c.r.Context(), id); err == nil {
		return exists
	} else if !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	var u storage.User
	err = s.mutate(c, storage.ControlMutation{Entity: "Users", ID: id, User: storage.User{ID: id, Active: true, DefaultDomain: domain}, SetQuota: true, QuotaLimit: int64(limit)}, &u)
	if errors.Is(err, storage.ErrConflict) {
		return exists
	}
	if err != nil {
		return err
	}
	c.w.Header().Set("ETag", ETag(u.Revision))
	c.json(http.StatusCreated, newUserJSON(u, int64(limit)))
	return nil
}

func (s *server) listUsers(c *call) error {
	if err := c.admin(); err != nil {
		return err
	}
	req, q, err := s.page(c, "")
	if err != nil {
		return err
	}
	page, err := s.Store.ListUsers(c.r.Context(), req)
	if err != nil {
		return err
	}
	items := make([]userJSON, 0, len(page.Items))
	for _, u := range page.Items {
		q, err := s.quota(c, u.ID)
		if err != nil {
			return err
		}
		items = append(items, newUserJSON(u, q.Limit))
	}
	return s.list(c, "", q, items, page.Next)
}

func (s *server) getUser(c *call) error {
	if err := c.admin(); err != nil {
		return err
	}
	id, err := pathID(c, "id")
	if err != nil {
		return err
	}
	if _, err := query(c.r); err != nil {
		return err
	}
	u, q, err := s.user(c, id)
	if err != nil {
		return err
	}
	return c.ok(u.Revision, newUserJSON(u, q.Limit))
}

func (s *server) patchUser(c *call) error {
	if err := c.admin(); err != nil {
		return err
	}
	id, err := pathID(c, "id")
	if err != nil {
		return err
	}
	o, err := decodeObject(c.w, c.r, "status", "default_domain", "logical_limit", "expected_revision")
	if err != nil {
		return err
	}
	if !o.has("status") && !o.has("default_domain") && !o.has("logical_limit") {
		return invalid("at least one of \"status\", \"default_domain\", \"logical_limit\" is required")
	}
	status, err := o.str("status", "", false, statusPattern)
	if err != nil {
		return err
	}
	domain, err := o.str("default_domain", "", false, idPattern)
	if err != nil {
		return err
	}
	limit, err := o.integer("logical_limit", 0, 0, MaxRevision)
	if err != nil {
		return err
	}
	expected, err := o.optionalRevision()
	if err != nil {
		return err
	}
	rev, err := Precondition(expected, c.r.Header.Values("If-Match"), false)
	if err != nil {
		return err
	}
	u, q, err := s.user(c, id)
	if err != nil {
		return err
	}
	if u.Revision != rev {
		return errRevisionConflict
	}
	next := u
	if next.DefaultDomain == "" {
		next.DefaultDomain = "default"
	}
	if status != "" {
		next.Active = status == "active"
	}
	if domain != "" {
		next.DefaultDomain = domain
	}
	setQuota := o.has("logical_limit") && int64(limit) != q.Limit
	if next == u && !setQuota {
		return c.ok(u.Revision, newUserJSON(u, q.Limit))
	}
	if rev == MaxRevision {
		return errRevisionMax
	}
	if setQuota {
		q.Limit = int64(limit)
	}
	var out storage.User
	err = s.mutate(c, storage.ControlMutation{Entity: "Users", ID: id, ExpectedRevision: rev, User: next, SetQuota: setQuota, QuotaLimit: q.Limit}, &out)
	if errors.Is(err, storage.ErrQuotaExceeded) {
		// Checked against the ledger in the write transaction; nothing changed.
		return invalid("logical_limit is below the bytes the user already uses plus reserves")
	}
	if err != nil {
		return err
	}
	return c.ok(out.Revision, newUserJSON(out, q.Limit))
}

func (s *server) usage(c *call) error {
	id := c.r.PathValue("id")
	if !c.p.Admin && c.p.UserID != id {
		return errForbidden
	}
	if !idPattern.MatchString(id) {
		return invalid("path parameter \"id\" does not match %s", idPattern)
	}
	q, err := query(c.r, "detail")
	if err != nil {
		return err
	}
	detail := false
	if v, ok := q["detail"]; ok {
		if v[0] != "true" && v[0] != "false" {
			return invalid("query parameter \"detail\" must be true or false")
		}
		detail = v[0] == "true"
	}
	_, quota, err := s.user(c, id)
	if err != nil {
		return err
	}
	type usage struct {
		UserID        string `json:"user_id"`
		Used          int64  `json:"bytes_logical_used"`
		Reserved      int64  `json:"bytes_logical_reserved"`
		Limit         int64  `json:"bytes_logical_limit"`
		Staged        *int64 `json:"bytes_logical_staged,omitempty"`
		ActiveUploads *int   `json:"active_upload_count,omitempty"`
	}
	out := usage{UserID: id, Used: quota.Used, Reserved: quota.Reserved, Limit: quota.Limit}
	if detail {
		out.Staged, out.ActiveUploads = &quota.Staged, &quota.ActiveUploads
	}
	c.json(http.StatusOK, out)
	return nil
}

// Buckets, grants and bucket policy: owner or admin (API §2.2.3).

// bucket resolves {b} (404 bucket_not_found) then the owner-or-admin role.
func (s *server) bucket(c *call) (storage.Bucket, error) {
	b, err := s.Store.GetBucket(c.r.Context(), c.r.PathValue("b"))
	if errors.Is(err, storage.ErrNotFound) {
		return b, errBucketNotFound
	}
	if err != nil {
		return b, err
	}
	if !c.p.Admin && b.OwnerID != c.p.UserID {
		return b, errForbidden
	}
	return b, nil
}

// bucketChanged explains a write refused on the bucket incarnation: the
// name is gone (404) or names another bucket_id (409); nil if neither.
func (s *server) bucketChanged(c *call, b storage.Bucket) error {
	now, err := s.Store.GetBucket(c.r.Context(), b.Name)
	switch {
	case errors.Is(err, storage.ErrNotFound):
		return errBucketNotFound
	case err != nil:
		return err
	case now.ID != b.ID:
		return errBucketMismatch
	}
	return nil
}

func (s *server) getBucket(c *call) error {
	b, err := s.bucket(c)
	if err != nil {
		return err
	}
	if _, err := query(c.r); err != nil {
		return err
	}
	var cacheRev uint64
	p, err := s.Store.GetBucketPolicy(c.r.Context(), b.Name, b.ID, 0)
	switch {
	case err == nil:
		cacheRev = p.Revision
	case errors.Is(err, storage.ErrBucketNotFound) || errors.Is(err, storage.ErrConflict):
		return s.orBucketChanged(c, b, err)
	case !errors.Is(err, storage.ErrNotFound):
		return err
	}
	state := "active"
	if !b.Active {
		state = "deleting"
	}
	return c.ok(b.Revision, struct {
		Bucket              string `json:"bucket"`
		BucketID            string `json:"bucket_id"`
		OwnerID             string `json:"owner_id"`
		DedupDomain         string `json:"dedup_domain"`
		State               string `json:"state"`
		Revision            uint64 `json:"revision"`
		CachePolicyRevision uint64 `json:"cache_policy_revision"`
		CreatedAt           string `json:"created_at"`
	}{b.Name, b.ID, b.OwnerID, b.Domain, state, b.Revision, cacheRev, rfc3339(b.CreatedAt)})
}

// orBucketChanged maps a bucket incarnation error of a store call.
func (s *server) orBucketChanged(c *call, b storage.Bucket, err error) error {
	if errors.Is(err, storage.ErrNotFound) || errors.Is(err, storage.ErrConflict) {
		if changed := s.bucketChanged(c, b); changed != nil {
			return changed
		}
	}
	return err
}

type grantJSON struct {
	Bucket   string `json:"bucket"`
	BucketID string `json:"bucket_id"`
	UserID   string `json:"user_id"`
	Read     bool   `json:"read"`
	Write    bool   `json:"write"`
	Revision uint64 `json:"revision"`
}

func (s *server) listGrants(c *call) error {
	b, err := s.bucket(c)
	if err != nil {
		return err
	}
	req, q, err := s.page(c, b.ID)
	if err != nil {
		return err
	}
	page, err := s.Store.ListGrants(c.r.Context(), b.ID, req)
	if err != nil {
		return err
	}
	items := make([]grantJSON, 0, len(page.Items))
	for _, g := range page.Items {
		items = append(items, grantJSON{b.Name, g.BucketID, g.UserID, g.Read, g.Write, g.Revision})
	}
	return s.list(c, b.ID, q, items, page.Next)
}

// grant reads the grant of user on b; a tombstone (read = write = false)
// is no grant but keeps its revision for the next CAS.
func (s *server) grant(c *call, b storage.Bucket, user string) (g storage.Grant, live bool, err error) {
	g, err = s.Store.GetGrant(c.r.Context(), b.ID, user)
	if errors.Is(err, storage.ErrNotFound) {
		return storage.Grant{BucketID: b.ID, UserID: user}, false, nil
	}
	return g, err == nil && (g.Read || g.Write), err
}

func (s *server) putGrant(c *call) error {
	b, err := s.bucket(c)
	if err != nil {
		return err
	}
	user, err := pathID(c, "user_id")
	if err != nil {
		return err
	}
	o, err := decodeObject(c.w, c.r, "read", "write", "expected_revision", "expected_bucket_id")
	if err != nil {
		return err
	}
	read, err := o.boolean("read", false)
	if err != nil {
		return err
	}
	write, err := o.boolean("write", false)
	if err != nil {
		return err
	}
	bucketID, err := o.str("expected_bucket_id", "", true, idPattern)
	if err != nil {
		return err
	}
	expected, err := o.optionalRevision()
	if err != nil {
		return err
	}
	rev, err := Precondition(expected, c.r.Header.Values("If-Match"), true)
	if err != nil {
		return err
	}
	if o.has("read") && o.has("write") && !read && !write {
		return invalid("read and write are both false; revoke with DELETE")
	}
	if user == b.OwnerID {
		return invalid("user_id is the bucket owner, whose rights are implicit")
	}
	if _, err := s.Store.GetUser(c.r.Context(), user); errors.Is(err, storage.ErrNotFound) {
		return errUserNotFound
	} else if err != nil {
		return err
	}
	if bucketID != b.ID {
		return errBucketMismatch
	}
	g, live, err := s.grant(c, b, user)
	if err != nil {
		return err
	}
	if live && g.Revision != rev || !live && rev != 0 {
		return errRevisionConflict
	}
	next := g
	if !live {
		next.Read, next.Write = false, false
	}
	if o.has("read") {
		next.Read = read
	}
	if o.has("write") {
		next.Write = write
	}
	if !next.Read && !next.Write {
		return invalid("read and write are both false; revoke with DELETE")
	}
	if g.Revision == MaxRevision {
		if live && next == g {
			return c.ok(g.Revision, grantJSON{b.Name, b.ID, user, g.Read, g.Write, g.Revision})
		}
		return errRevisionMax
	}
	var out storage.Grant
	err = s.mutate(c, storage.ControlMutation{Entity: "BucketGrants", ID: user, ParentID: b.ID, ExpectedRevision: g.Revision,
		Bucket: storage.Bucket{Name: b.Name}, Grant: storage.Grant{BucketID: b.ID, UserID: user, Read: next.Read, Write: next.Write}}, &out)
	if errors.Is(err, storage.ErrBucketNotFound) {
		return s.orBucketChanged(c, b, err)
	}
	if errors.Is(err, storage.ErrNotFound) {
		return errUserNotFound
	}
	if err != nil {
		return err
	}
	return c.ok(out.Revision, grantJSON{b.Name, b.ID, user, out.Read, out.Write, out.Revision})
}

func (s *server) deleteGrant(c *call) error {
	b, err := s.bucket(c)
	if err != nil {
		return err
	}
	user, err := pathID(c, "user_id")
	if err != nil {
		return err
	}
	q, err := query(c.r, "expected_bucket_id")
	if err != nil {
		return err
	}
	bucketID := q.Get("expected_bucket_id")
	if !idPattern.MatchString(bucketID) {
		return invalid("query parameter \"expected_bucket_id\" is required and must match %s", idPattern)
	}
	g, live, err := s.grant(c, b, user)
	if err != nil {
		return err
	}
	if !live {
		return notFound("grant_not_found", "grant does not exist")
	}
	if bucketID != b.ID {
		return errBucketMismatch
	}
	if g.Revision == MaxRevision {
		return errRevisionMax
	}
	err = s.mutate(c, storage.ControlMutation{Entity: "BucketGrants", ID: user, ParentID: b.ID, ExpectedRevision: g.Revision, Deleted: true,
		Bucket: storage.Bucket{Name: b.Name}, Grant: storage.Grant{BucketID: b.ID, UserID: user}}, nil)
	if errors.Is(err, storage.ErrBucketNotFound) {
		return s.orBucketChanged(c, b, err)
	}
	if err != nil {
		return err
	}
	return c.noContent()
}

// Global policy (clock-and-policy §4.2).

func (s *server) currentPolicy(c *call) (config.GlobalSnapshot, error) {
	p, err := s.Store.GetPolicy(c.r.Context(), 0)
	if errors.Is(err, storage.ErrNotFound) {
		return config.GlobalSnapshot{}, storage.ErrUnavailable // bootstrap writes revision 1
	}
	if err != nil {
		return config.GlobalSnapshot{}, err
	}
	g, err := s.policy.DecodeGlobal(p)
	if err != nil {
		return g, storage.ErrUnavailable
	}
	return g, nil
}

func policyJSON(g config.GlobalSnapshot) any {
	return struct {
		Revision uint64 `json:"revision"`
		config.GlobalPolicy
	}{g.Revision(), g.Policy()}
}

func (s *server) getPolicy(c *call) error {
	if err := c.admin(); err != nil {
		return err
	}
	if _, err := query(c.r); err != nil {
		return err
	}
	g, err := s.currentPolicy(c)
	if err != nil {
		return err
	}
	return c.ok(g.Revision(), policyJSON(g))
}

// policyPrecondition resolves the CAS revision of a policy body; the config
// merge then owns every policy field.
func policyPrecondition(c *call) ([]byte, object, uint64, error) {
	data, err := readBody(c.w, c.r)
	if err != nil {
		return nil, nil, 0, err
	}
	o, err := parseObject(data)
	if err != nil {
		return nil, nil, 0, err
	}
	expected, err := o.optionalRevision()
	if err != nil {
		return nil, nil, 0, err
	}
	rev, err := Precondition(expected, c.r.Header.Values("If-Match"), false)
	return data, o, rev, err
}

// mergeError maps a config merge failure.
func mergeError(err error) error {
	switch {
	case errors.Is(err, config.ErrPolicyRevisionExhausted):
		return errRevisionMax
	case errors.Is(err, config.ErrPolicyConflict):
		return errRevisionConflict
	case errors.Is(err, config.ErrPolicyInvalid):
		return invalid("%s", err.Error())
	}
	return err
}

func (s *server) putPolicy(c *call) error {
	if err := c.admin(); err != nil {
		return err
	}
	data, _, rev, err := policyPrecondition(c)
	if err != nil {
		return err
	}
	current, err := s.currentPolicy(c)
	if err != nil {
		return err
	}
	p, err := s.policy.MergeGlobal(current, data, rev)
	if err != nil {
		return mergeError(err)
	}
	if !p.Changed() {
		return c.ok(current.Revision(), policyJSON(current))
	}
	candidate, err := p.Candidate()
	if err != nil {
		return err
	}
	ctx, m, err := s.admit(c)
	if err != nil {
		return err
	}
	eventID, err := storage.EventID(m.HistoryID, m.MutationID, "SystemConfig", "entity_put")
	if err != nil {
		return err
	}
	// The backend journal replaces pointer and digest with the produced payload.
	event := storage.OutboxEvent{Version: 1, ID: eventID, HistoryID: m.HistoryID, Entity: "SystemConfig", EntityID: "global-policy", Action: "entity_put",
		Revision: rev + 1, OpID: m.MutationID, Epoch: m.Epoch, PayloadPointer: "backend", PayloadSHA256: hexZero64}
	stored, err := s.Store.PutPolicy(ctx, storage.PolicyUpdate{Candidate: candidate, ExpectedRevision: rev, AdminTokenID: c.p.TokenID, Writer: m.Writer, Event: &event})
	if err != nil {
		return err
	}
	g, err := s.policy.DecodeGlobal(stored)
	if err != nil {
		return storage.ErrUnavailable
	}
	return c.ok(g.Revision(), policyJSON(g))
}

const hexZero64 = "0000000000000000000000000000000000000000000000000000000000000000"

// Bucket cache policy (clock-and-policy §4.3).

func cacheDocument(p config.CachePolicy) storage.PolicySnapshot {
	doc, _ := json.Marshal(struct {
		Cache config.CachePolicy `json:"cache"`
	}{p}) // two scalar fields always marshal
	return storage.PolicySnapshot{Version: storage.ModelVersion, Document: doc}
}

func (s *server) putCache(c *call, b storage.Bucket, expected uint64, p config.CachePolicy) (storage.PolicySnapshot, error) {
	ctx, m, err := s.admit(c)
	if err != nil {
		return storage.PolicySnapshot{}, err
	}
	out, err := s.Store.PutBucketPolicy(ctx, storage.BucketPolicyUpdate{Bucket: b.Name, BucketID: b.ID, AdminTokenID: c.p.TokenID,
		ExpectedRevision: expected, Candidate: cacheDocument(p), Writer: m.Writer})
	if err != nil {
		return out, s.orBucketChanged(c, b, err)
	}
	return out, nil
}

// firstCache is the bucket policy a bucket without one starts from:
// revision 1 = config.DefaultCachePolicy.
func (s *server) firstCache(b storage.Bucket) (config.BucketSnapshot, error) {
	return s.policy.NewBucketSnapshot(b.ID, 1, config.DefaultCachePolicy())
}

// bucketPolicy reads the current cache policy of b. A bucket without one
// reports missing with firstCache; materialize writes that revision 1
// first, under the caller's token like any bucket policy write.
func (s *server) bucketPolicy(c *call, b storage.Bucket, materialize bool) (snap config.BucketSnapshot, missing bool, err error) {
	p, err := s.Store.GetBucketPolicy(c.r.Context(), b.Name, b.ID, 0)
	if errors.Is(err, storage.ErrNotFound) && !errors.Is(err, storage.ErrBucketNotFound) {
		if !materialize {
			snap, err = s.firstCache(b)
			return snap, true, err
		}
		p, err = s.putCache(c, b, 0, config.DefaultCachePolicy())
		if errors.Is(err, storage.ErrConflict) {
			p, err = s.Store.GetBucketPolicy(c.r.Context(), b.Name, b.ID, 0)
		}
	}
	if err != nil {
		return snap, false, s.orBucketChanged(c, b, err)
	}
	snap, err = s.policy.DecodeBucket(b.ID, p.Revision, p.Document)
	if err != nil {
		return snap, false, storage.ErrUnavailable
	}
	return snap, false, nil
}

func bucketPolicyJSON(b storage.Bucket, p config.BucketSnapshot) any {
	return struct {
		Bucket   string             `json:"bucket"`
		BucketID string             `json:"bucket_id"`
		Revision uint64             `json:"revision"`
		Cache    config.CachePolicy `json:"cache"`
	}{b.Name, b.ID, p.Revision(), p.Cache()}
}

func (s *server) getBucketPolicy(c *call) error {
	b, err := s.bucket(c)
	if err != nil {
		return err
	}
	if _, err := query(c.r); err != nil {
		return err
	}
	p, _, err := s.bucketPolicy(c, b, true)
	if err != nil {
		return err
	}
	return c.ok(p.Revision(), bucketPolicyJSON(b, p))
}

func (s *server) putBucketPolicy(c *call) error {
	b, err := s.bucket(c)
	if err != nil {
		return err
	}
	data, o, rev, err := policyPrecondition(c)
	if err != nil {
		return err
	}
	named, err := o.str("expected_bucket_id", "", false, idPattern)
	if err != nil {
		return err
	}
	// Schema first (400), then bucket_id (409), then revision (409): the
	// merge runs against the current policy, or the first-use default
	// in memory, so a refused request writes nothing.
	current, missing, err := s.bucketPolicy(c, b, false)
	if err != nil {
		return err
	}
	p, err := s.policy.MergeBucket(current, data, rev)
	if errors.Is(err, config.ErrPolicyConflict) && named != b.ID {
		return errBucketMismatch
	}
	if err != nil {
		return mergeError(err)
	}
	if missing {
		// A concurrent first use wrote revision 1 already; the CAS below
		// decides against it.
		if _, err := s.putCache(c, b, 0, config.DefaultCachePolicy()); err != nil && !errors.Is(err, storage.ErrConflict) {
			return err
		}
	}
	if !p.Changed() && rev == MaxRevision {
		return c.ok(current.Revision(), bucketPolicyJSON(b, current))
	}
	out, err := s.putCache(c, b, rev, p.Cache())
	if err != nil {
		return err
	}
	snap, err := s.policy.DecodeBucket(b.ID, out.Revision, out.Document)
	if err != nil {
		return storage.ErrUnavailable
	}
	return c.ok(snap.Revision(), bucketPolicyJSON(b, snap))
}

// Credentials (API §2.2.3, §6.5.4).

type credentialJSON struct {
	AccessKeyID  string `json:"access_key_id"`
	UserID       string `json:"user_id"`
	Status       string `json:"status"`
	SystemAdmin  bool   `json:"system_admin"`
	DebugAllowed bool   `json:"debug_allowed"`
	CreatedAt    string `json:"created_at"`
}

func newCredentialJSON(v storage.Credential) credentialJSON {
	status := "active"
	if !v.Active {
		status = "revoked"
	}
	return credentialJSON{v.ID, v.UserID, status, v.SystemAdmin, v.DebugAllowed, rfc3339(v.CreatedAt)}
}

// newAccessKeyID draws 20 characters uniformly from [A-Z0-9].
func newAccessKeyID() (string, error) {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	out := make([]byte, 0, 20)
	var buf [32]byte
	for len(out) < 20 {
		if _, err := rand.Read(buf[:]); err != nil {
			return "", err
		}
		for _, b := range buf {
			if b < 252 && len(out) < 20 { // 252 = 7 * 36: no modulo bias
				out = append(out, alphabet[b%36])
			}
		}
	}
	return string(out), nil
}

// createAttempts bounds retries on a colliding random id.
const createAttempts = 3

func (s *server) createCredential(c *call) error {
	if err := c.admin(); err != nil {
		return err
	}
	o, err := decodeObject(c.w, c.r, "user_id", "system_admin", "debug_allowed")
	if err != nil {
		return err
	}
	user, err := o.str("user_id", "", true, idPattern)
	if err != nil {
		return err
	}
	admin, err := o.boolean("system_admin", false)
	if err != nil {
		return err
	}
	debug, err := o.boolean("debug_allowed", false)
	if err != nil {
		return err
	}
	if debug && !admin {
		return invalid("debug_allowed requires system_admin")
	}
	if _, err := s.Store.GetUser(c.r.Context(), user); errors.Is(err, storage.ErrNotFound) {
		return errUserNotFound
	} else if err != nil {
		return err
	}
	var raw [30]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return err
	}
	secret := base64.StdEncoding.EncodeToString(raw[:])
	for range createAttempts {
		id, err := newAccessKeyID()
		if err != nil {
			return err
		}
		sec, err := s.Sealer.Seal(c.r.Context(), id, []byte(secret))
		if err != nil {
			return err
		}
		cred := storage.Credential{ID: id, UserID: user, Active: true, SystemAdmin: admin, DebugAllowed: debug, CreatedAt: s.now().Truncate(time.Second)}
		var out struct{ Credential storage.Credential }
		err = s.mutate(c, storage.ControlMutation{Entity: "Credentials", ID: id, Credential: cred, Security: sec}, &out)
		if errors.Is(err, storage.ErrConflict) {
			continue
		}
		if errors.Is(err, storage.ErrNotFound) {
			return errUserNotFound
		}
		if err != nil {
			return err
		}
		c.json(http.StatusCreated, struct {
			credentialJSON
			SecretAccessKey string `json:"secret_access_key"`
		}{newCredentialJSON(out.Credential), secret})
		return nil
	}
	return storage.ErrConflict
}

func (s *server) listCredentials(c *call) error {
	if err := c.admin(); err != nil {
		return err
	}
	req, q, err := s.page(c, "", "user_id")
	if err != nil {
		return err
	}
	if v, ok := q["user_id"]; ok {
		if !idPattern.MatchString(v[0]) {
			return invalid("query parameter \"user_id\" does not match %s", idPattern)
		}
		req.UserID = v[0]
	}
	page, err := s.Store.ListCredentials(c.r.Context(), req)
	if err != nil {
		return err
	}
	items := make([]credentialJSON, 0, len(page.Items))
	for _, v := range page.Items {
		items = append(items, newCredentialJSON(v))
	}
	return s.list(c, "", q, items, page.Next)
}

func (s *server) deleteCredential(c *call) error {
	if err := c.admin(); err != nil {
		return err
	}
	id := c.r.PathValue("access_key_id")
	if !accessKeyPattern.MatchString(id) {
		return invalid("path parameter \"access_key_id\" does not match %s", accessKeyPattern)
	}
	if _, err := query(c.r); err != nil {
		return err
	}
	cred, err := s.Store.GetCredential(c.r.Context(), id)
	if errors.Is(err, storage.ErrNotFound) {
		return notFound("credential_not_found", "credential does not exist")
	}
	if err != nil {
		return err
	}
	if !cred.Active {
		return c.noContent() // revoked is terminal
	}
	if cred.Revision == MaxRevision {
		return errRevisionMax
	}
	if err := s.mutate(c, storage.ControlMutation{Entity: "Credentials", ID: id, ExpectedRevision: cred.Revision, Deleted: true, Credential: cred}, nil); err != nil {
		return err
	}
	return c.noContent()
}

// Admin tokens (API §6.4).

type tokenJSON struct {
	TokenID     string `json:"token_id"`
	UserID      string `json:"user_id"`
	SystemAdmin bool   `json:"system_admin"`
	Status      string `json:"status"`
	IssuedAt    string `json:"issued_at"`
	ExpiresAt   string `json:"expires_at"`
}

func (s *server) createToken(c *call) error {
	if err := c.admin(); err != nil {
		return err
	}
	o, err := decodeObject(c.w, c.r, "user_id", "system_admin", "expires_in_seconds")
	if err != nil {
		return err
	}
	user, err := o.str("user_id", "", true, idPattern)
	if err != nil {
		return err
	}
	admin, err := o.boolean("system_admin", false)
	if err != nil {
		return err
	}
	ttl, err := o.integer("expires_in_seconds", defaultTokenTTL, minTokenTTL, maxTokenTTL)
	if err != nil {
		return err
	}
	if _, err := s.Store.GetUser(c.r.Context(), user); errors.Is(err, storage.ErrNotFound) {
		return errUserNotFound
	} else if err != nil {
		return err
	}
	scope := storage.TokenScopeUser
	if admin {
		scope = storage.TokenScopeSystem
	}
	for range createAttempts {
		id, secret, token, err := NewToken()
		if err != nil {
			return err
		}
		hash := sha256.Sum256([]byte(secret))
		now := s.now().Truncate(time.Second)
		t := storage.AdminToken{ID: id, UserID: user, Scope: scope, Hash: hash[:], Active: true, IssuedAt: now, ExpiresAt: now.Add(time.Duration(ttl) * time.Second)}
		err = s.mutate(c, storage.ControlMutation{Entity: "AdminTokens", ID: id, Token: t}, nil)
		if errors.Is(err, storage.ErrConflict) {
			continue
		}
		if errors.Is(err, storage.ErrNotFound) {
			return errUserNotFound
		}
		if err != nil {
			return err
		}
		c.json(http.StatusCreated, struct {
			TokenID     string `json:"token_id"`
			Token       string `json:"token"`
			UserID      string `json:"user_id"`
			SystemAdmin bool   `json:"system_admin"`
			ExpiresAt   string `json:"expires_at"`
			RequestID   string `json:"request_id"`
		}{id, token, user, admin, rfc3339(t.ExpiresAt), c.id})
		return nil
	}
	return storage.ErrConflict
}

func (s *server) listTokens(c *call) error {
	if err := c.admin(); err != nil {
		return err
	}
	req, q, err := s.page(c, "", "user_id")
	if err != nil {
		return err
	}
	if v, ok := q["user_id"]; ok {
		if !idPattern.MatchString(v[0]) {
			return invalid("query parameter \"user_id\" does not match %s", idPattern)
		}
		req.UserID = v[0]
	}
	page, err := s.Store.ListAdminTokens(c.r.Context(), req)
	if err != nil {
		return err
	}
	items := make([]tokenJSON, 0, len(page.Items))
	for _, t := range page.Items {
		status := "active"
		if !t.Active {
			status = "revoked"
		}
		items = append(items, tokenJSON{t.ID, t.UserID, t.Scope == storage.TokenScopeSystem, status, rfc3339(t.IssuedAt), rfc3339(t.ExpiresAt)})
	}
	return s.list(c, "", q, items, page.Next)
}

// deleteToken looks the token up before the role check (API §6.5.1 step
// 3): a non-admin learns nothing about tokens that are not its own.
func (s *server) deleteToken(c *call) error {
	id := c.r.PathValue("token_id")
	var t storage.AdminToken
	err := storage.ErrNotFound
	if validTokenID(id) {
		t, err = s.Store.GetAdminToken(c.r.Context(), id)
	}
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	if !c.p.Admin && (err != nil || t.UserID != c.p.UserID) {
		return errForbidden
	}
	if !validTokenID(id) {
		return invalid("path parameter \"token_id\" must be 32 lowercase hex")
	}
	if _, err := query(c.r); err != nil {
		return err
	}
	if err != nil {
		return notFound("token_not_found", "admin token does not exist")
	}
	if !t.Active {
		return c.noContent() // revoked is terminal
	}
	if t.Revision == MaxRevision {
		return errRevisionMax
	}
	if err := s.mutate(c, storage.ControlMutation{Entity: "AdminTokens", ID: id, ExpectedRevision: t.Revision, Deleted: true, Token: t}, nil); err != nil {
		return err
	}
	return c.noContent()
}
