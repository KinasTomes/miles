package integration

import (
	"context"
	"encoding/json"

	"github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/policy"
	s "github.com/KinasTomes/miles/internal/storage"
)

// LoadBucket reads one exact incarnation/revision from the same metadata
// backend. Authentication/authorization must precede this internal disclosure.
func (c Context) LoadBucket(ctx context.Context, store s.BucketPolicyStore, v config.PolicyValidator, bucket, id string, revision uint64) (Context, error) {
	if store == nil || revision > s.MaxPolicyRevision {
		return Context{}, s.ErrInvalid
	}
	r, err := store.GetBucketPolicy(ctx, bucket, id, revision)
	if err != nil {
		return Context{}, err
	}
	if r.Version != s.ModelVersion || revision != 0 && r.Revision != revision {
		return Context{}, s.ErrUnavailable
	}
	b, err := v.DecodeBucket(id, r.Revision, r.Document)
	if err != nil {
		return Context{}, err
	}
	return c.BindBucket(b, id, r.Revision)
}

// CreateBucketPolicy is explicit bootstrap/migration of a previously missing
// record. The caller chooses defaults HERE; reads never manufacture revision1.
func (c Context) CreateBucketPolicy(ctx context.Context, store s.BucketPolicyStore, v config.PolicyValidator, bucket, id, credential string, writer s.WriterFence, cache config.CachePolicy) (Context, error) {
	if err := v.ValidateCache(cache); err != nil {
		return Context{}, err
	}
	return c.publishBucket(ctx, store, v, bucket, id, credential, writer, 0, cache, 1)
}

func (c Context) CommitBucket(ctx context.Context, store s.BucketPolicyStore, v config.PolicyValidator, bucket, credential string, writer s.WriterFence, proposal config.BucketProposal) (Context, error) {
	if proposal.ExpectedRevision() == 0 || proposal.ExpectedRevision() > s.MaxPolicyRevision || proposal.ExpectedBucketID() == "" {
		return Context{}, s.ErrInvalid
	}
	if err := v.ValidateCache(proposal.Cache()); err != nil {
		return Context{}, err
	}
	revision := proposal.ExpectedRevision()
	if proposal.Changed() {
		if revision == s.MaxPolicyRevision {
			return Context{}, config.ErrPolicyRevisionExhausted
		}
		revision++
	}
	return c.publishBucket(ctx, store, v, bucket, proposal.ExpectedBucketID(), credential, writer, proposal.ExpectedRevision(), proposal.Cache(), revision)
}

func (c Context) publishBucket(ctx context.Context, store s.BucketPolicyStore, v config.PolicyValidator, bucket, id, credential string, writer s.WriterFence, expected uint64, cache config.CachePolicy, resultRevision uint64) (Context, error) {
	if store == nil || c.global.Revision() == 0 {
		return Context{}, s.ErrInvalid
	}
	doc, err := json.Marshal(struct {
		Cache config.CachePolicy `json:"cache"`
	}{cache})
	if err != nil {
		return Context{}, err
	}
	// Validate the complete envelope BEFORE the authoritative mutation.
	if _, err := v.DecodeBucket(id, resultRevision, doc); err != nil {
		return Context{}, err
	}
	r, err := store.PutBucketPolicy(ctx, s.BucketPolicyUpdate{Bucket: bucket, BucketID: id, CredentialID: credential, Writer: writer, ExpectedRevision: expected, Candidate: s.PolicySnapshot{Version: s.ModelVersion, Document: doc}})
	if err != nil {
		return Context{}, err
	}
	b, err := v.DecodeBucket(id, r.Revision, r.Document)
	if err != nil || r.Version != s.ModelVersion || r.Revision != resultRevision || b.Cache() != cache {
		return Context{}, publicationResponseError(s.ErrInvalid)
	}
	return c.BindBucket(b, id, r.Revision)
}

func (c Context) EvaluateTiering(in policy.TieringInput) (policy.TieringDecision, error) {
	p, err := c.bind(in.Policy)
	if err != nil {
		return policy.TieringDecision{Decision: policy.Decision{Verdict: policy.Indeterminate, Reason: policy.RevisionMismatch, PolicyRevision: c.global.Revision()}}, err
	}
	in.Policy = p
	return policy.EvaluateTiering(in)
}
