package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

var ErrRecoveryCloseTimeout = errors.New("recovery close wait budget exhausted")

const RecoveryCloseBudget = 120 * time.Second
const RecoveryCutInterval = time.Hour

// RecoveryEpoch is closure/progress, not a checkpoint or a physical retention
// certificate. Cursor/count/hash only advance after native base pages verify.
type RecoveryEpoch struct {
	Version                                uint32
	HistoryID, Incarnation, State, CloseID string
	Epoch, Revision                        uint64
	Worker                                 WriterFence
	StartedAt, CutAt                       time.Time
	Phase, Cursor, ProofSHA256             string
	ProducerCount, EventCount              uint64
	EventsSHA256                           string
	SealKey, SealSHA256                    string
}

type RecoveryState struct {
	Version                uint32
	HistoryID, Incarnation string
	GenesisSHA256          string
	GenesisLength          int
	NextToSeal, Revision   uint64
	SealKey, SealSHA256    string
	NextCutEpoch           uint64
	NextCutAt              time.Time
}

type RecoveryCutRequest struct {
	HistoryID string
	Epoch     uint64
	CloseID   string
	Worker    WriterFence
}

// RecoveryStore uses the same R1 authority and metadata tables as producers.
// Step verifies one bounded cohort/mutation page, then atomically advances it.
// An error/unknown outcome never grants permission to reopen an admission gate.
type RecoveryStore interface {
	OutboxExportStore
	GetR1Gate(context.Context) (R1Gate, error)
	CheckRecoveryWorker(context.Context, WriterFence) error
	InitializeRecovery(context.Context, RecoveryState) error
	GetRecoveryState(context.Context, string) (RecoveryState, error)
	RecoveryCutDue(context.Context, string, WriterFence) (uint64, bool, error)
	GetRecoveryEpoch(context.Context, string, uint64) (RecoveryEpoch, error)
	BeginRecoveryCut(context.Context, RecoveryCutRequest) (RecoveryEpoch, error)
	StepRecoveryCut(context.Context, RecoveryCutRequest) (RecoveryEpoch, error)
	AbortRecoveryCut(context.Context, RecoveryCutRequest) error
	PublishRecoverySeal(context.Context, RecoveryCutRequest, uint64, string, string) error
}

func (s RecoveryState) Validate() error {
	if s.Version != 1 || !hex128(s.HistoryID) || s.Incarnation == "" || !sha256Hex.MatchString(s.GenesisSHA256) || s.GenesisLength < 1 || s.GenesisLength > 256<<10 || s.NextToSeal > MaxPolicyRevision || s.Revision == 0 || s.Revision > MaxPolicyRevision || s.NextCutEpoch > MaxPolicyRevision || s.NextToSeal > s.NextCutEpoch || s.NextCutAt.IsZero() || s.NextCutAt.Year() < 1 || s.NextCutAt.Year() > 9999 {
		return ErrInvalid
	}
	if s.NextToSeal == 0 {
		if s.SealKey != "" || s.SealSHA256 != "" {
			return ErrInvalid
		}
	} else if s.SealKey == "" || !sha256Hex.MatchString(s.SealSHA256) {
		return ErrInvalid
	}
	return nil
}
func (r RecoveryCutRequest) Validate() error {
	if !hex128(r.HistoryID) || r.Epoch >= MaxPolicyRevision || !hex128(r.CloseID) || r.Worker.LeaseID == "" || r.Worker.Epoch == 0 || r.Worker.ServiceIncarnation == "" {
		return ErrInvalid
	}
	return nil
}

func (e RecoveryEpoch) Validate() error {
	if e.Version != 1 || !hex128(e.HistoryID) || e.Incarnation == "" || e.Epoch >= MaxPolicyRevision || e.Revision == 0 || e.Revision > MaxPolicyRevision || !hex128(e.CloseID) || e.Worker.LeaseID == "" || e.Worker.ServiceIncarnation != e.Incarnation || e.Worker.Epoch == 0 || e.StartedAt.IsZero() || e.ProducerCount > MaxPolicyRevision || e.EventCount > MaxPolicyRevision {
		return ErrInvalid
	}
	if e.ProofSHA256 != "" && !sha256Hex.MatchString(e.ProofSHA256) || e.EventsSHA256 != "" && !sha256Hex.MatchString(e.EventsSHA256) {
		return ErrInvalid
	}
	switch e.State {
	case "Open":
		if e.Phase != "" || e.Cursor != "" {
			return ErrInvalid
		}
	case "Closing":
		if e.Phase != "producers" && e.Phase != "mutations" || !e.CutAt.IsZero() {
			return ErrInvalid
		}
	case "Closed", "Sealed":
		if e.Phase != "complete" || e.Cursor != "" || e.CutAt.IsZero() || (e.ProducerCount == 0) != (e.ProofSHA256 == "") || (e.EventCount == 0) != (e.EventsSHA256 == "") {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	if e.State == "Sealed" {
		if e.SealKey == "" || !sha256Hex.MatchString(e.SealSHA256) {
			return ErrInvalid
		}
	} else if e.SealKey != "" || e.SealSHA256 != "" {
		return ErrInvalid
	}
	return nil
}

// RecoveryDigest is an ordered, framed digest chain, not wall/flush ordering.
// The adapter derives expected events from atomic mutation membership; the
// exporter independently enumerates base Outbox and must reproduce this chain.
func RecoveryDigest(previous string, v any) (string, error) {
	if previous != "" && !sha256Hex.MatchString(previous) {
		return "", ErrInvalid
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", ErrInvalid
	}
	h := sha256.New()
	h.Write([]byte("miles-r2-membership-v1\x00"))
	h.Write([]byte(previous))
	h.Write([]byte{0})
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil)), nil
}
