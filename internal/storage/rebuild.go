package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// IndexRecord is an installation projection, never an online mutation or lease.
// Adapters whitelist kinds and decode the corresponding concrete record type.
type IndexRecord struct {
	Kind string
	IDs  []string
	Body []byte
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
