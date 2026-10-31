package dynamodb

import (
	"context"
	"encoding/base64"
	"encoding/json"
	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"reflect"
)

type observationCursor struct {
	Kind string
	Keys map[string]string
}

var _ s.ObservationStore = (*Store)(nil)

func (m *Store) ObservePage(ctx context.Context, r s.ObservationRequest) (s.ObservationPage, error) {
	var out s.ObservationPage
	if err := r.Validate(); err != nil {
		return out, err
	}
	table := map[string]string{"buckets": "Buckets", "heads": "Objects", "packs": "Packs"}[r.Kind]
	var cursor Item
	if r.Cursor != "" {
		body, err := base64.RawURLEncoding.Strict().DecodeString(r.Cursor)
		var c observationCursor
		if err != nil || json.Unmarshal(body, &c) != nil || c.Kind != r.Kind {
			return out, s.ErrInvalid
		}
		cursor = Item{}
		spec := Tables()[table]
		for _, name := range []string{spec.Partition, spec.Sort} {
			if name != "" {
				value, ok := c.Keys[name]
				if !ok {
					return out, s.ErrInvalid
				}
				cursor[name] = text(value)
			}
		}
		if len(cursor) != len(c.Keys) {
			return out, s.ErrInvalid
		}
	}
	res, err := m.api.Scan(ctx, &ddb.ScanInput{TableName: aws.String(m.tables[table]), ConsistentRead: aws.Bool(true), Limit: aws.Int32(int32(r.Limit)), ExclusiveStartKey: cursor})
	if err != nil {
		return out, backend(err)
	}
	if res == nil {
		return out, s.ErrUnavailable
	}
	for _, row := range res.Items {
		a := &types.AttributeValueMemberM{Value: row}
		switch r.Kind {
		case "buckets":
			var v s.Bucket
			if e := unmarshalNative(a, reflect.ValueOf(&v).Elem()); e != nil {
				return out, e
			}
			out.Buckets = append(out.Buckets, v)
		case "heads":
			v, e := decodeHead(row)
			if e != nil {
				return out, e
			}
			out.Heads = append(out.Heads, v)
		case "packs":
			var v s.PackInfo
			if e := unmarshalNative(a, reflect.ValueOf(&v).Elem()); e != nil {
				return out, e
			}
			out.Packs = append(out.Packs, v)
		}
	}
	if len(res.LastEvaluatedKey) > 0 {
		c := observationCursor{Kind: r.Kind, Keys: map[string]string{}}
		for name, a := range res.LastEvaluatedKey {
			v, ok := a.(*types.AttributeValueMemberS)
			if !ok || v == nil {
				return out, s.ErrUnavailable
			}
			c.Keys[name] = v.Value
		}
		body, e := json.Marshal(c)
		if e != nil {
			return out, e
		}
		out.Next = base64.RawURLEncoding.EncodeToString(body)
		if out.Next == r.Cursor {
			return out, s.ErrUnavailable
		}
	}
	return out, nil
}
