package dynamodb

import (
	"context"
	"reflect"
	"time"

	"github.com/KinasTomes/miles/internal/manifest"
	"github.com/KinasTomes/miles/internal/metadata/operations"
	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// MultipartAuthority binds logical upload/candidate roots to the shared
// protection protocol. A writer timestamp alone is not sufficient.
type MultipartAuthority interface {
	CheckMultipart(context.Context, s.WriterFence, string, Observe) error
}
type multipartBridge struct{ m *Store }
type multipartTx struct{ *transaction }

func (m *Store) multipartOps() operations.Multipart {
	return operations.Multipart{Backend: multipartBridge{m}}
}
func (b multipartBridge) Update(ctx context.Context, fn func(operations.Tx) error) error {
	return b.m.update(ctx, func(t *transaction) error { return fn(multipartTx{t}) })
}
func (b multipartBridge) View(ctx context.Context, fn func(operations.Tx) error) error {
	return b.m.view(ctx, func(t *transaction) error { return fn(multipartTx{t}) })
}
func (b multipartBridge) Now() time.Time            { return b.m.now() }
func (b multipartBridge) Metadata() s.MetadataStore { return b.m }
func (b multipartBridge) Part(ctx context.Context, id string, n uint32) (s.ManifestPage, error) {
	return b.m.GetPartPage(ctx, id, n)
}
func (b multipartBridge) CompletionPage(ctx context.Context, k byte, id string, n uint32) ([]byte, error) {
	return b.m.ReadCompletionPage(ctx, k, id, n)
}
func logicalRecord(kind string, ids []string) string {
	if kind == "heads" {
		return headKey(s.ObjectKey{Bucket: ids[0], BucketID: ids[1], Key: ids[2]})
	}
	return key(kind, ids...)
}
func (t multipartTx) Get(kind string, ids []string, out any) error {
	k := logicalRecord(kind, ids)
	a, err := resolve(k)
	if err != nil {
		return err
	}
	row, err := t.observe(a.key)
	if err != nil {
		return err
	}
	if row.after == nil {
		return s.ErrNotFound
	}
	if a.incarnation != "" {
		actual, ok := row.after["bucket_id"].(*types.AttributeValueMemberS)
		if !ok || actual == nil {
			return s.ErrUnavailable
		}
		if actual.Value != a.incarnation {
			return s.ErrNotFound
		}
	}
	var attr types.AttributeValue = &types.AttributeValueMemberM{Value: row.after}
	if a.scope != "" && a.scope != "users" && a.scope != "quotas" {
		attr = row.after[a.scope]
		if attr == nil {
			return s.ErrNotFound
		}
	}
	return unmarshalNative(attr, reflect.ValueOf(out).Elem())
}
func (t multipartTx) Put(kind string, ids []string, value any) error {
	return put(t.transaction, logicalRecord(kind, ids), value)
}
func (t multipartTx) Authorize(credential, bucket string) (s.Authorization, error) {
	return t.store.authorize(t.transaction, credential, bucket)
}
func (t multipartTx) Fence(w s.WriterFence, id string) error {
	if err := t.store.fence(t.transaction, w); err != nil {
		return err
	}
	a, ok := t.store.authority.(MultipartAuthority)
	if !ok {
		return s.ErrFenced
	}
	return a.CheckMultipart(t.ctx, w, id, func(k Key) (Item, error) {
		row, err := t.observe(k)
		if err != nil {
			return nil, err
		}
		if row.after == nil {
			return nil, s.ErrNotFound
		}
		return copyItem(row.after), nil
	})
}
func (t multipartTx) CloseOperation(op, reservation string) error {
	r, err := get[s.Reservation](t.transaction, key("reservations", reservation))
	if err != nil {
		return err
	}
	if r.OpID != op {
		return s.ErrConflict
	}
	row, err := t.load(key("outcomes", op))
	if err != nil {
		return err
	}
	if row.after != nil && row.after["result"] != nil {
		return s.ErrConflict
	}
	if operationClosed(row.after) {
		return nil
	}
	return closeOperation(row, r)
}
func (t multipartTx) ResetOperation(op, reservation string) error {
	row, err := t.load(key("outcomes", op))
	if err != nil {
		return err
	}
	if row.after == nil {
		return nil
	}
	if binding, ok := row.after["reservation_id"].(*types.AttributeValueMemberS); ok && binding.Value != reservation {
		return s.ErrConflict
	}
	if row.after["result"] != nil || row.after["event_id"] != nil {
		return s.ErrConflict
	}
	if row.after["pending_request"] != nil {
		return s.ErrUnavailable
	}
	if operationClosed(row.after) {
		row.after = copyItem(row.after)
		delete(row.after, "input_sha256")
		delete(row.after, "closed_attempt")
		row.after["state"] = text("Prepared")
		row.dirty = true
	}
	return nil
}
func (b multipartBridge) Parts(ctx context.Context, id string, after uint32, limit int) (s.PartListPage, error) {
	var result s.PartListPage
	if limit < 1 || limit > 1000 {
		return result, s.ErrInvalid
	}
	rows, err := b.query(ctx, "UploadParts", "upload_id", id, "part_number", number(uint64(after)), limit+1)
	if err != nil {
		return result, err
	}
	for i, row := range rows {
		if i == limit {
			result.Next = result.Parts[i-1].Number
			break
		}
		var p s.UploadPart
		if err := unmarshalNative(&types.AttributeValueMemberM{Value: row}, reflect.ValueOf(&p).Elem()); err != nil {
			return result, err
		}
		result.Parts = append(result.Parts, p)
	}
	return result, nil
}
func (b multipartBridge) Publishers(ctx context.Context, id, after string, limit int) (s.PublisherPage, error) {
	var result s.PublisherPage
	if limit < 1 || limit > 1000 {
		return result, s.ErrInvalid
	}
	rows, err := b.query(ctx, "Leases", "resource_id", "upload/"+id, "lease_id", text(after), limit+1)
	if err != nil {
		return result, err
	}
	for i, row := range rows {
		if i == limit {
			result.Next = result.Publishers[i-1].ID
			break
		}
		var p s.PartPublisher
		if err := unmarshalNative(&types.AttributeValueMemberM{Value: row}, reflect.ValueOf(&p).Elem()); err != nil {
			return result, err
		}
		result.Publishers = append(result.Publishers, p)
	}
	return result, nil
}
func (b multipartBridge) query(ctx context.Context, table, pk, id, sk string, after types.AttributeValue, limit int) ([]Item, error) {
	var rows []Item
	var cursor Item
	for len(rows) < limit {
		in := &ddb.QueryInput{TableName: aws.String(b.m.tables[table]), ConsistentRead: aws.Bool(true), Limit: aws.Int32(int32(limit - len(rows))), ExclusiveStartKey: cursor, KeyConditionExpression: aws.String("#pk = :pk AND #sk > :after"), ExpressionAttributeNames: map[string]string{"#pk": pk, "#sk": sk}, ExpressionAttributeValues: Item{":pk": text(id), ":after": after}}
		// DynamoDB forbids empty String key values, including expression operands.
		if v, ok := after.(*types.AttributeValueMemberS); ok && v.Value == "" {
			in.KeyConditionExpression = aws.String("#pk = :pk")
			delete(in.ExpressionAttributeNames, "#sk")
			delete(in.ExpressionAttributeValues, ":after")
		}
		out, err := b.m.api.Query(ctx, in)
		if err != nil {
			return nil, backend(err)
		}
		if out == nil {
			return nil, s.ErrUnavailable
		}
		rows = append(rows, out.Items...)
		cursor = out.LastEvaluatedKey
		if len(cursor) == 0 {
			break
		}
	}
	return rows, nil
}
func (m *Store) WriteCompletionPage(ctx context.Context, kind byte, id string, n uint32, body []byte, w s.WriterFence) error {
	if _, err := manifest.DecodeCompletionPage(body, kind, id, n); err != nil {
		return err
	}
	return m.update(ctx, func(t *transaction) error {
		if err := (multipartTx{t}).Fence(w, id); err != nil {
			return err
		}
		k, err := PageKey(kind, id, n)
		if err != nil {
			return err
		}
		row, err := t.observe(k)
		if err != nil {
			return err
		}
		if row.after != nil {
			old, err := CloneBinary(row.after["body"])
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(old, body) {
				return s.ErrConflict
			}
			return nil
		}
		row.after = copyItem(k.Attributes)
		row.after["body"] = &types.AttributeValueMemberB{Value: append([]byte(nil), body...)}
		row.dirty = true
		return nil
	})
}
func (m *Store) ReadCompletionPage(ctx context.Context, kind byte, id string, n uint32) ([]byte, error) {
	k, err := PageKey(kind, id, n)
	if err != nil {
		return nil, err
	}
	row, err := m.transaction(ctx).observe(k)
	if err != nil {
		return nil, err
	}
	if row.after == nil {
		return nil, s.ErrNotFound
	}
	body, err := CloneBinary(row.after["body"])
	if err != nil {
		return nil, err
	}
	_, err = manifest.DecodeCompletionPage(body, kind, id, n)
	return body, err
}
