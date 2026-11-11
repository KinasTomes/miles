package dynamodb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"time"

	"github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/lifecycle"
	"github.com/KinasTomes/miles/internal/manifest"
	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// Check must attest persistent isolation of ALL old workers and terminal
// reconciliation of S3 destructive actions, including actions after the cut.
// Observe binds external control records to the SAME native transaction. A
// database lease expiry, HEAD or caller-supplied boolean is not this proof.
type OfflineRecoveryGuard interface {
	Check(context.Context, s.IndexInstall, Observe) error
}
type indexInstallState struct {
	Version                            uint32
	Request                            s.IndexInstall
	State, LastKey, LastDigest, Digest string
	Count                              uint64
}

func indexInstallKey() Key { return stringKey("SystemConfig", "config_id", "recovery/index-install") }
func (m *Store) offlineProof(t *transaction, r s.IndexInstall) error {
	if m.offlineRecovery == nil {
		return s.ErrFenced
	}
	return m.offlineRecovery.Check(t.ctx, r, func(k Key) (Item, error) {
		row, e := t.observe(k)
		if e != nil {
			return nil, e
		}
		if row.after == nil {
			return nil, s.ErrNotFound
		}
		return copyItem(row.after), nil
	})
}
func validInstall(r s.IndexInstall) bool {
	return mutationID.MatchString(r.SourceHistory) && mutationID.MatchString(r.History) && mutationID.MatchString(r.Incarnation) && r.History != r.SourceHistory && r.Incarnation != r.SourceIncarnation && r.SourceIncarnation != "" && validRecoveryDigest(r.TargetSHA256) && validRecoveryDigest(r.GenesisSHA256) && r.GenesisLength > 0 && r.GenesisLength <= 256<<10 && validRecoveryDigest(r.RecordsSHA256)
}
func validRecoveryDigest(v string) bool {
	return len(v) == 64 && mutationID.MatchString(v[:32]) && mutationID.MatchString(v[32:])
}

// Begin requires a dedicated empty table inventory. This is an offline empty-
// target audit, never a multi-table snapshot used for checkpoint completeness.
func (m *Store) BeginIndexInstall(ctx context.Context, r s.IndexInstall) error {
	if !validInstall(r) || m.offlineRecovery == nil {
		return s.ErrFenced
	}
	var existing indexInstallState
	err := m.view(ctx, func(t *transaction) error {
		var e error
		existing, e = r1Read[indexInstallState](t, indexInstallKey())
		return e
	})
	if err == nil {
		if existing.Request != r {
			return s.ErrConflict
		}
		return nil
	}
	if !errors.Is(err, s.ErrNotFound) {
		return err
	}
	for table, physical := range m.tables {
		out, e := m.api.Scan(ctx, &ddb.ScanInput{TableName: aws.String(physical), ConsistentRead: aws.Bool(true), Limit: aws.Int32(1)})
		if e != nil {
			return backend(e)
		}
		if out == nil {
			return s.ErrUnavailable
		}
		if len(out.Items) != 0 || len(out.LastEvaluatedKey) != 0 {
			return fmt.Errorf("%w: nonempty recovery target %s", s.ErrConflict, table)
		}
	}
	return m.update(ctx, func(t *transaction) error {
		if e := m.offlineProof(t, r); e != nil {
			return e
		}
		if _, e := r1Read[indexInstallState](t, indexInstallKey()); e == nil {
			return s.ErrConflict
		} else if !errors.Is(e, s.ErrNotFound) {
			return e
		}
		if _, e := r1Read[s.R1Gate](t, r1GateKey()); e == nil {
			return s.ErrConflict
		} else if !errors.Is(e, s.ErrNotFound) {
			return e
		}
		if e := r1Put(t, r1GateKey(), s.R1Gate{Version: 1, HistoryID: r.History, Incarnation: r.Incarnation, State: "Closing", Revision: 1, RecoveryVersion: 1}); e != nil {
			return e
		}
		return r1Put(t, indexInstallKey(), indexInstallState{Version: 1, Request: r, State: "Installing"})
	})
}

