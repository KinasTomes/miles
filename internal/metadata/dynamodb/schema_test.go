package dynamodb

import (
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/KinasTomes/miles/internal/storage"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// Fail when infrastructure and request helpers drift; do not silently create a
// parallel inventory when the W4 adapter is introduced.
func TestInventoryMatchesTerraform(t *testing.T) {
	b, err := os.ReadFile("../../../infra/terraform/cloud/modules/data/main.tf")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`(?m)^\s*(\w+)\s*=\s*\{ hash = "([^"]+)", range = (null|"[^"]+"), range_type = "([SN])" \}`)
	actual := map[string]Table{}
	for _, m := range re.FindAllStringSubmatch(string(b), -1) {
		table := Table{Partition: m[2]}
		if m[3] != "null" {
			table.Sort, table.SortType = strings.Trim(m[3], `"`), m[4]
		}
		actual[m[1]] = table
	}
	if !reflect.DeepEqual(Tables(), actual) {
		t.Fatalf("schema/infrastructure drift: schema=%v infrastructure=%v", Tables(), actual)
	}
}
func TestKeysPreserveDomainGenerationAndIncarnationContract(t *testing.T) {
	a, err := ChunkKey(storage.ChunkKey{Domain: "default", Hash: strings.Repeat("a", 64)})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := ChunkKey(storage.ChunkKey{Domain: "isolated", Hash: strings.Repeat("a", 64)})
	if reflect.DeepEqual(a, b) || a.Attributes["chunk_id"].(*types.AttributeValueMemberS).Value != "default#"+strings.Repeat("a", 64) {
		t.Fatal("hash-only or domain-only identity", a, b)
	}
	p, _ := PackKey(storage.PackRef{ID: "pack", Generation: "old"})
	q, _ := PackKey(storage.PackRef{ID: "pack", Generation: "new"})
	if reflect.DeepEqual(p, q) {
		t.Fatal("generation lost")
	}
	o := storage.ObjectKey{Bucket: "demo", BucketID: "old", Key: "a//../đồ vật"}
	x, err := ObjectKey(o)
	if err != nil {
		t.Fatal(err)
	}
	o.BucketID = "new"
	y, _ := ObjectKey(o)
	if !reflect.DeepEqual(x, y) || x.Attributes["key"].(*types.AttributeValueMemberS).Value != o.Key {
		t.Fatal("object path normalized or physical incarnation key changed")
	}
	// Incarnation belongs to conditions, not the Objects physical key.
	for _, kind := range []byte{1, 2, 3, 4} {
		k, err := PageKey(kind, "owner", 1023)
		if err != nil || k.Attributes["page_no"].(*types.AttributeValueMemberN).Value != "1023" {
			t.Fatal(k, err)
		}
	}
	for _, kind := range []byte{0, 5} {
		if _, err := PageKey(kind, "owner", 0); err == nil {
			t.Fatal("unknown kind accepted")
		}
	}
	if _, err := PageKey(1, "owner", 1024); err == nil {
		t.Fatal("page bound ignored")
	}
}
func TestAuxiliaryNamespacesDoNotAlias(t *testing.T) {
	op, err := OperationKey(strings.Repeat("a", 32))
	if err != nil {
		t.Fatal(err)
	}
	seal, _ := ManifestSealKey(1, strings.Repeat("a", 32))
	part, _ := ManifestSealKey(2, strings.Repeat("a", 32))
	current, _ := PolicyKey(0)
	history, _ := PolicyKey(1)
	seen := map[string]bool{}
	for _, k := range []Key{op, seal, part, current, history} {
		v := k.Attributes["config_id"].(*types.AttributeValueMemberS).Value
		if seen[v] {
			t.Fatal("namespace collision", v)
		}
		seen[v] = true
	}
	if _, err := OperationKey(strings.Repeat("A", 32)); err == nil {
		t.Fatal("invalid mutation id")
	}
}
