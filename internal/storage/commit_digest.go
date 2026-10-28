package storage

import (
	"crypto/sha256"
	"encoding/json"
	"time"
)

// Generated execution facts are not semantic Complete input. The first winning
// transaction retains its original receipt/expiry; a retry cannot refresh it.
func CommitInputDigest(r CommitRequest) ([32]byte, error) {
	r.Writer = WriterFence{}
	if r.Producer != nil {
		copy := *r.Producer
		copy.Writer = WriterFence{}
		r.Producer = &copy
		r.Event.PayloadPointer = ""
		r.Event.PayloadSHA256 = ""
	}
	r.Completion = nil
	r.ReservationRevision = 0
	if r.MultipartAttempt > 0 && r.Receipt != nil {
		copy := *r.Receipt
		copy.ExpiresAt = time.Time{}
		r.Receipt = &copy
	}
	r.MultipartAttempt = 0
	b, err := json.Marshal(r)
	return sha256.Sum256(b), err
}