func (m *Store) StageIndexRecord(ctx context.Context, r s.IndexInstall, record s.IndexRecord) error {
	if !validInstall(r) {
		return s.ErrInvalid
	}
	return m.update(ctx, func(t *transaction) error {
		state, e := r1Read[indexInstallState](t, indexInstallKey())
		if e != nil {
			return e
		}
		if state.Request != r || state.State != "Installing" && state.State != "Ready" {
			return s.ErrFenced
		}
		key := s.IndexRecordKey(record.Kind, record.IDs)
		single, e := s.IndexRecordDigest("", record)
		if e != nil {
			return e
		}
		ackKey := stringKey("SystemConfig", "config_id", "recovery/install-record/"+single)
		if key <= state.LastKey {
			ack, e := r1Read[struct{ Key string }](t, ackKey)
			if e != nil || ack.Key != key {
				return s.ErrConflict
			}
			return nil
		}
		if state.State != "Installing" {
			return s.ErrFenced
		}
		g, e := r1Read[s.R1Gate](t, r1GateKey())
		if e != nil {
			return e
		}
		if g.HistoryID != r.History || g.Incarnation != r.Incarnation || g.State != "Closing" || g.Revision != 1 {
			return s.ErrFenced
		}
		if e = m.stageRecoveryRecord(t, record); e != nil {
			return e
		}
		state.Digest, e = s.IndexRecordDigest(state.Digest, record)
		if e != nil {
			return e
		}
		state.Count++
		state.LastKey = key
		state.LastDigest = single
		if e = r1Put(t, ackKey, struct{ Key string }{key}); e != nil {
			return e
		}
		return r1Put(t, indexInstallKey(), state)
	})
}
func decodeRecoveryRecord(body []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	return d.Decode(v)
}
func (m *Store) stageRecoveryRecord(t *transaction, r s.IndexRecord) error {
	ids := r.IDs
	if (r.Kind == "uploads" || r.Kind == "receipts") && len(ids) != 1 {
		return s.ErrInvalid
	}
	counts := map[string]int{"users": 1, "buckets": 1, "grants": 2, "credentials": 1, "credential-security": 1, "admin-tokens": 1, "heads": 3, "chunks": 2, "packs": 2, "quotas": 1, "bucket-namespace": 1, "policies": 1, "bucket-cache-history": 2, "policy-current": 0, "bucket-cache-current": 1, "full-receipt": 1, "upload-record": 1}
	if n, ok := counts[r.Kind]; ok && len(ids) != n {
		return s.ErrInvalid
	}
	var value any
	switch r.Kind {
	case "uploads":
		value = &s.Completion{}
	case "receipts":
		value = &s.CompletedReceipt{}
	case "users":
		value = &s.User{}
	case "buckets":
		value = &s.Bucket{}
	case "grants":
		value = &s.Grant{}
	case "credentials":
		value = &s.Credential{}
	case "credential-security":
		value = &s.CredentialSecurity{}
	case "admin-tokens":
		value = &s.AdminToken{}
	case "heads":
		value = &s.ObjectHead{}
	case "chunks":
		value = &s.Chunk{}
	case "packs":
		value = &s.PackInfo{}
	case "quotas":
		value = &s.LogicalQuota{}
	case "bucket-namespace":
		value = &s.BucketNamespace{}
	case "policies", "bucket-cache-history":
		value = &s.PolicySnapshot{}
	case "policy-current", "bucket-cache-current":
		value = new(uint64)
	case "full-receipt":
		value = &s.CompletionReceipt{}
	case "upload-record":
		value = &s.MultipartUpload{}
	case "recovery-lifecycle":
		if len(ids) != 1 {
			return s.ErrInvalid
		}
		var a lifecycle.Action
		if decodeRecoveryRecord(r.Body, &a) != nil || a.Fence != (s.WriterFence{}) || a.ValidateSchema() != nil {
			return s.ErrInvalid
		}
		return r1Put(t, stringKey("SystemConfig", "config_id", "recovery/lifecycle/"+ids[0]), struct {
			Version uint32
			Fact    lifecycle.Action
		}{1, a})
	case "recovery-refcount":
		if len(ids) != 2 {
			return s.ErrInvalid
		}
		var count int64
		if decodeRecoveryRecord(r.Body, &count) != nil || count < 0 {
			return s.ErrInvalid
		}
		return r1Put(t, stringKey("SystemConfig", "config_id", "recovery/refcount/"+ids[0]+"/"+ids[1]), struct{ Count int64 }{count})
	case "manifest-page":
		var p s.ManifestPage
		if len(ids) != 2 || decodeRecoveryRecord(r.Body, &p) != nil || p.ManifestID != ids[0] || strconv.FormatUint(uint64(p.Number), 10) != ids[1] {
			return s.ErrInvalid
		}
		body, e := manifest.Encode(1, p)
		if e != nil {
			return e
		}
		return t.setPage(pageKey(p.ManifestID, p.Number), body)
	case "completion-page":
		if len(ids) != 3 {
			return s.ErrInvalid
		}
		kind, e := strconv.ParseUint(ids[0], 10, 8)
		n, e2 := strconv.ParseUint(ids[2], 10, 32)
		var body []byte
		if e != nil || e2 != nil || (kind != 3 && kind != 4) || decodeRecoveryRecord(r.Body, &body) != nil {
			return s.ErrInvalid
		}
		if _, e = manifest.DecodeCompletionPage(body, byte(kind), ids[1], uint32(n)); e != nil {
			return e
		}
		k, e := PageKey(byte(kind), ids[1], uint32(n))
		if e != nil {
			return e
		}
		row, e := t.observe(k)
		if e != nil {
			return e
		}
		item := copyItem(k.Attributes)
		item["body"] = &types.AttributeValueMemberB{Value: append([]byte(nil), body...)}
		row.after = item
		row.dirty = true
		return nil
	default:
		return s.ErrInvalid
	}
	if e := decodeRecoveryRecord(r.Body, value); e != nil {
		return s.ErrInvalid
	}
	switch v := value.(type) {
	case *s.Completion:
		if v.State != "Completed" && v.State != "Aborted" {
			return s.ErrInvalid
		}
	case *s.Credential:
		if v.Active {
			return s.ErrInvalid
		}
	case *s.AdminToken:
		if v.Active {
			return s.ErrInvalid
		}
	case *s.PackInfo:
		if v.Writer != (s.WriterFence{}) {
			return s.ErrInvalid
		}
	case *s.LogicalQuota:
		if v.Reserved != 0 || v.Staged != 0 || v.ActiveUploads != 0 || v.Used < 0 || v.Used > v.Limit {
			return s.ErrInvalid
		}
	case *s.MultipartUpload:
		if v.State != "Completed" && v.State != "Aborted" || v.Writer != (s.WriterFence{}) || v.ClosedWriter != (s.WriterFence{}) || v.Publishers != 0 {
			return s.ErrInvalid
		}
	}
	address := logicalRecord(r.Kind, ids)
	if r.Kind == "policies" {
		revision, e := strconv.ParseUint(ids[0], 16, 64)
		if e != nil {
			return s.ErrInvalid
		}
		address = policyKey(revision)
	}
	return put(t, address, reflect.ValueOf(value).Elem().Interface())
}
func (m *Store) FinishIndexInstall(ctx context.Context, r s.IndexInstall) error {
	if !validInstall(r) {
		return s.ErrInvalid
	}
	return m.update(ctx, func(t *transaction) error {
		state, e := r1Read[indexInstallState](t, indexInstallKey())
		if e != nil {
			return e
		}
		if state.Request != r || state.Digest != r.RecordsSHA256 {
			return s.ErrConflict
		}
		if state.State == "Ready" {
			g, e := r1Read[s.R1Gate](t, r1GateKey())
			if e != nil {
				return e
			}
			if g.HistoryID != r.History || g.Incarnation != r.Incarnation || g.State != "Open" {
				return s.ErrFenced
			}
			return nil
		}
		if state.State != "Installing" {
			return s.ErrFenced
		}
		if e = m.offlineProof(t, r); e != nil {
			return e
		}
		g, e := r1Read[s.R1Gate](t, r1GateKey())
		if e != nil {
			return e
		}
		if g.State != "Closing" || g.Incarnation != r.Incarnation || g.HistoryID != r.History || g.Revision != 1 {
			return s.ErrFenced
		}
		revision, e := get[uint64](t, "policy-current")
		if e != nil || revision == 0 {
			return s.ErrInvalid
		}
		policy, e := get[s.PolicySnapshot](t, policyKey(revision))
		if e != nil {
			return e
		}
		validator, _ := config.NewPolicyValidator(config.DSGPolicyLimits())
		if _, e = validator.DecodeGlobal(policy); e != nil {
			return s.ErrInvalid
		}
		g.State = "Open"
		t.beforeDispatch = func(ctx context.Context) error {
			if e := s.CheckIndexReadiness(ctx, r); e != nil {
				return e
			}
			var e error
			t.dispatchDeadline, e = s.IndexReadinessDeadline(ctx, r)
			return e
		}
		g.Revision++
		g.RecoveryEnabled = true
		if e = r1Put(t, r1GateKey(), g); e != nil {
			return e
		}
		rs := s.RecoveryState{Version: 1, HistoryID: r.History, Incarnation: r.Incarnation, GenesisSHA256: r.GenesisSHA256, GenesisLength: r.GenesisLength, Revision: 1, NextCutAt: m.now().Add(time.Hour).UTC()}
		if e = r1Put(t, recoveryStateKey(r.History), rs); e != nil {
			return e
		}
		state.State = "Ready"
		return r1Put(t, indexInstallKey(), state)
	})
}

var _ s.IndexInstaller = (*Store)(nil)
