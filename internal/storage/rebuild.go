package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// IndexRecord is an installation projection, never an online mutation or lease.
// Adapters whitelist kinds and decode the corresponding concrete record type.
type IndexRecord struct {
	Kind string
	IDs  []string
	Body []byte
}

// IndexRecordKey frames IDs without delimiter ambiguity (object keys may
// contain control characters). All installation producers use this byte order.
func IndexRecordKey(kind string, ids []string) string {
	if len(ids) == 0 {
		return kind + "/"
	}
	body, _ := json.Marshal(ids)
	return kind + "/" + hex.EncodeToString(body)
}

type IndexInstall struct {
	SourceHistory, SourceIncarnation, TargetSHA256 string
	History, Incarnation, GenesisSHA256            string
	GenesisLength                                  int
	RecordsSHA256                                  string
}
type IndexInstaller interface {
	BeginIndexInstall(context.Context, IndexInstall) error
	StageIndexRecord(context.Context, IndexInstall, IndexRecord) error
	FinishIndexInstall(context.Context, IndexInstall) error
}

type indexReadinessKey struct{}
type indexReadiness struct {
	Request    IndexInstall
	ValidUntil time.Time
	Check      func(context.Context) error
}

// WithIndexReadiness carries trusted, request-bound readiness validity to the
// final metadata dispatch. It is never populated from HTTP parameters.
func WithIndexReadiness(ctx context.Context, r IndexInstall, validUntil time.Time, check func(context.Context) error) context.Context {
	return context.WithValue(ctx, indexReadinessKey{}, indexReadiness{r, validUntil, check})
}
func CheckIndexReadiness(ctx context.Context, r IndexInstall) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	proof, ok := ctx.Value(indexReadinessKey{}).(indexReadiness)
	if !ok || proof.Request != r || proof.Check == nil || proof.ValidUntil.IsZero() {
		return ErrFenced
	}
	if e := proof.Check(ctx); e != nil {
		return e
	}
	if !time.Now().Before(proof.ValidUntil) {
		return ErrFenced
	}
	return nil
}
func IndexReadinessDeadline(ctx context.Context, r IndexInstall) (time.Time, error) {
	proof, ok := ctx.Value(indexReadinessKey{}).(indexReadiness)
	if !ok || proof.Request != r || proof.ValidUntil.IsZero() || proof.Check == nil {
		return time.Time{}, ErrFenced
	}
	return proof.ValidUntil, nil
}

func IndexRecordDigest(previous string, r IndexRecord) (string, error) {
	if previous != "" && !sha256Hex.MatchString(previous) || r.Kind == "" || len(r.Body) == 0 || !json.Valid(r.Body) {
		return "", ErrInvalid
	}
	b, e := json.Marshal(r)
	if e != nil {
		return "", e
	}
	h := sha256.New()
	h.Write([]byte("miles-rebuilt-index-v1\x00"))
	h.Write([]byte(previous))
	h.Write([]byte{0})
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil)), nil
}

type RecoveryCatalogHead struct {
	Version              uint32
	History, Incarnation string
	Revision             uint64
	Key, SHA256          string
	Length               int
}
type RecoveryCatalogStore interface {
	GetRecoveryCatalog(context.Context, string) (RecoveryCatalogHead, error)
	CompareRecoveryCatalog(context.Context, RecoveryCatalogHead, RecoveryCatalogHead, WriterFence) error
}
