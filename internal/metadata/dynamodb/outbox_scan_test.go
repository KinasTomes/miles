package dynamodb

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/KinasTomes/miles/internal/storage/contracttest"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/smithy-go"
)

// Every discovery loses a later physical page once. Restarting from the first
// page cannot finish; retrying that exact physical page can.
type outboxScanFixture struct {
	API
	mu                 sync.Mutex
	rows               [][]Item
	starts, retries    int
	failed             bool
	permanent, looping bool
	code               string
	cancel             context.CancelFunc
}

func (a *outboxScanFixture) Scan(ctx context.Context, in *ddb.ScanInput, _ ...func(*ddb.Options)) (*ddb.ScanOutput, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	index := 0
	if len(in.ExclusiveStartKey) == 0 {
		a.starts++
		a.failed = false
	} else {
		for i := 1; i < len(a.rows); i++ {
			if reflect.DeepEqual(in.ExclusiveStartKey, Item{"event_id": text(fmt.Sprint(i))}) {
				index = i
				break
			}
		}
	}
	if index == 1 && (!a.failed || a.permanent) {
		a.failed = true
		a.retries++
		if a.cancel != nil {
			a.cancel()
		}
		return nil, &smithy.GenericAPIError{Code: a.code}
	}
	next := Item(nil)
	if index+1 < len(a.rows) {
		next = Item{"event_id": text(fmt.Sprint(index + 1))}
	}
	if a.looping && index > 0 {
		next = in.ExclusiveStartKey
	}
	return &ddb.ScanOutput{Items: a.rows[index], LastEvaluatedKey: next}, nil
}

func newOutboxScanFixture(t *testing.T) (*Store, *outboxScanFixture, s.OutboxQuery) {
	t.Helper()
	f := contracttest.NewFixture()
	api := &outboxScanFixture{rows: make([][]Item, 3), code: "ThrottlingException"}
	for i := range 7 {
		e := f.Request(fmt.Sprintf("scan-%d", i), 0).Event
		if i == 6 {
			e.Epoch++ // filtered epoch must still advance the logical cursor
		}
		row, err := nativeItem(e)
		if err != nil {
			t.Fatal(err)
		}
		row["event_id"] = text(e.ID)
		api.rows[i%3] = append(api.rows[i%3], row)
	}
	m := &Store{api: api, maxAttempts: 3, tables: map[string]string{"Outbox": "fixture-Outbox"}}
	return m, api, s.OutboxQuery{HistoryID: f.Request("scan-0", 0).Event.HistoryID, Limit: 2}
}

func TestOutboxLaterPageThrottlePreservesDiscovery(t *testing.T) {
	m, api, query := newOutboxScanFixture(t)
	seen, last, pages := 0, "", 0
	for {
		page, err := m.ListOutbox(t.Context(), query)
		if err != nil {
			t.Fatal("later-page throttle discarded successful discovery pages", err)
		}
		pages++
		for _, event := range page.Events {
			if event.ID <= last || event.Epoch != query.Epoch {
				t.Fatal("retry duplicated or reordered an event", event)
			}
			last = event.ID
			seen++
		}
		if page.Next == "" {
			break
		}
		if page.Next <= query.After || pages > 4 {
			t.Fatal("logical cursor did not advance", page)
		}
		query.After = page.Next
	}
	if seen != 6 || api.starts != pages || api.retries != pages {
		t.Fatal("discovery restarted from the beginning or skipped work", seen, pages, api.starts, api.retries)
	}
}

func TestOutboxScanFailureNeverReturnsPartialSuccess(t *testing.T) {
	for _, mode := range []string{"exhausted", "validation", "cancelled", "deadline", "loop"} {
		t.Run(mode, func(t *testing.T) {
			m, api, query := newOutboxScanFixture(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			want := s.ErrUnavailable
			switch mode {
			case "exhausted":
				api.permanent = true
			case "validation":
				api.code = "ValidationException"
			case "cancelled":
				api.cancel = cancel
				want = context.Canceled
			case "deadline":
				var stop context.CancelFunc
				ctx, stop = context.WithTimeout(ctx, 10*time.Millisecond)
				defer stop()
				api.permanent = true
				want = context.DeadlineExceeded
			case "loop":
				api.looping = true
				api.failed = true
				api.rows = api.rows[:2]
			}
			page, err := m.ListOutbox(ctx, query)
			if !errors.Is(err, want) || len(page.Events) != 0 || page.Next != "" {
				t.Fatal("failed or incomplete scan became success", page, err)
			}
			if mode == "exhausted" && api.retries != m.maxAttempts {
				t.Fatal("physical page retry escaped its attempt limit", api.retries)
			}
		})
	}
}
