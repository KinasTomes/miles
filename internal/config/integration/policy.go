// Package integration connects config snapshots to the EXISTING metadata ports
// and shared pure evaluators. APIs are PROPOSALS, not frozen wire contracts.
// It is not production runtime wiring, an authorization layer or another store.
package integration

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/policy"
	"github.com/KinasTomes/miles/internal/storage"
)

// Context contains only immutable value snapshots, never a store/current lookup.
// Global provenance comes from LoadGlobal; optional bucket provenance remains a
// caller obligation until shared bucket cache-policy persistence ports exist.
type Context struct {
	global config.GlobalSnapshot
	bucket config.BucketSnapshot
}

// LoadGlobal performs exactly one lookup. Zero selects current at admission;
// positive revisions select exact history. Lookup errors never cause fallback.
// Caller must authorize before disclosing policy/resource/revision information.
func LoadGlobal(ctx context.Context, store storage.MetadataStore, v config.PolicyValidator, revision uint64) (Context, error) {
	if store == nil || revision > storage.MaxPolicyRevision {
		return Context{}, storage.ErrInvalid
	}
	record, err := store.GetPolicy(ctx, revision)
	snapshot, err := v.ResolveGlobal(revision, record, err)
	if err != nil {
		return Context{}, err
	}
	return Context{global: snapshot}, nil
}

func (c Context) Global() config.GlobalSnapshot { return c.global }
func (c Context) Bucket() config.BucketSnapshot { return c.bucket }

// BindBucket validates an explicitly handed-off snapshot and its independent
// revision/incarnation, NOT its authoritative origin. Never substitute the global
// revision. No bucket read/write/history capability is invented here.
func (c Context) BindBucket(snapshot config.BucketSnapshot, id string, revision uint64) (Context, error) {
	if id == "" || revision == 0 || revision > storage.MaxPolicyRevision || snapshot.BucketID() != id || snapshot.Revision() != revision {
		return Context{}, storage.ErrInvalid
	}
	if _, err := config.EffectivePolicy(c.global, snapshot); err != nil {
		return Context{}, err
	}
	c.bucket = snapshot
	return c, nil
}

// CreateGlobal is the INTERNAL empty-store bootstrap path only. Defaults must
// have been explicitly selected by the creator; historical reads never use it.
// The backend allocates revision1; config does not assign committed revisions.
func CreateGlobal(ctx context.Context, store storage.MetadataStore, v config.PolicyValidator, p config.GlobalPolicy, credential string, writer storage.WriterFence, event *storage.OutboxEvent) (Context, error) {
	if store == nil {
		return Context{}, storage.ErrInvalid
	}
	if err := v.ValidatePolicy(p); err != nil {
		return Context{}, err
	}
	doc, err := json.Marshal(p)
	if err != nil {
		return Context{}, err
	}
	result, err := store.PutPolicy(ctx, storage.PolicyUpdate{Candidate: storage.PolicySnapshot{Version: storage.ModelVersion, Document: doc}, CredentialID: credential, Writer: writer, Event: event})
	if err != nil {
		return Context{}, err
	}
	snapshot, err := v.ResolveGlobal(1, result, nil)
	if err != nil {
		return Context{}, publicationResponseError(err)
	}
	if snapshot.Policy() != p {
		return Context{}, publicationResponseError(storage.ErrInvalid)
	}
	return Context{global: snapshot}, nil
}

