package config

import (
	"encoding/json"
	"fmt"

	"github.com/KinasTomes/miles/internal/storage"
)

// GlobalProposal is a validated candidate against an expected revision, NOT a
// committed snapshot. No-op is advisory; the backend must still check current
// authorization, incarnation/fence and CAS atomically. Never return success from
// Changed alone or automatically rebase a failed/unknown update onto new state.
type GlobalProposal struct {
	expected uint64
	policy   GlobalPolicy
	changed  bool
	valid    bool
}

func (p GlobalProposal) ExpectedRevision() uint64 { return p.expected }
func (p GlobalProposal) Changed() bool            { return p.changed }
func (p GlobalProposal) Policy() GlobalPolicy     { return p.policy }

// Candidate matches the existing PolicyUpdate.Candidate envelope: Revision=0;
// the backend assigns the resulting revision and publishes history/outbox.
// Deterministic JSON is internal snapshot encoding, not a new frozen R2 format.
func (p GlobalProposal) Candidate() (storage.PolicySnapshot, error) {
	if !p.valid {
		return storage.PolicySnapshot{}, ErrPolicyInvalid
	}
	doc, err := json.Marshal(p.policy)
	if err != nil {
		return storage.PolicySnapshot{}, err
	}
	return storage.PolicySnapshot{Version: storage.ModelVersion, Document: doc}, nil
}

// resolvedRevision is an optional already-validated strong If-Match value from
// the Admin consumer (zero means absent). Wire ETag parsing/auth/HTTP mapping are
// not config responsibilities. Body revision and supplied token must agree.
func expectedRevision(fields map[string]json.RawMessage, resolvedRevision uint64) (uint64, error) {
	if resolvedRevision > storage.MaxPolicyRevision {
		return 0, fmt.Errorf("%w: supplied revision token", ErrPolicyInvalid)
	}
	if raw, ok := fields["expected_revision"]; ok {
		var body uint64
		if err := json.Unmarshal(raw, &body); err != nil || !revisionValid(body) {
			return 0, fmt.Errorf("%w: body revision token", ErrPolicyInvalid)
		}
		if resolvedRevision != 0 && resolvedRevision != body {
			return 0, fmt.Errorf("%w: revision tokens disagree", ErrPolicyInvalid)
		}
		resolvedRevision = body
	}
	if !revisionValid(resolvedRevision) {
		return 0, fmt.Errorf("%w: missing/invalid expected revision", ErrPolicyInvalid)
	}
	return resolvedRevision, nil
}

func (v PolicyValidator) MergeGlobal(current GlobalSnapshot, data []byte, resolvedRevision uint64) (GlobalProposal, error) {
	fields, err := v.parsePolicyJSON(data, updateFields(globalPolicyFields, false), false)
	if err != nil {
		return GlobalProposal{}, err
	}
	expected, err := expectedRevision(fields, resolvedRevision)
	if err != nil {
		return GlobalProposal{}, err
	}
	if !current.valid {
		return GlobalProposal{}, ErrPolicyInvalid
	}
	candidate := current.policy
	// Struct decoding into a value copy preserves omitted nested fields; the
	// strict pass above already rejected nulls, duplicates and unknown keys.
	if err := json.Unmarshal(data, &candidate); err != nil {
		return GlobalProposal{}, fmt.Errorf("%w: policy types: %v", ErrPolicyInvalid, err)
	}
	if err := v.ValidatePolicy(candidate); err != nil {
		return GlobalProposal{}, err
	}
	if expected != current.revision {
		return GlobalProposal{}, ErrPolicyConflict
	}
	changed := candidate != current.policy // Decimal is normalized/value-only.
	if changed && expected == storage.MaxPolicyRevision {
		return GlobalProposal{}, ErrPolicyRevisionExhausted
	}
	return GlobalProposal{expected: expected, policy: candidate, changed: changed, valid: true}, nil
}

type BucketProposal struct {
	expected uint64
	bucketID string
	cache    CachePolicy
	changed  bool
}

func (p BucketProposal) ExpectedRevision() uint64 { return p.expected }
func (p BucketProposal) ExpectedBucketID() string { return p.bucketID }
func (p BucketProposal) Cache() CachePolicy       { return p.cache }
func (p BucketProposal) Changed() bool            { return p.changed }

func (v PolicyValidator) MergeBucket(current BucketSnapshot, data []byte, resolvedRevision uint64) (BucketProposal, error) {
	fields, err := v.parsePolicyJSON(data, updateFields(bucketPolicyFields, true), false)
	if err != nil {
		return BucketProposal{}, err
	}
	expected, err := expectedRevision(fields, resolvedRevision)
	if err != nil {
		return BucketProposal{}, err
	}
	var bucketID string
	if raw, ok := fields["expected_bucket_id"]; !ok {
		return BucketProposal{}, fmt.Errorf("%w: required bucket incarnation", ErrPolicyInvalid)
	} else if err := json.Unmarshal(raw, &bucketID); err != nil || bucketID == "" {
		return BucketProposal{}, fmt.Errorf("%w: bucket incarnation", ErrPolicyInvalid)
	}
	if !current.valid {
		return BucketProposal{}, ErrPolicyInvalid
	}
	candidate := struct {
		Cache CachePolicy `json:"cache"`
	}{Cache: current.cache}
	if err := json.Unmarshal(data, &candidate); err != nil {
		return BucketProposal{}, fmt.Errorf("%w: cache policy types", ErrPolicyInvalid)
	}
	if err := v.ValidateCache(candidate.Cache); err != nil {
		return BucketProposal{}, err
	}
	if expected != current.revision || bucketID != current.bucketID {
		return BucketProposal{}, ErrPolicyConflict
	}
	changed := candidate.Cache != current.cache
	if changed && expected == storage.MaxPolicyRevision {
		return BucketProposal{}, ErrPolicyRevisionExhausted
	}
	return BucketProposal{expected: expected, bucketID: bucketID, cache: candidate.Cache, changed: changed}, nil
}
