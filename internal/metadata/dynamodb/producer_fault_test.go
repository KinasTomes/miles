package dynamodb

import (
	"context"
	"errors"
	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/KinasTomes/miles/internal/storage/contracttest"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"strings"
	"testing"
)

type producerACKFault struct {
	API
	before, after bool
}

func (f *producerACKFault) TransactWriteItems(ctx context.Context, in *ddb.TransactWriteItemsInput, opts ...func(*ddb.Options)) (*ddb.TransactWriteItemsOutput, error) {
	if f.before {
		return nil, s.ErrUnavailable
	}
	out, err := f.API.TransactWriteItems(ctx, in, opts...)
	if err == nil && f.after {
		f.after = false
		return nil, s.ErrUnavailable
	}
	return out, err
}

func TestLocalProducerAtomicFaultAndRetry(t *testing.T) {
	for _, after := range []bool{false, true} {
		name := "before-send"
		if after {
			name = "lost-ack"
		}
		t.Run(name, func(t *testing.T) {
			f := contracttest.NewFixture()
			m, _ := fixtureStore(t, f)
			ctx := context.Background()
			op := strings.Repeat("a", 32)
			r := s.ControlMutation{CredentialID: "admin-key", Entity: "Users", ID: "fault-user", User: s.User{ID: "fault-user", Active: true, DefaultDomain: "default"}, SetQuota: true, QuotaLimit: 10, Context: s.MutationContext{HistoryID: f.Request("producer", 0).Event.HistoryID, LogicalOpID: op, MutationID: op, Writer: f.Lease.Fence}}
			fault := &producerACKFault{API: m.api, before: !after, after: after}
			m.api = fault
			if _, err := m.MutateControl(ctx, r); !errors.Is(err, s.ErrUnavailable) {
				t.Fatal(err)
			}
			original, err := m.GetMutation(ctx, op)
			if after && err != nil || !after && !errors.Is(err, s.ErrNotFound) {
				t.Fatal("partial producer outcome", original, err)
			}
			user, userErr := read[s.User](m, ctx, key("users", r.ID))
			if after && (userErr != nil || user.Revision != 1) || !after && !errors.Is(userErr, s.ErrNotFound) {
				t.Fatal("partial business write", user, userErr)
			}
			fault.before = false
			result, err := m.MutateControl(ctx, r)
			if err != nil {
				t.Fatal(err)
			}
			if after && result.Event.ID != original.Event.ID {
				t.Fatal("retry replaced original event")
			}
			if _, err = read[s.OutboxEvent](m, ctx, key("events", result.Event.ID)); err != nil {
				t.Fatal("business commit without outbox", err)
			}
			user, err = read[s.User](m, ctx, key("users", r.ID))
			if err != nil || user.Revision != 1 {
				t.Fatal("retry incremented revision", user, err)
			}
		})
	}
}
