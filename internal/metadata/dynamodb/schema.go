package dynamodb

import (
	"fmt"
	"regexp"
	"strconv"

	"github.com/KinasTomes/miles/internal/storage"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

type Table struct{ Partition, Sort, SortType string }

// Tables returns the deployed inventory's logical names, without an environment
// prefix. No GSI is required for authoritative reads in the W4 baseline.
func Tables() map[string]Table {
	return map[string]Table{
		"Users": {Partition: "user_id"}, "Buckets": {Partition: "bucket"},
		"BucketGrants": {"bucket_id", "user_id", "S"}, "AdminTokens": {Partition: "token_id"},
		"Credentials": {Partition: "access_key_id"}, "SystemConfig": {Partition: "config_id"},
		"Objects": {"bucket", "key", "S"}, "ManifestPages": {"manifest_id", "page_no", "N"},
		"Chunks": {Partition: "chunk_id"}, "Packs": {"pack_id", "generation", "S"},
		"Uploads": {Partition: "upload_id"}, "UploadParts": {"upload_id", "part_number", "N"},
		"UploadPartPages": {"part_manifest_id", "page_no", "N"}, "CompletionPages": {"completion_id", "page_no", "N"},
		"Outbox": {Partition: "event_id"}, "RecoveryEpochs": {Partition: "history_epoch_key"},
		"QuotaReservations": {Partition: "reservation_id"}, "Leases": {"resource_id", "lease_id", "S"},
		"Jobs": {Partition: "job_id"}, "Nodes": {Partition: "node_id"},
	}
}

// Key binds a logical table name to native SDK attributes. The runtime resolves
// the table name through its explicit environment inventory, not a guessed ARN.
type Key struct {
	Table      string
	Attributes map[string]types.AttributeValue
}

func stringKey(table, name, value string) Key {
	return Key{table, map[string]types.AttributeValue{name: &types.AttributeValueMemberS{Value: value}}}
}

func ChunkKey(k storage.ChunkKey) (Key, error) {
	id, err := k.PartitionKey()
	if err != nil {
		return Key{}, err
	}
	return stringKey("Chunks", "chunk_id", id), nil
}
func ObjectKey(k storage.ObjectKey) (Key, error) {
	if err := k.Validate(); err != nil {
		return Key{}, err
	}
	if len(k.Bucket) > 2048 || len(k.Key) > 1024 {
		return Key{}, storage.ErrInvalid
	}
	out := stringKey("Objects", "bucket", k.Bucket)
	out.Attributes["key"] = &types.AttributeValueMemberS{Value: k.Key}
	return out, nil
}
func PackKey(k storage.PackRef) (Key, error) {
	if err := k.Validate(); err != nil {
		return Key{}, err
	}
	out := stringKey("Packs", "pack_id", k.ID)
	out.Attributes["generation"] = &types.AttributeValueMemberS{Value: k.Generation}
	return out, nil
}

var ownerID = regexp.MustCompile(`^[a-z0-9_-]{1,64}$`)
var mutationID = regexp.MustCompile(`^[0-9a-f]{32}$`)

// PageKey supports all frozen page namespaces. The codec must also verify the
// kind/owner/number in body before any item is accepted.
func PageKey(kind byte, owner string, number uint32) (Key, error) {
	if !ownerID.MatchString(owner) || number >= storage.MaxManifestPages {
		return Key{}, storage.ErrInvalid
	}
	var table, name string
	switch kind {
	case 1:
		table, name = "ManifestPages", "manifest_id"
	case 2:
		table, name = "UploadPartPages", "part_manifest_id"
	case 3, 4:
		table, name = "CompletionPages", "completion_id"
	default:
		return Key{}, storage.ErrInvalid
	}
	out := stringKey(table, name, owner)
	out.Attributes["page_no"] = &types.AttributeValueMemberN{Value: strconv.FormatUint(uint64(number), 10)}
	return out, nil
}

// Reserved SystemConfig namespaces are adapter records, never global-policy
// entities. Their mutations must be included in the owning R2 bundle.
func OperationKey(opID string) (Key, error) {
	if !mutationID.MatchString(opID) {
		return Key{}, storage.ErrInvalid
	}
	return stringKey("SystemConfig", "config_id", "storage/op/"+opID), nil
}
func ManifestSealKey(kind byte, owner string) (Key, error) {
	if !ownerID.MatchString(owner) || (kind != 1 && kind != 2) {
		return Key{}, storage.ErrInvalid
	}
	return stringKey("SystemConfig", "config_id", fmt.Sprintf("storage/manifest/%d/%s", kind, owner)), nil
}
func PolicyKey(revision uint64) (Key, error) {
	if revision > storage.MaxPolicyRevision {
		return Key{}, storage.ErrInvalid
	}
	id := "global-policy"
	if revision != 0 {
		id = fmt.Sprintf("global-policy/revision/%020d", revision)
	}
	return stringKey("SystemConfig", "config_id", id), nil
}
