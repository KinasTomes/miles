package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"unicode/utf8"

	"github.com/KinasTomes/miles/internal/storage"
)

// Nil children denote a typed leaf. These private schemas are never mutated.
type policyObject map[string]policyObject

var globalPolicyFields = policyObject{
	"tiering": {
		"enabled": nil, "warm_storage_class": nil, "cold_storage_class": nil,
		"cold_after_days": nil, "min_pack_live_ratio": nil, "auto_restore": nil,
		"auto_restore_days": nil, "auto_restore_tier": nil,
	},
	"compression": {"enabled": nil, "codec": nil, "level": nil, "min_saving_ratio": nil},
	"chunking":    {"min_bytes": nil, "avg_bytes": nil, "max_bytes": nil},
}

var bucketPolicyFields = policyObject{
	"cache": {"admit_on_write": nil, "readahead_chunks": nil},
}

func updateFields(base policyObject, bucket bool) policyObject {
	fields := make(policyObject, len(base)+2)
	for name, child := range base {
		fields[name] = child
	}
	fields["expected_revision"] = nil
	if bucket {
		fields["expected_bucket_id"] = nil
	}
	return fields
}

func (v PolicyValidator) parsePolicyJSON(data []byte, fields policyObject, complete bool) (map[string]json.RawMessage, error) {
	if !v.ready {
		return nil, ErrPolicyDeferred
	}
	if len(data) > v.limits.MaxJSONBytes {
		return nil, fmt.Errorf("%w: supplied policy JSON budget exceeded", ErrPolicyInvalid)
	}
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("%w: policy JSON must be UTF-8", ErrPolicyInvalid)
	}
	return strictPolicyObject(data, fields, complete)
}

func strictPolicyObject(data []byte, fields policyObject, complete bool) (map[string]json.RawMessage, error) {
	d := json.NewDecoder(bytes.NewReader(data))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return nil, fmt.Errorf("%w: expected policy object", ErrPolicyInvalid)
	}
	values := make(map[string]json.RawMessage)
	for d.More() {
		token, err := d.Token()
		if err != nil {
			return nil, fmt.Errorf("%w: policy key", ErrPolicyInvalid)
		}
		name, ok := token.(string)
		child, allowed := fields[name]
		_, duplicate := values[name]
		if !ok || !allowed || duplicate {
			return nil, fmt.Errorf("%w: unknown/duplicate policy field %q", ErrPolicyInvalid, name)
		}
		var value json.RawMessage
		if err := d.Decode(&value); err != nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return nil, fmt.Errorf("%w: invalid/null policy field %q", ErrPolicyInvalid, name)
		}
		if child != nil {
			if _, err := strictPolicyObject(value, child, complete); err != nil {
				return nil, err
			}
		}
		values[name] = value
	}
	if _, err := d.Token(); err != nil {
		return nil, fmt.Errorf("%w: policy object closing delimiter", ErrPolicyInvalid)
	}
	if err := d.Decode(new(json.RawMessage)); err != io.EOF {
		return nil, fmt.Errorf("%w: trailing JSON content", ErrPolicyInvalid)
	}
	if complete && len(values) != len(fields) {
		return nil, fmt.Errorf("%w: incomplete persisted policy; defaults apply only at creation", ErrPolicyInvalid)
	}
	return values, nil
}

// DecodeGlobal loads a COMPLETE existing shared snapshot. Missing fields/history
// are never repaired with defaults. Output owns no reference to input Document.
func (v PolicyValidator) DecodeGlobal(record storage.PolicySnapshot) (GlobalSnapshot, error) {
	if record.Version != storage.ModelVersion || !revisionValid(record.Revision) {
		return GlobalSnapshot{}, fmt.Errorf("%w: policy snapshot envelope", ErrPolicyInvalid)
	}
	if _, err := v.parsePolicyJSON(record.Document, globalPolicyFields, true); err != nil {
		return GlobalSnapshot{}, err
	}
	var p GlobalPolicy
	if err := json.Unmarshal(record.Document, &p); err != nil {
		return GlobalSnapshot{}, fmt.Errorf("%w: policy types: %v", ErrPolicyInvalid, err)
	}
	return v.NewGlobalSnapshot(record.Revision, p)
}

// ResolveGlobal decodes a MetadataStore lookup result, not a second lookup port.
// The caller passes the actual lookup error unchanged; a missing historical
// revision stays missing. A mismatched returned revision also fails closed.
// Requested revision zero is the existing port's current selector only.
func (v PolicyValidator) ResolveGlobal(requested uint64, record storage.PolicySnapshot, lookupErr error) (GlobalSnapshot, error) {
	if lookupErr != nil {
		return GlobalSnapshot{}, lookupErr
	}
	if requested > storage.MaxPolicyRevision || requested != 0 && record.Revision != requested {
		return GlobalSnapshot{}, fmt.Errorf("%w: exact history revision mismatch", ErrPolicyInvalid)
	}
	return v.DecodeGlobal(record)
}

// DecodeBucket requires complete cache fields; bucket identity/revision come
// from the caller's authoritative bucket record, not from JSON defaults.
func (v PolicyValidator) DecodeBucket(bucketID string, revision uint64, data []byte) (BucketSnapshot, error) {
	if _, err := v.parsePolicyJSON(data, bucketPolicyFields, true); err != nil {
		return BucketSnapshot{}, err
	}
	var p struct {
		Cache CachePolicy `json:"cache"`
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return BucketSnapshot{}, fmt.Errorf("%w: cache policy types", ErrPolicyInvalid)
	}
	return v.NewBucketSnapshot(bucketID, revision, p.Cache)
}
