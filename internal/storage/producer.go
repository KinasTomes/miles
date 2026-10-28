package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"time"
)

// MutationContext is supplied by trusted admission, never HTTP headers. History,
// producer epoch and writer authority are independently checked by the backend.
type MutationContext struct {
	Entity, Identity                   string
	Object                             ObjectKey
	HistoryID, LogicalOpID, MutationID string
	Epoch                              uint64
	Writer                             WriterFence
}
type mutationContextKey struct{}

func (m MutationContext) Validate() error {
	if !hex128(m.HistoryID) || !hex128(m.LogicalOpID) || !hex128(m.MutationID) || m.Epoch > MaxPolicyRevision || m.Writer.LeaseID == "" || m.Writer.Epoch == 0 || m.Writer.ServiceIncarnation == "" {
		return ErrInvalid
	}
	return nil
}

func WithMutation(ctx context.Context, m MutationContext) context.Context {
	return context.WithValue(ctx, mutationContextKey{}, m)
}
func MutationFromContext(ctx context.Context) (MutationContext, bool) {
	m, ok := ctx.Value(mutationContextKey{}).(MutationContext)
	return m, ok
}
func ChildMutation(m MutationContext, entity, identity, action string) MutationContext {
	b, _ := json.Marshal([]string{"miles-child-mutation-v1", m.MutationID, entity, identity, action})
	sum := sha256.Sum256(b)
	m.MutationID = hex.EncodeToString(sum[:16])
	m.Entity = entity
	m.Identity = identity
	return m
}

type AdminToken struct {
	ID, UserID, Scope string
	Hash              []byte
	Active            bool
	Revision          uint64
	ExpiresAt         time.Time
}
type CredentialSecurity struct {
	Version    uint32
	KeyID      string
	Ciphertext []byte
	ExpiresAt  time.Time
}
type ControlMutation struct {
	AdminTokenID                       string
	Context                            MutationContext
	CredentialID, Entity, ID, ParentID string
	ExpectedRevision                   uint64
	Deleted                            bool
	User                               User
	Bucket                             Bucket
	Grant                              Grant
	Credential                         Credential
	Security                           CredentialSecurity
	Token                              AdminToken
	SetQuota                           bool
	QuotaLimit                         int64
}
type MutationResult struct {
	HistoryID, LogicalOpID string
	Epoch                  uint64
	NoOp                   bool
	Version                uint32
	Scope, InputSHA256     string
	Event                  OutboxEvent
	Value                  []byte
}
type ProducerStore interface {
	MutateControl(context.Context, ControlMutation) (MutationResult, error)
	GetMutation(context.Context, string) (MutationResult, error)
	PutPayload(context.Context, string, []byte) error
	Open(context.Context, string) (io.ReadCloser, error)
}

// BucketNamespace is updated with object/upload mutations under the same CAS.
// Missing baseline is unknown, never an empty-bucket proof.
type BucketNamespace struct {
	Version              uint32
	LiveObjects, Uploads int64
}
