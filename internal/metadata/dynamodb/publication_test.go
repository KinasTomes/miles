package dynamodb

import (
	"context"
	"fmt"
	"testing"
	"time"

	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/KinasTomes/miles/internal/storage/contracttest"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

type transactionCapture struct {
	API
	inputs []*ddb.TransactWriteItemsInput
}

func (c *transactionCapture) TransactWriteItems(ctx context.Context, in *ddb.TransactWriteItemsInput, options ...func(*ddb.Options)) (*ddb.TransactWriteItemsOutput, error) {
	c.inputs = append(c.inputs, in)
	return c.API.TransactWriteItems(ctx, in, options...)
}

func TestLocalLargeManifestUsesBoundedAtomicPublication(t *testing.T) {
	f := contracttest.NewFixture()
	f.Quota.Limit = 10000
	m, _ := fixtureStore(t, f)
	ctx := context.Background()
	r := f.Request("many-unique", 0)
	r.Head.Size = 105 * 4
	page := s.ManifestPage{Version: s.ModelVersion, ManifestID: r.Head.ManifestID, Domain: r.Head.Domain}
	for start := 0; start < 105; start += 35 {
		end := min(start+35, 105)
		if err := m.update(ctx, func(tx *transaction) error {
			for i := start; i < end; i++ {
				c := f.Chunk("default")
				c.Key.Hash = fmt.Sprintf("%064x", i+1)
				if err := put(tx, chunkKey(c.Key), c); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 105; i++ {
		c := f.Chunk("default")
		c.Key.Hash = fmt.Sprintf("%064x", i+1)
		page.Chunks = append(page.Chunks, s.ChunkRef{Key: c.Key, RawBytes: 4, LogicalOffset: int64(i * 4)})
	}
	if err := m.WriteManifestPages(ctx, []s.ManifestPage{page}, r.Writer); err != nil {
		t.Fatal(err)
	}
	reservation := s.Reservation{Version: s.ModelVersion, OpID: r.Head.OpID, State: s.ReservationActive, Revision: 1, Deadline: f.Now.Add(time.Minute), ID: r.ReservationID, OwnerID: f.Quota.OwnerID, Object: r.Head.Object, Bytes: r.Head.Size, StagedBytes: r.Head.Size, Writer: r.Writer}
	if err := m.ReserveLogicalQuota(ctx, reservation); err != nil {
		t.Fatal(err)
	}
	capture := &transactionCapture{API: m.api}
	m.api = capture
	result, err := m.CommitObject(ctx, r)
	if err != nil || result.Head.ChunkCount != 105 {
		t.Fatal(result, err)
	}
	last := capture.inputs[len(capture.inputs)-1]
	if len(last.TransactItems) > 20 {
		t.Fatal("manifest chunks leaked into final transaction", len(last.TransactItems))
	}
	owner, err := read[manifestOwner](m, ctx, key("manifest-owner", r.Head.ManifestID))
	if err != nil || owner.State != "Published" {
		t.Fatal(owner, err)
	}
	for _, item := range last.TransactItems {
		if item.ConditionCheck != nil && *item.ConditionCheck.TableName == m.tables["Chunks"] {
			t.Fatal("unbounded chunk condition in publication")
		}
	}
}
