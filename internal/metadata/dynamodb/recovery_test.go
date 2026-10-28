package dynamodb

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/KinasTomes/miles/internal/storage/contracttest"
)

func TestLocalOutboxPaginationAndImmutableExportProgress(t *testing.T) {
	f := contracttest.NewFixture()
	m, _ := fixtureStore(t, f)
	ctx := context.Background()
	ids := []string{}
	var event s.OutboxEvent
	for i := 0; i < 3; i++ {
		r := f.Request(fmt.Sprintf("export-%d", i), uint64(i))
		reserved := int64(0)
		if i == 0 {
			reserved = 4
		}
		contracttest.Prepare(t, m, f, r, reserved)
		if _, err := m.CommitObject(ctx, r); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, r.Event.ID)
		event = r.Event
	}
	sort.Strings(ids)
	q := s.OutboxQuery{HistoryID: event.HistoryID, Epoch: event.Epoch, Limit: 1}
	var got []string
	for {
		page, err := m.ListOutbox(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range page.Events {
			got = append(got, e.ID)
		}
		if page.Next == "" {
			break
		}
		if page.Next <= q.After {
			t.Fatal("cursor did not advance")
		}
		q.After = page.Next
	}
	if !reflect.DeepEqual(got, ids) {
		t.Fatal(got, ids)
	}
	q.After = ""
	q.Epoch++
	page, err := m.ListOutbox(ctx, q)
	if err != nil || len(page.Events) != 0 || page.Next == "" {
		t.Fatal("filtered page must retain base cursor", page, err)
	}
	progress := s.OutboxExport{Event: event, DescriptorKey: "r2/descriptor", DescriptorSHA256: strings.Repeat("c", 64)}
	for i := 0; i < 2; i++ {
		if err := m.MarkOutboxExported(ctx, progress); err != nil {
			t.Fatal(err)
		}
	}
	actual, err := m.GetOutboxExport(ctx, event.ID)
	if err != nil || !reflect.DeepEqual(actual, progress) {
		t.Fatal(actual, err)
	}
	unchanged, err := m.GetOutbox(ctx, event.ID)
	if err != nil || !reflect.DeepEqual(unchanged, event) {
		t.Fatal("export changed immutable event", unchanged, err)
	}
	progress.Event.PayloadPointer = "another-source"
	if err := m.MarkOutboxExported(ctx, progress); !errors.Is(err, s.ErrConflict) {
		t.Fatal("changed source accepted", err)
	}
}
