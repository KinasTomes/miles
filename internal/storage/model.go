// Package storage defines the shared metadata, pack and cache boundary.
// It contains no backend SDK, HTTP handler, lease worker or binary pack codec.
package storage

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"time"
)

const ModelVersion uint32 = 1
const MaxObjectBytes int64 = 100 << 30

var (
	ErrInvalid       = errors.New("invalid storage input")
	ErrNotFound      = errors.New("storage record not found")
	ErrConflict      = errors.New("storage revision or immutable content conflict")
	ErrAccessDenied  = errors.New("storage access denied")
	ErrQuotaExceeded = errors.New("logical quota exceeded")
	ErrFenced        = errors.New("writer lease or fence is no longer valid")
	ErrUnavailable   = errors.New("backend unavailable; outcome may be unknown")
	ErrCold          = errors.New("pack requires restore")
)

var identifier = regexp.MustCompile(`^[a-z0-9_-]{1,64}$`)
var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

type ChunkKey struct {
	Domain string
	Hash   string
}

func (k ChunkKey) Validate() error {
	if !identifier.MatchString(k.Domain) || !sha256Hex.MatchString(k.Hash) {
		return fmt.Errorf("%w: domain/hash", ErrInvalid)
	}
	return nil
}

// PartitionKey is the single DynamoDB PK, never a domain-only partition.
func (k ChunkKey) PartitionKey() (string, error) {
	if err := k.Validate(); err != nil {
		return "", err
	}
	return k.Domain + "#" + k.Hash, nil
}

// NewBucketID must be called for every new incarnation, including name reuse.
func NewBucketID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

type Bucket struct {
	Name, ID, OwnerID, Domain string
	Revision                  uint64
	Active                    bool
}
type User struct {
	ID       string
	Active   bool
	Revision uint64
}
type Credential struct {
	ID, UserID          string
	Active, SystemAdmin bool
	Revision            uint64
}
type Grant struct {
	BucketID, UserID string
	Read, Write      bool
	Revision         uint64
}
type Authorization struct {
	UserID, CredentialID     string
	Bucket                   Bucket
	Read, Write, SystemAdmin bool
}

type PackRef struct{ ID, Generation string }
type ByteRange struct{ Offset, Length int64 }

func (r ByteRange) Validate(size int64) error {
	if r.Offset < 0 || r.Length <= 0 || size < 0 || r.Offset > size || r.Length > size-r.Offset {
		return fmt.Errorf("%w: byte range", ErrInvalid)
	}
	return nil
}
func (r PackRef) Validate() error {
	if r.ID == "" || r.Generation == "" {
		return fmt.Errorf("%w: pack identity/generation", ErrInvalid)
	}
	return nil
}

type StorageClass string

const (
	Standard   StorageClass = "STANDARD"
	StandardIA StorageClass = "STANDARD_IA"
	GlacierIR  StorageClass = "GLACIER_IR"
	Glacier    StorageClass = "GLACIER"
)

func (c StorageClass) Validate() error {
	switch c {
	case Standard, StandardIA, GlacierIR, Glacier:
		return nil
	}
	return fmt.Errorf("%w: storage class", ErrInvalid)
}

type PackInfo struct {
	State                                            string // Warm/Cold/Restoring/Restored/Transitioning/Retiring/Deleting; R1 owns admission.
	Version                                          uint32
	Ref                                              PackRef
	Size                                             int64
	Class                                            StorageClass
	RestoredUntil, ProtectedUntil, MinRetentionUntil time.Time
	StateRevision                                    uint64
}
type ChunkLocation struct {
	Pack  PackRef
	Range ByteRange
	Codec string
}
type Chunk struct {
	Version          uint32
	Key              ChunkKey
	RawBytes         int64
	Location         ChunkLocation
	LocationRevision uint64
}
type ChunkRef struct {
	Key                     ChunkKey
	RawBytes, LogicalOffset int64
}
type ManifestPage struct {
	Version            uint32
	ManifestID, Domain string
	Number             uint32
	Chunks             []ChunkRef
}
type ObjectKey struct{ Bucket, BucketID, Key string }
type Checksum struct {
	Algorithm, Type, Value string
	PartCount              int
}
type ObjectHead struct {
	Version            uint32
	Object             ObjectKey
	Domain, ManifestID string
	Size               int64
	ETag, ETagKind     string
	Checksum           Checksum
	Metadata           map[string]string
	Revision           uint64
	OpID               string
	Deleted            bool
}

// WriterFence is checked against authoritative lease state inside each mutation.
// It is not a client-supplied authorization token or a substitute for R1 admission.
type WriterFence struct {
	LeaseID, ServiceIncarnation string
	Epoch                       uint64
}
type WriterLease struct {
	Fence     WriterFence
	ExpiresAt time.Time
}
type LogicalQuota struct {
	OwnerID                       string
	Limit, Used, Reserved, Staged int64
	ActiveUploads                 int
}
type Reservation struct {
	ID, OwnerID string
	UploadID    string
	StagedBytes int64
	Object      ObjectKey
	Bytes       int64
	Writer      WriterFence
}
type OutboxEvent struct {
	Version                             uint32
	ID, HistoryID                       string
	Epoch                               uint64
	Entity                              string
	Object                              ObjectKey
	Revision                            uint64
	OpID, PayloadPointer, PayloadSHA256 string
}
type Completion struct {
	UploadID, BucketID, Key, Initiator string
	Revision, Fence                    uint64
	State                              string
}
type CompletedReceipt struct {
	Version                                                                 uint32
	UploadID, BucketID, Key, Initiator, OpID, InputDigest, ManifestID, ETag string
	Checksum                                                                Checksum
	ExpiresAt                                                               time.Time
}
type CommitRequest struct {
	Head                                ObjectHead // Revision is assigned by the backend; must be zero on input.
	ExpectedRevision                    uint64     // zero means absent, not a wildcard; tombstones have revisions.
	UserID, CredentialID, ReservationID string
	Writer                              WriterFence
	Event                               OutboxEvent
	Receipt                             *CompletedReceipt
	Completion                          *Completion // frozen Completing state/revision/fence to condition-check.
}
type CommitResult struct {
	Head             ObjectHead
	AlreadyCommitted bool
}

type RestoreRequest struct {
	Days int
	Tier string
}

type CacheKey struct {
	Pack  PackRef
	Range ByteRange
}
