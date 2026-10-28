package dynamodb

import (
	"bytes"
	"regexp"
	"strconv"
	"time"

	"github.com/KinasTomes/miles/internal/manifest"
	"github.com/KinasTomes/miles/internal/storage"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func text(s string) types.AttributeValue { return &types.AttributeValueMemberS{Value: s} }
func number(n uint64) types.AttributeValue {
	return &types.AttributeValueMemberN{Value: strconv.FormatUint(n, 10)}
}
func checksum(c storage.Checksum) types.AttributeValue {
	return &types.AttributeValueMemberM{Value: map[string]types.AttributeValue{
		"algorithm": text(c.Algorithm), "type": text(c.Type), "value": text(c.Value), "part_count": number(uint64(c.PartCount)),
	}}
}

// ReferencePageItem is exactly keys + Binary body. Completion kinds 3/4 remain
// unavailable until their reviewed production codec exists; PageKey alone is
// only namespace preparation for those streams.
func ReferencePageItem(kind byte, page storage.ManifestPage) (Key, map[string]types.AttributeValue, error) {
	body, err := manifest.Encode(kind, page)
	if err != nil {
		return Key{}, nil, err
	}
	k, err := PageKey(kind, page.ManifestID, page.Number)
	if err != nil {
		return Key{}, nil, err
	}
	item := map[string]types.AttributeValue{}
	for name, v := range k.Attributes {
		item[name] = v
	}
	item["body"] = &types.AttributeValueMemberB{Value: body}
	size, err := ItemSizeUpperBound(item)
	if err != nil || size >= 400000 {
		return Key{}, nil, storage.ErrInvalid
	}
	return k, item, nil
}

// HeadItem encodes a complete published head. Input revision zero or an omitted
// descriptor is not a persisted item: a backend must derive/check those first.
func HeadItem(h storage.ObjectHead) (map[string]types.AttributeValue, error) {
	if err := h.ValidateManifestDescriptor(); err != nil {
		return nil, err
	}
	if h.Revision == 0 || !mutationID.MatchString(h.OpID) || h.Checksum.PartCount < 0 {
		return nil, storage.ErrInvalid
	}
	k, err := ObjectKey(h.Object)
	if err != nil {
		return nil, err
	}
	item := map[string]types.AttributeValue{}
	for name, v := range k.Attributes {
		item[name] = v
	}
	item["v"], item["bucket_id"], item["domain_id"] = number(uint64(h.Version)), text(h.Object.BucketID), text(h.Domain)
	item["revision"], item["op_id"], item["deleted"] = number(h.Revision), text(h.OpID), &types.AttributeValueMemberBOOL{Value: h.Deleted}
	item["size"] = number(uint64(h.Size))
	if h.Deleted {
		if h.ETag != "" || h.ETagKind != "" || h.Checksum != (storage.Checksum{}) || len(h.Metadata) != 0 {
			return nil, storage.ErrInvalid
		}
	} else {
		if h.ETag == "" {
			return nil, storage.ErrInvalid
		}
		index, err := manifest.EncodePageIndex(h)
		if err != nil {
			return nil, err
		}
		item["manifest_id"], item["page_count"], item["chunk_count"] = text(h.ManifestID), number(uint64(h.PageCount)), number(h.ChunkCount)
		item["page_index"] = &types.AttributeValueMemberB{Value: index}
		item["etag"], item["etag_kind"], item["checksum"] = text(h.ETag), text(h.ETagKind), checksum(h.Checksum)
		metadata := map[string]types.AttributeValue{}
		for name, value := range h.Metadata {
			metadata[name] = text(value)
		}
		item["metadata"] = &types.AttributeValueMemberM{Value: metadata}
	}
	if err := ValidateItem(item); err != nil {
		return nil, err
	}
	return item, nil
}

// OutboxItem encodes the existing dev envelope only. Full R2 effect projections,
// export state and registered authority are additional W4 integration fields.
func OutboxItem(e storage.OutboxEvent) (map[string]types.AttributeValue, error) {
	if err := e.Validate(); err != nil {
		return nil, err
	}
	item := map[string]types.AttributeValue{
		"event_id": text(e.ID), "v": number(uint64(e.Version)), "history_id": text(e.HistoryID), "recovery_epoch": number(e.Epoch),
		"entity": text(e.Entity), "action": text(e.Action), "entity_id": text(e.EntityID), "revision": number(e.Revision),
		"op_id": text(e.OpID), "payload_pointer": text(e.PayloadPointer), "payload_sha256": text(e.PayloadSHA256),
	}
	if e.Entity == "Objects" {
		item["bucket"], item["bucket_id"], item["key"] = text(e.Object.Bucket), text(e.Object.BucketID), text(e.Object.Key)
	}
	if err := ValidateItem(item); err != nil {
		return nil, err
	}
	return item, nil
}

var sha256ID = regexp.MustCompile(`^[0-9a-f]{64}$`)

// OutcomeItem prepares the result portion of an operation row for sizing.
// The W4 builder must merge retained reservation/attempt/authority bindings and
// validate its entire final image; this helper alone is not a publishable bundle.
func OutcomeItem(historyID, inputDigest string, result storage.CommitResult) (Key, map[string]types.AttributeValue, error) {
	if !mutationID.MatchString(historyID) || !sha256ID.MatchString(inputDigest) {
		return Key{}, nil, storage.ErrInvalid
	}
	head, err := HeadItem(result.Head)
	if err != nil {
		return Key{}, nil, err
	}
	k, err := OperationKey(result.Head.OpID)
	if err != nil {
		return Key{}, nil, err
	}
	item := map[string]types.AttributeValue{"config_id": text(k.Attributes["config_id"].(*types.AttributeValueMemberS).Value),
		"record_kind": text("storage_operation_v1"), "state": text("Committed"), "history_id": text(historyID),
		"op_id": text(result.Head.OpID), "input_sha256": text(inputDigest), "head": &types.AttributeValueMemberM{Value: head}}
	if err := ValidateItem(item); err != nil {
		return Key{}, nil, err
	}
	return k, item, nil
}

// ReceiptAttributes is embedded in Uploads, not a new receipt table. Validate
// the entire final Uploads image too; passing this alone does not bound that row.
func ReceiptAttributes(r storage.CompletedReceipt) (map[string]types.AttributeValue, error) {
	if r.Version != storage.ModelVersion || r.UploadID == "" || r.BucketID == "" || r.Key == "" || r.Initiator == "" || !mutationID.MatchString(r.OpID) || !sha256ID.MatchString(r.InputDigest) || !ownerID.MatchString(r.ManifestID) || r.ETag == "" || r.Checksum.PartCount < 0 || r.ExpiresAt.IsZero() {
		return nil, storage.ErrInvalid
	}
	item := map[string]types.AttributeValue{"v": number(uint64(r.Version)), "upload_id": text(r.UploadID), "bucket_id": text(r.BucketID), "key": text(r.Key),
		"initiator": text(r.Initiator), "op_id": text(r.OpID), "input_digest": text(r.InputDigest), "manifest_id": text(r.ManifestID),
		"etag": text(r.ETag), "checksum": checksum(r.Checksum), "expires_at": text(r.ExpiresAt.UTC().Format(time.RFC3339Nano))}
	if err := ValidateItem(item); err != nil {
		return nil, err
	}
	return item, nil
}

// CloneBinary returns owned bytes when decoding a native attribute. Absence is
// an error even when a present binary field can legitimately be empty.
func CloneBinary(v types.AttributeValue) ([]byte, error) {
	b, ok := v.(*types.AttributeValueMemberB)
	if !ok || b == nil {
		return nil, storage.ErrInvalid
	}
	return bytes.Clone(b.Value), nil
}
