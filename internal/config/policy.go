package config

import (
	"encoding/json"
	"errors"

	"github.com/KinasTomes/miles/internal/storage"
)

// These concrete config APIs are proposals for consumer review, not a frozen
// storage format. MetadataStore remains the authority for CAS/history/outbox.
// Callers must authorize before using these trusted internal APIs to disclose
// resource/revision information; config does not implement Admin authentication.
var (
	ErrPolicyInvalid           = storage.ErrInvalid
	ErrPolicyConflict          = storage.ErrConflict
	ErrPolicyDeferred          = errors.New("policy validation requires reviewed limits and restore-tier rules")
	ErrPolicyRevisionExhausted = errors.New("policy revision exhausted")
)

type GlobalPolicy struct {
	Tiering     TieringPolicy     `json:"tiering"`
	Compression CompressionPolicy `json:"compression"`
	Chunking    ChunkingPolicy    `json:"chunking"`
}

type TieringPolicy struct {
	Enabled          bool                 `json:"enabled"`
	WarmStorageClass storage.StorageClass `json:"warm_storage_class"`
	ColdStorageClass storage.StorageClass `json:"cold_storage_class"`
	ColdAfterDays    int64                `json:"cold_after_days"`
	MinPackLiveRatio Decimal              `json:"min_pack_live_ratio"`
	AutoRestore      bool                 `json:"auto_restore"`
	AutoRestoreDays  int64                `json:"auto_restore_days"`
	AutoRestoreTier  string               `json:"auto_restore_tier"`
}

type CompressionPolicy struct {
	Enabled        bool    `json:"enabled"`
	Codec          string  `json:"codec"`
	Level          int64   `json:"level"`
	MinSavingRatio Decimal `json:"min_saving_ratio"`
}

type ChunkingPolicy struct {
	MinBytes int64 `json:"min_bytes"`
	AvgBytes int64 `json:"avg_bytes"`
	MaxBytes int64 `json:"max_bytes"`
}

type CachePolicy struct {
	AdmitOnWrite    bool  `json:"admit_on_write"`
	ReadaheadChunks int64 `json:"readahead_chunks"`
}

// Snapshots contain only value fields and immutable strings, never caller-owned
// maps/slices. Accessors return values. Updates produce proposals, not mutation
// of a pinned snapshot or an assertion that metadata committed a revision.
type GlobalSnapshot struct {
	revision uint64
	policy   GlobalPolicy
	valid    bool
}

func (s GlobalSnapshot) Revision() uint64     { return s.revision }
func (s GlobalSnapshot) Policy() GlobalPolicy { return s.policy }

func (s GlobalSnapshot) StorageSnapshot() (storage.PolicySnapshot, error) {
	if !s.valid {
		return storage.PolicySnapshot{}, ErrPolicyInvalid
	}
	doc, err := json.Marshal(s.policy)
	if err != nil {
		return storage.PolicySnapshot{}, err
	}
	return storage.PolicySnapshot{Version: storage.ModelVersion, Revision: s.revision, Document: doc}, nil
}

type BucketSnapshot struct {
	bucketID string
	revision uint64
	cache    CachePolicy
	valid    bool
}

func (s BucketSnapshot) BucketID() string   { return s.bucketID }
func (s BucketSnapshot) Revision() uint64   { return s.revision }
func (s BucketSnapshot) Cache() CachePolicy { return s.cache }

// EffectiveSnapshot pins both independent revisions. Cache is bucket-only; no
// bucket field can replace compression, chunking, tiering, or dedup identity.
type EffectiveSnapshot struct {
	global GlobalSnapshot
	bucket BucketSnapshot
}

func EffectivePolicy(global GlobalSnapshot, bucket BucketSnapshot) (EffectiveSnapshot, error) {
	if !global.valid || !bucket.valid {
		return EffectiveSnapshot{}, ErrPolicyInvalid
	}
	return EffectiveSnapshot{global: global, bucket: bucket}, nil
}

func (s EffectiveSnapshot) Global() GlobalSnapshot { return s.global }
func (s EffectiveSnapshot) Bucket() BucketSnapshot { return s.bucket }
