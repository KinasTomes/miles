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
	ErrInvalid  = errors.New("invalid storage input")
	ErrNotFound = errors.New("storage record not found")
	// ErrBucketNotFound preserves ErrNotFound for existing callers while letting
	// write adapters distinguish a missing/inactive/recreated bucket from an
	// absent reservation or another internal record.
	ErrBucketNotFound = fmt.Errorf("%w: bucket missing, inactive or incarnation changed", ErrNotFound)
	ErrConflict       = errors.New("storage revision or immutable content conflict")
	ErrAccessDenied   = errors.New("storage access denied")
	ErrQuotaExceeded  = errors.New("logical quota exceeded")
	ErrFenced         = errors.New("writer lease or fence is no longer valid")
	ErrUnavailable    = errors.New("backend unavailable; outcome may be unknown")
	ErrCold           = errors.New("pack requires restore")
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

// ValidateDomain checks one dedup domain identifier (API v2 2.2.1): 1-64
// characters of [a-z0-9_-], no "#", so it can never collide with the "#"-joined
// partition key of a chunk.
func ValidateDomain(domain string) error {
	if !identifier.MatchString(domain) {
		return fmt.Errorf("%w: domain", ErrInvalid)
	}
	return nil
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
	// CreatedAt is set once by CreateBucket; ListBuckets returns it as
	// CreationDate. It is zero on fixtures seeded before the field existed.
	CreatedAt time.Time
}
type User struct {
	ID       string
	Active   bool
	Revision uint64
	// DefaultDomain is the dedup domain CreateBucket copies into the new
	// bucket (API v2 3.5). Empty falls back to "default": fixture and dev
	// seeds predate the field and every domain was "default".
	DefaultDomain string
}

// BucketCreate is the CreateBucket transaction of API v2 2.2.3/3.5: the
// credential that signed the request, the user it belongs to (both must still
// be active when the transaction runs) and the requested name. The backend
// generates the bucket_id, copies the owner's current default_domain and sets
// CreatedAt; nothing else of the caller survives into the record.
type BucketCreate struct {
	CredentialID, UserID, Name string
}

func (r BucketCreate) Validate() error {
	if r.CredentialID == "" || r.UserID == "" || r.Name == "" {
		return fmt.Errorf("%w: bucket create identity", ErrInvalid)
	}
	return nil
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
	Writer                                           WriterFence // Uploading -> Warm publication owner.
	RestoredUntil, ProtectedUntil, MinRetentionUntil time.Time
	StateRevision                                    uint64
	LastAccess                                       time.Time
}

// SamePackInfo compares persisted pack metadata by value. time.Time.Equal
// ignores monotonic clock readings and location pointers lost in JSON storage.
func SamePackInfo(a, b PackInfo) bool {
	if !a.RestoredUntil.Equal(b.RestoredUntil) || !a.ProtectedUntil.Equal(b.ProtectedUntil) ||
		!a.MinRetentionUntil.Equal(b.MinRetentionUntil) || !a.LastAccess.Equal(b.LastAccess) {
		return false
	}
	a.RestoredUntil, b.RestoredUntil = time.Time{}, time.Time{}
	a.ProtectedUntil, b.ProtectedUntil = time.Time{}, time.Time{}
	a.MinRetentionUntil, b.MinRetentionUntil = time.Time{}, time.Time{}
	a.LastAccess, b.LastAccess = time.Time{}, time.Time{}
	return a == b
}

type ChunkLocation struct {
	Pack  PackRef
	Range ByteRange
	Codec string
	// CRC32C covers the stored range. It lets GET verify bytes before zstd
	// decoding without fetching the footer for every chunk.
	CRC32C uint32
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
	// Derived and persisted by the metadata publisher. An all-zero descriptor
	// on input requests derivation; supplied descriptors must match the pages.
	PageCount          uint32
	ChunkCount         uint64
	PageIndex          []uint64
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
type ReservationState string

const (
	ReservationActive    ReservationState = "Active"
	ReservationCommitted ReservationState = "Committed"
	ReservationReleased  ReservationState = "Released"
)

type Reservation struct {
	Version      uint32
	OpID         string
	BaseRevision uint64
	BaseSize     int64
	Revision     uint64
	State        ReservationState
	Deadline     time.Time
	ID, OwnerID  string
	UploadID     string
	StagedBytes  int64
	Object       ObjectKey
	Bytes        int64
	Writer       WriterFence
}
type OutboxEvent struct {
	Version                             uint32
	ID, HistoryID                       string
	Epoch                               uint64
	Entity                              string
	Action                              string
	EntityID                            string
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
	MultipartAttempt                    uint64 `json:",omitempty"`
	ReservationRevision                 uint64
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

// Claims reconcile the durable operation outcome before transferring an orphaned
// Active ledger. A committed result resolves the operation without a new owner.
type ReservationClaim struct {
	ID, OpID         string
	ExpectedRevision uint64
	Writer           WriterFence
}
type ReservationResolution struct {
	Reservation Reservation
	Committed   *CommitResult
}
type ReservationRelease struct {
	ID, OpID         string
	ExpectedRevision uint64
	Writer           WriterFence
}

// ReservationAdjustment replaces both counters at one ledger revision. The
// writer must use the returned revision for publication or another adjustment.
type ReservationAdjustment struct {
	ID, OpID         string
	ExpectedRevision uint64
	Bytes            int64
	StagedBytes      int64
	Writer           WriterFence
}

// Cursors below are INTERNAL scan state, never unsigned HTTP tokens. The API
// layer owns opaque encoding/MAC/expiry and must validate it before using them.
type BucketListRequest struct {
	PrincipalID, After string
	Limit              int
}
type BucketListPage struct {
	Buckets   []Bucket
	NextAfter string
}
type ListObjectsRequest struct {
	PrincipalID, Bucket, BucketID, Prefix, Delimiter, StartAfter string
	MaxKeys                                                      int
	Cursor                                                       *ObjectListCursor
}
type ObjectListCursor struct {
	PrincipalID, Bucket, BucketID, Prefix, Delimiter string
	LastScannedKey, LastCommonPrefix                 string
}
type ObjectListPage struct {
	Objects        []ObjectHead
	CommonPrefixes []string
	Next           *ObjectListCursor
}
type PackListRequest struct {
	State string
	After *PackRef
	Limit int
}
type PackListPage struct {
	Packs     []PackInfo
	NextAfter *PackRef
}
type PackAccess struct {
	Pack       PackRef
	LastAccess time.Time
}

// MaxPolicyRevision is the approved JSON-safe revision domain (Clock/policy §4.1).
const MaxPolicyRevision uint64 = 1<<53 - 1

// Document is the complete canonical policy snapshot validated by internal/config,
// not an unvalidated HTTP patch. Storage owns CAS/current/history/event atomicity;
// policy field validation and merge semantics remain owned by config/policy.
type PolicySnapshot struct {
	Version  uint32
	Revision uint64
	Document []byte
}
type PolicyUpdate struct {
	Candidate        PolicySnapshot
	ExpectedRevision uint64
	CredentialID     string
	Writer           WriterFence
	Event            *OutboxEvent // required for a change, absent on a semantic no-op.
}

// Promotions require a new Warm pack and location CAS, never a reverse class copy.
func (c StorageClass) ValidateTransition(next StorageClass) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if err := next.Validate(); err != nil {
		return err
	}
	if (c == Glacier && next != Glacier) || (c == GlacierIR && next != GlacierIR && next != Glacier) {
		return fmt.Errorf("%w: Cold pack promotion requires a new Warm pack", ErrInvalid)
	}
	return nil
}
