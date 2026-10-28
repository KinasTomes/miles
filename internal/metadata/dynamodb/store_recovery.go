package dynamodb

import (
	"context"
	"errors"
	"reflect"
	"sort"

	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

var _ s.OutboxExportStore = (*Store)(nil)

// The frozen inventory has no event ordering index. A base scan retains only
// Limit+1 smallest IDs after the cursor. Memory is bounded; read cost is linear
// in table size. Neither consistent reads nor this discovery scan seal an epoch.
func (m *Store) ListOutbox(ctx context.Context, q s.OutboxQuery) (s.OutboxPage, error) {
	var out s.OutboxPage
	if err := q.Validate(); err != nil {
		return out, err
	}
	candidates := map[string]Item{}
	var cursor Item
	for {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		page, err := m.api.Scan(ctx, &ddb.ScanInput{TableName: aws.String(m.tables["Outbox"]), ConsistentRead: aws.Bool(true), Limit: aws.Int32(128), ExclusiveStartKey: cursor})
		if err != nil {
			return out, backend(err)
		}
		if page == nil {
			return out, s.ErrUnavailable
		}
		for _, item := range page.Items {
			id, ok := item["event_id"].(*types.AttributeValueMemberS)
			if !ok || id == nil {
				return out, s.ErrUnavailable
			}
			if id.Value <= q.After {
				continue
			}
			candidates[id.Value] = item
			if len(candidates) > q.Limit+1 {
				largest := ""
				for k := range candidates {
					if k > largest {
						largest = k
					}
				}
				delete(candidates, largest)
			}
		}
		cursor = page.LastEvaluatedKey
		if len(cursor) == 0 {
			break
		}
	}
	ids := make([]string, 0, len(candidates))
	for id := range candidates {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for i, id := range ids {
		if i == q.Limit {
			out.Next = ids[i-1]
			break
		}
		var event s.OutboxEvent
		if err := unmarshalNative(&types.AttributeValueMemberM{Value: candidates[id]}, reflect.ValueOf(&event).Elem()); err != nil {
			return out, err
		}
		if event.ID != id || event.Validate() != nil {
			return out, s.ErrUnavailable
		}
		if event.HistoryID == q.HistoryID && event.Epoch == q.Epoch {
			out.Events = append(out.Events, event)
		}
	}
	return out, nil
}
func (m *Store) MarkOutboxExported(ctx context.Context, p s.OutboxExport) error {
	if err := p.Validate(); err != nil {
		return err
	}
	return m.update(ctx, func(t *transaction) error {
		e, err := get[s.OutboxEvent](t, key("events", p.Event.ID))
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(e, p.Event) {
			return s.ErrConflict
		}
		old, err := get[s.OutboxExport](t, key("exports", e.ID))
		if err == nil {
			if reflect.DeepEqual(old, p) {
				return nil
			}
			return s.ErrConflict
		}
		if !errors.Is(err, s.ErrNotFound) {
			return err
		}
		return put(t, key("exports", e.ID), p)
	})
}
func (m *Store) GetOutboxExport(ctx context.Context, id string) (s.OutboxExport, error) {
	return read[s.OutboxExport](m, ctx, key("exports", id))
}
