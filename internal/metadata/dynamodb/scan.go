package dynamodb

import (
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"

	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// Reads are paginated and context-bound. Base scans are discovery only; they
// never establish epoch completeness or replace transaction predicates.
func scan(t *transaction, prefix string, fn func(string) error) error {
	parts := strings.Split(strings.TrimSuffix(prefix, "/"), "/")
	var table string
	var query *ddb.QueryInput
	decode := func(i int) (string, error) {
		if i >= len(parts) {
			return "", s.ErrInvalid
		}
		b, e := hex.DecodeString(parts[i])
		return string(b), e
	}
	switch parts[0] {
	case "buckets":
		table = "Buckets"
	case "packs":
		table = "Packs"
	case "pages":
		table = "ManifestPages"
		owner, e := decode(1)
		if e != nil {
			return e
		}
		query = &ddb.QueryInput{KeyConditionExpression: aws.String("#owner = :owner"), ExpressionAttributeNames: map[string]string{"#owner": "manifest_id"}, ExpressionAttributeValues: Item{":owner": text(owner)}}
	case "heads":
		table = "Objects"
		bucket, e := decode(1)
		if e != nil {
			return e
		}
		query = &ddb.QueryInput{KeyConditionExpression: aws.String("#bucket = :bucket"), ExpressionAttributeNames: map[string]string{"#bucket": "bucket"}, ExpressionAttributeValues: Item{":bucket": text(bucket)}}
	default:
		return s.ErrInvalid
	}
	keys := []string{}
	var cursor Item
	for {
		if err := t.ctx.Err(); err != nil {
			return err
		}
		var items []Item
		if query != nil {
			query.TableName = aws.String(t.store.tables[table])
			query.ConsistentRead = aws.Bool(true)
			query.Limit = aws.Int32(128)
			query.ExclusiveStartKey = cursor
			out, err := t.store.api.Query(t.ctx, query)
			if err != nil {
				return backend(err)
			}
			if out == nil {
				return s.ErrUnavailable
			}
			items = out.Items
			cursor = out.LastEvaluatedKey
		} else {
			out, err := t.store.api.Scan(t.ctx, &ddb.ScanInput{TableName: aws.String(t.store.tables[table]), ConsistentRead: aws.Bool(true), Limit: aws.Int32(128), ExclusiveStartKey: cursor})
			if err != nil {
				return backend(err)
			}
			if out == nil {
				return s.ErrUnavailable
			}
			items = out.Items
			cursor = out.LastEvaluatedKey
		}
		for _, item := range items {
			var logical string
			k := Key{Table: table, Attributes: Item{}}
			spec := Tables()[table]
			k.Attributes[spec.Partition] = item[spec.Partition]
			if spec.Sort != "" {
				k.Attributes[spec.Sort] = item[spec.Sort]
			}
			str := func(n string) string {
				v, ok := item[n].(*types.AttributeValueMemberS)
				if !ok || v == nil {
					return ""
				}
				return v.Value
			}
			switch table {
			case "Buckets":
				logical = key("buckets", str("bucket"))
			case "Packs":
				logical = packKey(s.PackRef{ID: str("pack_id"), Generation: str("generation")})
			case "ManifestPages":
				n, ok := item["page_no"].(*types.AttributeValueMemberN)
				if !ok {
					return s.ErrUnavailable
				}
				number, e := strconv.ParseUint(n.Value, 10, 32)
				if e != nil {
					return s.ErrUnavailable
				}
				logical = pageKey(str("manifest_id"), uint32(number))
			case "Objects":
				inc, e := decode(2)
				if e != nil {
					return e
				}
				if str("bucket_id") != inc {
					continue
				}
				logical = headKey(s.ObjectKey{Bucket: str("bucket"), BucketID: inc, Key: str("key")})
			}
			if _, err := itemIdentity(t.store.tables[table], spec, k.Attributes); err != nil {
				return fmt.Errorf("%w: scan key", s.ErrUnavailable)
			}
			id := identity(k)
			if t.rows[id] == nil {
				t.rows[id] = &observedRow{key: k, before: item, after: copyItem(item)}
			}
			keys = append(keys, logical)
		}
		if len(cursor) == 0 {
			break
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		if err := fn(k); err != nil {
			return err
		}
	}
	return nil
}

func (t *transaction) batch(logical []string, consistent bool) error {
	unique := map[string]Key{}
	for _, key := range logical {
		a, err := resolve(key)
		if err != nil {
			return err
		}
		unique[identity(a.key)] = a.key
	}
	keys := make([]Key, 0, len(unique))
	for _, key := range unique {
		keys = append(keys, key)
	}
	for start := 0; start < len(keys); start += 100 {
		end := start + 100
		if end > len(keys) {
			end = len(keys)
		}
		pending := map[string]types.KeysAndAttributes{}
		for _, key := range keys[start:end] {
			name := t.store.tables[key.Table]
			v := pending[name]
			v.Keys = append(v.Keys, key.Attributes)
			v.ConsistentRead = aws.Bool(consistent)
			pending[name] = v
		}
		for attempt := 0; len(pending) > 0; attempt++ {
			if err := t.ctx.Err(); err != nil {
				return err
			}
			if attempt >= t.store.maxAttempts {
				return s.ErrUnavailable
			}
			out, err := t.store.api.BatchGetItem(t.ctx, &ddb.BatchGetItemInput{RequestItems: pending})
			if err != nil {
				if retryableRead(err) && attempt+1 < t.store.maxAttempts {
					if err := retryPause(t.ctx, attempt); err != nil {
						return err
					}
					continue
				}
				return backend(err)
			}
			if out == nil {
				return s.ErrUnavailable
			}
			unprocessed := map[string]bool{}
			for table, group := range out.UnprocessedKeys {
				for _, key := range group.Keys {
					unprocessed[physicalIdentity(table, key)] = true
				}
			}
			for _, key := range keys[start:end] {
				table := t.store.tables[key.Table]
				if unprocessed[physicalIdentity(table, key.Attributes)] {
					continue
				}
				id := identity(key)
				if t.rows[id] != nil {
					continue
				}
				var found Item
				for _, item := range out.Responses[table] {
					candidate := Item{}
					for n := range key.Attributes {
						candidate[n] = item[n]
					}
					if physicalIdentity(table, candidate) == physicalIdentity(table, key.Attributes) {
						found = item
						break
					}
				}
				t.rows[id] = &observedRow{key: key, before: found, after: copyItem(found)}
			}
			pending = out.UnprocessedKeys
			if len(pending) > 0 {
				if err := retryPause(t.ctx, attempt); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
func physicalIdentity(table string, k Item) string { return identity(Key{Table: table, Attributes: k}) }
