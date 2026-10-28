package storage

import (
	"context"
	"time"
)

// Multipart records are an additive schema. Existing Completion/Receipt v1
// records keep their decoder; full records occupy a distinct adapter projection.
type StreamDescriptor struct {
	Kind          byte
	Owner, SHA256 string
	Length        int64
	Pages         uint32
}
type MultipartUpload struct {
	Version                              uint32
	ID                                   string
	CreateOp, CreateDigest               string
	Object                               ObjectKey
	Owner, Initiator, Credential, Domain string
	State                                string
	Revision, Fence                      uint64
	ExpiresAt, LeaseUntil                time.Time
	Writer                               WriterFence
	ReservationID, CompletionOp          string
	ChecksumAlgorithm, ChecksumType      string
	Metadata                             map[string]string
	PublishedBytes, ProvisionalBytes     int64
	Publishers                           uint32
	Input, Snapshot                      StreamDescriptor
	Prepared                             ObjectHead
	ClosedFence                          uint64
	ClosedWriter                         WriterFence
}
type UploadPart struct {
	Version    uint32
	UploadID   string
	Number     uint32
	Revision   uint64
	Size       int64
	ETag       string
	Checksum   Checksum
	ManifestID string
	Pages      uint32
	OpID       string
}
type PartPublisher struct {
	Version                        uint32
	ID, UploadID, OpID, ManifestID string
	PartNumber                     uint32
	BaseRevision                   uint64
	Bytes                          int64
	State                          string
	Writer                         WriterFence
	Deadline                       time.Time
	Packs                          []PackRef
}
type MultipartRequest struct {
	UploadID, Credential, OpID string
	ExpectedRevision, Fence    uint64
	Writer                     WriterFence
}
type CreateUploadRequest struct {
	Request MultipartRequest
	Upload  MultipartUpload
}
type PartPublishRequest struct {
	Request     MultipartRequest
	PublisherID string
	Part        UploadPart
}
type FreezeRequest struct {
	Request MultipartRequest
	Input   StreamDescriptor
}
type PrepareCompletionRequest struct {
	Request  MultipartRequest
	Snapshot StreamDescriptor
	Head     ObjectHead
}
type CompletionReceipt struct {
	Version                                                uint32
	UploadID, OpID, Initiator, InputDigest, SnapshotDigest string
	Object                                                 ObjectKey
	CommittedAt, ExpiresAt                                 time.Time
	Head                                                   ObjectHead
}
type PublisherPage struct {
	Publishers []PartPublisher
	Next       string
}
type PartListPage struct {
	Parts []UploadPart
	Next  uint32
}

// MultipartStore is the persistence boundary consumed by HTTP orchestration.
// No method uses an upload ID as authentication. Recovery/cleanup operations
// require writer authority; signing credential revocation cannot prevent proof.
type MultipartStore interface {
	CreateUpload(context.Context, CreateUploadRequest) (MultipartUpload, error)
	GetUpload(context.Context, string) (MultipartUpload, error)
	RegisterPartPublisher(context.Context, MultipartRequest, PartPublisher) (PartPublisher, error)
	PublishPart(context.Context, PartPublishRequest) (UploadPart, error)
	CancelPartPublisher(context.Context, MultipartRequest, string) error
	ListParts(context.Context, MultipartRequest, uint32, int) (PartListPage, error)
	FreezeCompletion(context.Context, FreezeRequest) (MultipartUpload, error)
	PrepareCompletion(context.Context, PrepareCompletionRequest) (MultipartUpload, error)
	ClaimCompletion(context.Context, MultipartRequest) (MultipartUpload, error)
	RenewCompletion(context.Context, MultipartRequest) (MultipartUpload, error)
	ReopenUpload(context.Context, MultipartRequest) (MultipartUpload, error)
	AbortUpload(context.Context, MultipartRequest) (MultipartUpload, error)
	GetMultipartReceipt(context.Context, MultipartRequest, string) (CompletionReceipt, error)
	WriteCompletionPage(context.Context, byte, string, uint32, []byte, WriterFence) error
	ReadCompletionPage(context.Context, byte, string, uint32) ([]byte, error)
}

// PublisherProtection is logical retained membership, independent of a wall
// clock lease. Unknown publication cannot be declared quiescent by a timeout.
type PublisherProtection interface {
	ListPublishers(context.Context, string, string, int) (PublisherPage, error)
	RetainPublisherPacks(context.Context, MultipartRequest, string, []PackRef) error
}

// RecoveryDisposition is evidence returned by a committed metadata transaction.
// Unknown (including a lost acknowledgement) never authorizes releasing roots.
type RecoveryDisposition string

const (
	RecoveryUnknown   RecoveryDisposition = "Unknown"
	RecoveryCommitted RecoveryDisposition = "Committed"
	RecoveryClosed    RecoveryDisposition = "ProvenClosedNoCommit"
)

type MultipartRecoveryResult struct {
	Disposition RecoveryDisposition
	Upload      MultipartUpload
	Receipt     CompletionReceipt
	Publisher   PartPublisher
}

// MultipartRecovery requires the current trusted upload authority, independent
// of the original signing credential. Closing serializes with late publication;
// lease expiry, missing outcomes and caller-supplied booleans are never proof.
// ResolveCompletion leaves Aborting until publishers are durably reconciled.
// Roots must remain retained for Active/Completing/Aborting uploads, including
// terminal publishers whose manifests may still be selected by Complete.
type MultipartRecovery interface {
	ResolveCompletion(context.Context, MultipartRequest) (MultipartRecoveryResult, error)
	ResolvePartPublisher(context.Context, MultipartRequest, string) (MultipartRecoveryResult, error)
}
