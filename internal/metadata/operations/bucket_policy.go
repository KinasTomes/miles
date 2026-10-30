package operations

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/KinasTomes/miles/internal/config"
	s "github.com/KinasTomes/miles/internal/storage"
)

func BucketPolicyIdentity(id string) string { return s.BucketPolicyEntityID(id) }
func bucketPolicyRevision(r uint64) string  { return fmt.Sprintf("%016x", r) }

// ReadBucketPolicy runs inside a consistent backend view, binding the name to
// its incarnation even for historical reads. No defaults or current fallback.
func ReadBucketPolicy(t Tx, bucket, id string, revision uint64) (s.PolicySnapshot, error) {
	if bucket == "" || id == "" || revision > s.MaxPolicyRevision {
		return s.PolicySnapshot{}, s.ErrInvalid
	}
	b, err := get[s.Bucket](t, "buckets", bucket)
	if err != nil {
		return s.PolicySnapshot{}, err
	}
	if !b.Active {
		return s.PolicySnapshot{}, s.ErrBucketNotFound
	}
	if b.ID != id {
		return s.PolicySnapshot{}, s.ErrConflict
	}
	if revision == 0 {
		revision, err = get[uint64](t, "bucket-cache-current", id)
		if err != nil {
			return s.PolicySnapshot{}, err
		}
	}
	p, err := get[s.PolicySnapshot](t, "bucket-cache-history", id, bucketPolicyRevision(revision))
	if err != nil {
		return s.PolicySnapshot{}, err
	}
	if p.Version != s.ModelVersion || p.Revision != revision {
		return s.PolicySnapshot{}, s.ErrUnavailable
	}
	return p, nil
}

// WriteBucketPolicy shares business rules, not persistence. The adapter validates
// its writer authority before entering this function. History/current/producer
// payload/outbox are committed together by the backend's transaction engine.
func WriteBucketPolicy(t Tx, ctx context.Context, now time.Time, r s.BucketPolicyUpdate) (s.PolicySnapshot, error) {
	var zero s.PolicySnapshot
	if r.Bucket == "" || r.BucketID == "" || r.ExpectedRevision > s.MaxPolicyRevision || r.Candidate.Version != s.ModelVersion || r.Candidate.Revision != 0 || (r.CredentialID == "") == (r.AdminTokenID == "") {
		return zero, s.ErrInvalid
	}
	if err := bucketPolicyActor(t, now, r); err != nil {
		return zero, err
	}
	c, admitted := s.MutationFromContext(ctx)
	if !admitted || c.Writer != r.Writer {
		return zero, s.ErrFenced
	}
	if err := t.Admit(c); err != nil {
		return zero, err
	}
	v, err := config.NewPolicyValidator(config.DSGPolicyLimits())
	if err != nil {
		return zero, err
	}
	validated, err := v.DecodeBucket(r.BucketID, 1, r.Candidate.Document)
	if err != nil {
		return zero, err
	}
	// Persist canonical complete bytes, without repairing missing fields.
	doc, err := json.Marshal(struct {
		Cache config.CachePolicy `json:"cache"`
	}{validated.Cache()})
	if err != nil {
		return zero, err
	}
	identity := BucketPolicyIdentity(r.BucketID)
	// Include expected revision and incarnation in the idempotency digest.
	input := r
	input.Candidate.Document = doc
	oldResult, err := Replay(t, ctx, "SystemConfig", "entity_put", identity, input)
	if err == nil {
		if json.Unmarshal(oldResult.Value, &zero) != nil {
			return s.PolicySnapshot{}, s.ErrUnavailable
		}
		return zero, nil
	}
	if !errors.Is(err, s.ErrNotFound) {
		return zero, err
	}
	current, err := get[uint64](t, "bucket-cache-current", r.BucketID)
	if err != nil && !errors.Is(err, s.ErrNotFound) {
		return zero, err
	}
	if current != r.ExpectedRevision {
		return zero, s.ErrConflict
	}
	if current > 0 {
		old, err := ReadBucketPolicy(t, r.Bucket, r.BucketID, current)
		if err != nil {
			return zero, err
		}
		previous, err := v.DecodeBucket(r.BucketID, current, old.Document)
		if err != nil {
			return zero, s.ErrUnavailable
		}
		if previous.Cache() == validated.Cache() {
			if err := NoOp(t, ctx, "SystemConfig", identity, input, old); err != nil {
				return zero, err
			}
			return old, nil
		}
	}
	if current >= s.MaxPolicyRevision {
		return zero, s.ErrConflict
	}
	if _, err := get[s.PolicySnapshot](t, "bucket-cache-history", r.BucketID, bucketPolicyRevision(current+1)); err == nil {
		return zero, s.ErrUnavailable
	} else if !errors.Is(err, s.ErrNotFound) {
		return zero, err
	}
	out := s.PolicySnapshot{Version: s.ModelVersion, Revision: current + 1, Document: bytes.Clone(doc)}
	// Journal requires an admitted mutation even when the store is in dev mode.
	if _, err := Journal(t, ctx, true, r.Writer, "SystemConfig", "entity_put", identity, out.Revision, input, out); err != nil {
		return zero, err
	}
	if err := put(t, "bucket-cache-history", out, r.BucketID, bucketPolicyRevision(out.Revision)); err != nil {
		return zero, err
	}
	if err := put(t, "bucket-cache-current", out.Revision, r.BucketID); err != nil {
		return zero, err
	}
	return out, nil
}

// bucketPolicyActor allows the bucket owner or a system admin and binds the
// name to r.BucketID (ErrConflict on another incarnation).
func bucketPolicyActor(t Tx, now time.Time, r s.BucketPolicyUpdate) error {
	if r.CredentialID != "" {
		a, err := t.Authorize(r.CredentialID, r.Bucket)
		if err != nil {
			return err
		}
		if !a.SystemAdmin && a.UserID != a.Bucket.OwnerID {
			return s.ErrAccessDenied
		}
		if a.Bucket.ID != r.BucketID {
			return s.ErrConflict
		}
		return nil
	}
	token, err := TokenActor(t, now, r.AdminTokenID)
	if err != nil {
		return err
	}
	b, err := get[s.Bucket](t, "buckets", r.Bucket)
	if errors.Is(err, s.ErrNotFound) || err == nil && !b.Active {
		return s.ErrBucketNotFound
	}
	if err != nil {
		return err
	}
	if token.Scope != s.TokenScopeSystem && token.UserID != b.OwnerID {
		return s.ErrAccessDenied
	}
	if b.ID != r.BucketID {
		return s.ErrConflict
	}
	return nil
}
