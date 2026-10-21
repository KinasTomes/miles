// Package policy evaluates observations without I/O or execution authority.
// Concrete evidence/action APIs are PROPOSALS for consumer review, not frozen.
// Runtime and simulator supply the same inputs to the same functions. Observed
// facts must be bound by the caller to one admitted operation, exact generation,
// pinned metadata revisions and current incarnation. These structs cannot prove
// those facts; adapters must establish and revalidate them before side effects.
package policy

import (
	"errors"
	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/storage"
	"time"
)

var ErrEvidence = errors.New("policy: invalid or contradictory evidence")

type Verdict string

const (
	Eligible      Verdict = "eligible"
	Denied        Verdict = "denied"
	Indeterminate Verdict = "indeterminate"
)

type Reason string

const (
	Ready            Reason = "ready"
	MissingPolicy    Reason = "missing-pinned-policy"
	RevisionMismatch Reason = "policy-revision-mismatch"
	MissingEvidence  Reason = "missing-evidence"
	InvalidEvidence  Reason = "invalid-evidence"
	NotPermitted     Reason = "not-permitted"
	Expired          Reason = "request-expired"
	Cancelled        Reason = "request-cancelled"
	RestoreRequired  Reason = "restore-or-extension-required"
	Unavailable      Reason = "physical-state-unavailable"
)

// Unknown is deliberately the zero value. Valid means caller-established proof,
// not an instruction to infer authorization from a pack or cache entry.
type Fact uint8

const (
	Unknown Fact = iota
	Valid
	Invalid
)

type PinnedPolicy struct {
	Snapshot         config.GlobalSnapshot
	RequiredRevision uint64
}
type Request struct {
	Now       clock.Instant
	Deadline  clock.Instant
	Cancelled bool
}

// UTCProof is a PROPOSAL: [Lower,Upper] already includes sample age, drift,
// precision and error THROUGH Horizon. It describes UTC at Request.Now, not at
// an earlier sample. Trusted requires external validation; no skew default or
// provider is supplied here. A point interval requires equally explicit proof.
type UTCProof struct {
	Trusted      bool
	Lower, Upper time.Time
	Horizon      clock.Instant
}
type ReadFacts struct {
	// Logical refers to the bound read view, not the current object head: a
	// subsequent overwrite/delete/revoke does not invalidate an admitted view.
	Logical, Metadata, Authority, PinCoverage, Physical Fact
}
type ReadInput struct {
	Policy  PinnedPolicy
	Request Request
	Pack    storage.PackInfo
	Facts   ReadFacts
	UTC     UTCProof
	// Backend-verified, conservative expiry observation for this exact pack and
	// state revision. Pack.RestoredUntil is the lower bound, never requested Days.
	RestoreVerified Fact
}
type ActionKind string

const (
	Reuse          ActionKind = "reuse"
	ReviveFromBody ActionKind = "revive-from-client-body"
)

type Action struct {
	Pack          storage.PackRef
	StateRevision uint64
	Kind          ActionKind
}
type Decision struct {
	Verdict        Verdict
	Reason         Reason
	PolicyRevision uint64
	// Actions are only returned for a complete eligible plan. Caller must acquire
	// or revalidate pins/protection, verify bytes and execute fenced transactions.
	Actions []Action
}
type WriteOperation string

const (
	PUT        WriteOperation = "PUT"
	UploadPart WriteOperation = "UploadPart"
)

type WriteInput struct {
	Policy              PinnedPolicy
	Request             Request
	Operation           WriteOperation
	Authority, Metadata Fact
	// Existing canonical candidates only; missing-chunk insertion belongs to the
	// engine. A refused reuse pin permits body revival, never grants read rights.
	Packs     []storage.PackInfo
	ReusePins Fact
}
