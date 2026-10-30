package storage

import (
	"context"
	"encoding/hex"
	"strings"
	"unicode/utf8"
)

// BucketPolicyEntityID is the SystemConfig recovery identity. Encoding the
// incarnation avoids ambiguity with global policy and adapter namespaces.
func BucketPolicyEntityID(id string) string { return "bucket-cache/" + hex.EncodeToString([]byte(id)) }

func validPolicyEntityID(id string) bool {
	if id == "global-policy" {
		return true
	}
	encoded, ok := strings.CutPrefix(id, "bucket-cache/")
	if !ok || encoded == "" {
		return false
	}
	decoded, err := hex.DecodeString(encoded)
	return err == nil && utf8.Valid(decoded) && hex.EncodeToString(decoded) == encoded
}

// BucketPolicyStore extends the existing store; global and bucket revision
// domains are independent. Callers authorize before reads. Mutations recheck
// owner/admin, bucket incarnation, writer and producer in the native transaction.
// Revision zero selects current on read and explicitly bootstraps on write.
// Missing history is an error, never an instruction to manufacture defaults.
type BucketPolicyStore interface {
	GetBucketPolicy(context.Context, string, string, uint64) (PolicySnapshot, error)
	PutBucketPolicy(context.Context, BucketPolicyUpdate) (PolicySnapshot, error)
}

type BucketPolicyUpdate struct {
	Bucket, BucketID, CredentialID string
	ExpectedRevision               uint64
	Candidate                      PolicySnapshot // complete cache-only document; Revision must be zero
	Writer                         WriterFence
}
