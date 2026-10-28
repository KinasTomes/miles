package dynamodb

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"unicode/utf8"

	"github.com/KinasTomes/miles/internal/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const MaxItemBytes = 400 << 10
const MaxTransactionBytes = 4 << 20
const MaxTransactionActions = 100

var integer = regexp.MustCompile(`^-?(0|[1-9][0-9]{0,37})$`)
var attemptToken = regexp.MustCompile(`^[0-9a-f]{36}$`)

// ItemSizeUpperBound measures native attributes, not JSON/base64 wire length.
// Integers are conservatively charged len(decimal)+2 bytes; decimal/exponent N
// are rejected until a reviewed serializer supports them. Collections include
// DynamoDB's 3-byte container and 1-byte per-element overhead. This is a size
// guard for prepared items, not a replacement for record/schema validation.
func ItemSizeUpperBound(item map[string]types.AttributeValue) (int, error) {
	return mapSize(item, 0, false)
}
func mapSize(item map[string]types.AttributeValue, depth int, nested bool) (int, error) {
	if depth > 32 {
		return 0, storage.ErrInvalid
	}
	size := 0
	if nested {
		size = 3 + len(item)
	}
	for name, v := range item {
		if name == "" || len(name) > 65535 || !utf8.ValidString(name) {
			return 0, storage.ErrInvalid
		}
		n, err := valueSize(v, depth)
		if err != nil {
			return 0, err
		}
		size += len(name) + n
	}
	return size, nil
}
func numberSize(s string) (int, error) {
	if !integer.MatchString(s) || s == "-0" {
		return 0, fmt.Errorf("%w: only canonical integer N supported", storage.ErrInvalid)
	}
	return len(s) + 2, nil
}
func valueSize(value types.AttributeValue, depth int) (int, error) {
	if value == nil || reflect.ValueOf(value).IsNil() || depth > 32 {
		return 0, storage.ErrInvalid
	}
	switch v := value.(type) {
	case *types.AttributeValueMemberS:
		if !utf8.ValidString(v.Value) {
			return 0, storage.ErrInvalid
		}
		return len(v.Value), nil
	case *types.AttributeValueMemberB:
		return len(v.Value), nil
	case *types.AttributeValueMemberN:
		return numberSize(v.Value)
	case *types.AttributeValueMemberBOOL:
		return 1, nil
	case *types.AttributeValueMemberNULL:
		if !v.Value {
			return 0, storage.ErrInvalid
		}
		return 1, nil
	case *types.AttributeValueMemberM:
		return mapSize(v.Value, depth+1, true)
	case *types.AttributeValueMemberL:
		if depth >= 32 {
			return 0, storage.ErrInvalid
		}
		n := 3 + len(v.Value)
		for _, x := range v.Value {
			size, err := valueSize(x, depth+1)
			if err != nil {
				return 0, err
			}
			n += size
		}
		return n, nil
	case *types.AttributeValueMemberSS:
		if len(v.Value) == 0 {
			return 0, storage.ErrInvalid
		}
		n := 0
		for _, s := range v.Value {
			if !utf8.ValidString(s) {
				return 0, storage.ErrInvalid
			}
			n += len(s)
		}
		return n, nil
	case *types.AttributeValueMemberBS:
		if len(v.Value) == 0 {
			return 0, storage.ErrInvalid
		}
		n := 0
		for _, b := range v.Value {
			n += len(b)
		}
		return n, nil
	case *types.AttributeValueMemberNS:
		if len(v.Value) == 0 {
			return 0, storage.ErrInvalid
		}
		n := 0
		for _, s := range v.Value {
			size, err := numberSize(s)
			if err != nil {
				return 0, err
			}
			n += size
		}
		return n, nil
	default:
		return 0, storage.ErrInvalid
	}
}
func ValidateItem(item map[string]types.AttributeValue) error {
	n, err := ItemSizeUpperBound(item)
	if err != nil {
		return err
	}
	if len(item) == 0 || n > MaxItemBytes {
		return fmt.Errorf("%w: DynamoDB item size %d", storage.ErrInvalid, n)
	}
	return nil
}

// ValidateTransactionPlan requires one full item image per action (Put/Update:
// postimage; Delete/ConditionCheck: observed image). Builders must bind those
// observations with version/content conditions. This checks shape/size, not
// whether arbitrary expressions really establish those preconditions. It does
// not serialize or execute a request and does not measure HTTP wire overhead.
func ValidateTransactionPlan(in *ddb.TransactWriteItemsInput, images []map[string]types.AttributeValue, inventory map[string]Table) error {
	if in == nil || len(in.TransactItems) == 0 || len(in.TransactItems) > MaxTransactionActions || len(images) != len(in.TransactItems) || !attemptToken.MatchString(aws.ToString(in.ClientRequestToken)) {
		return storage.ErrInvalid
	}
	seen := map[string]bool{}
	total := 0
	for i, action := range in.TransactItems {
		var table string
		var key map[string]types.AttributeValue
		count := 0
		if p := action.Put; p != nil {
			count++
			table = aws.ToString(p.TableName)
			key = p.Item
			if !reflect.DeepEqual(p.Item, images[i]) {
				return storage.ErrInvalid
			}
		}
		if p := action.Update; p != nil {
			count++
			table = aws.ToString(p.TableName)
			key = p.Key
		}
		if p := action.Delete; p != nil {
			count++
			table = aws.ToString(p.TableName)
			key = p.Key
		}
		if p := action.ConditionCheck; p != nil {
			count++
			table = aws.ToString(p.TableName)
			key = p.Key
		}
		spec, ok := inventory[table]
		if count != 1 || !ok || spec.Partition == "" {
			return storage.ErrInvalid
		}
		keyFields := 1
		if spec.Sort != "" {
			keyFields++
		}
		if action.Put == nil && len(key) != keyFields {
			return storage.ErrInvalid
		}
		identity, err := itemIdentity(table, spec, key)
		if err != nil {
			return err
		}
		imageIdentity, err := itemIdentity(table, spec, images[i])
		if err != nil || identity != imageIdentity || seen[identity] {
			return fmt.Errorf("%w: repeated or mismatched transaction target", storage.ErrInvalid)
		}
		seen[identity] = true
		if err := ValidateItem(images[i]); err != nil {
			return err
		}
		n, _ := ItemSizeUpperBound(images[i])
		total += n
		if total > MaxTransactionBytes {
			return fmt.Errorf("%w: transaction item total", storage.ErrInvalid)
		}
	}
	return nil
}
func itemIdentity(table string, spec Table, item map[string]types.AttributeValue) (string, error) {
	pk, ok := item[spec.Partition].(*types.AttributeValueMemberS)
	if !ok || pk == nil || pk.Value == "" || len(pk.Value) > 2048 || !utf8.ValidString(pk.Value) {
		return "", storage.ErrInvalid
	}
	values := []string{table, pk.Value}
	if spec.Sort != "" {
		switch spec.SortType {
		case "S":
			v, ok := item[spec.Sort].(*types.AttributeValueMemberS)
			if !ok || v == nil || v.Value == "" || len(v.Value) > 1024 || !utf8.ValidString(v.Value) {
				return "", storage.ErrInvalid
			}
			values = append(values, v.Value)
		case "N":
			v, ok := item[spec.Sort].(*types.AttributeValueMemberN)
			if !ok || v == nil {
				return "", storage.ErrInvalid
			}
			if _, err := numberSize(v.Value); err != nil {
				return "", err
			}
			values = append(values, v.Value)
		default:
			return "", storage.ErrInvalid
		}
	}
	b, _ := json.Marshal(values)
	return string(b), nil
}