// CommitGlobal forwards the original expected revision/candidate and caller's
// credential/fence/event. Even a no-op reaches authoritative CAS. Never rebase,
// retry or replace the supplied event after conflict/unknown outcome. A decode
// error AFTER PutPolicy does not prove no commit: reconcile retained operation
// identity/history/outbox before retry or cleanup.
func CommitGlobal(ctx context.Context, store storage.MetadataStore, v config.PolicyValidator, p config.GlobalProposal, credential string, writer storage.WriterFence, event *storage.OutboxEvent) (Context, error) {
	if store == nil {
		return Context{}, storage.ErrInvalid
	}
	candidate, err := p.Candidate()
	if err != nil {
		return Context{}, err
	}
	// A proposal only proves validation by its creator, not by THIS validator.
	// Validate the complete generated document under B before any publication.
	// The temporary revision labels a decode check, not a backend allocation;
	// the original candidate sent to PutPolicy retains Revision=0.
	if candidate.Version != storage.ModelVersion || candidate.Revision != 0 || p.ExpectedRevision() == 0 || p.ExpectedRevision() > storage.MaxPolicyRevision {
		return Context{}, storage.ErrInvalid
	}
	validationRecord := candidate
	validationRecord.Revision = p.ExpectedRevision()
	if _, err := v.DecodeGlobal(validationRecord); err != nil {
		return Context{}, err
	}
	expected := p.ExpectedRevision()
	if p.Changed() {
		if expected == storage.MaxPolicyRevision {
			return Context{}, config.ErrPolicyRevisionExhausted
		}
		expected++
	}
	result, err := store.PutPolicy(ctx, storage.PolicyUpdate{Candidate: candidate, ExpectedRevision: p.ExpectedRevision(), CredentialID: credential, Writer: writer, Event: event})
	if err != nil {
		return Context{}, err
	}
	snapshot, err := v.ResolveGlobal(expected, result, nil)
	if err != nil {
		return Context{}, publicationResponseError(err)
	}
	if snapshot.Policy() != p.Policy() {
		return Context{}, publicationResponseError(storage.ErrInvalid)
	}
	return Context{global: snapshot}, nil
}

// The existing shared error contract uses ErrUnavailable for an outcome that
// may be unknown. A successful PutPolicy with an unverifiable response cannot
// return an ordinary pre-publication ErrInvalid/Deferred. Keep verification
// details diagnostic only: callers must reconcile, never infer rollback.
// A richer committed-but-invalid-response taxonomy remains a consumer decision.
func publicationResponseError(cause error) error {
	return fmt.Errorf("%w: policy publication returned an unverifiable response; reconcile operation/history/outbox before retry: %v", storage.ErrUnavailable, cause)
}

// bind accepts empty evaluator policy input as an explicit request to use this
// context, or checks an already supplied pin in full. Conflicting pins are never
// silently overwritten. No store I/O occurs during evaluation.
func (c Context) bind(supplied policy.PinnedPolicy) (policy.PinnedPolicy, error) {
	if c.global.Revision() == 0 {
		return policy.PinnedPolicy{}, storage.ErrInvalid
	}
	if supplied.RequiredRevision != 0 || supplied.Snapshot.Revision() != 0 {
		if supplied.RequiredRevision != c.global.Revision() || supplied.Snapshot.Revision() != c.global.Revision() || supplied.Snapshot.Policy() != c.global.Policy() {
			return policy.PinnedPolicy{}, storage.ErrConflict
		}
	}
	return policy.PinnedPolicy{Snapshot: c.global, RequiredRevision: c.global.Revision()}, nil
}

func (c Context) EvaluateRead(in policy.ReadInput) (policy.Decision, error) {
	p, err := c.bind(in.Policy)
	if err != nil {
		return policy.Decision{Verdict: policy.Indeterminate, Reason: policy.RevisionMismatch, PolicyRevision: c.global.Revision()}, err
	}
	in.Policy = p
	return policy.EvaluateRead(in)
}
func (c Context) EvaluateWrite(in policy.WriteInput) (policy.Decision, error) {
	p, err := c.bind(in.Policy)
	if err != nil {
		return policy.Decision{Verdict: policy.Indeterminate, Reason: policy.RevisionMismatch, PolicyRevision: c.global.Revision()}, err
	}
	in.Policy = p
	return policy.EvaluateWrite(in)
}
func (c Context) EvaluateProtection(in policy.ProtectionInput) (policy.ProtectionDecision, error) {
	p, err := c.bind(in.Policy)
	if err != nil {
		return policy.ProtectionDecision{Verdict: policy.Indeterminate, Reason: policy.RevisionMismatch, PolicyRevision: c.global.Revision()}, err
	}
	in.Policy = p
	return policy.EvaluateProtection(in)
}
