package dynamodb

import (
	"context"
	"errors"
	"math"
	"reflect"
	"testing"

	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/KinasTomes/miles/internal/storage/contracttest"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func TestTypedNativeCodecPreservesIntegersAndRejectsMissingFields(t *testing.T) {
	want := s.LogicalQuota{OwnerID: "owner", Limit: math.MaxInt64, Used: 1<<53 + 7, Reserved: 3, Staged: 4, ActiveUploads: 2}
	item, err := nativeItem(want)
	if err != nil {
		t.Fatal(err)
	}
	var got s.LogicalQuota
	if err := unmarshalNative(&types.AttributeValueMemberM{Value: item}, reflect.ValueOf(&got).Elem()); err != nil || got != want {
		t.Fatal(got, err)
	}
	delete(item, "staged")
	if err := unmarshalNative(&types.AttributeValueMemberM{Value: item}, reflect.ValueOf(&got).Elem()); !errors.Is(err, s.ErrUnavailable) {
		t.Fatal("missing counter defaulted to zero", err)
	}
}

type partialBatch struct {
	API
	calls   int
	forever bool
}

func (p *partialBatch) BatchGetItem(ctx context.Context, in *ddb.BatchGetItemInput, options ...func(*ddb.Options)) (*ddb.BatchGetItemOutput, error) {
	p.calls++
	if p.forever || p.calls == 1 {
		return &ddb.BatchGetItemOutput{UnprocessedKeys: in.RequestItems}, nil
	}
	return p.API.BatchGetItem(ctx, in, options...)
}
func TestLocalBatchNeverTreatsUnprocessedAsMissing(t *testing.T) {
	f := contracttest.NewFixture()
	m, _ := fixtureStore(t, f)
	c := f.Chunk("default")
	if _, _, err := m.PutChunkIfAbsent(context.Background(), c, f.Lease.Fence); err != nil {
		t.Fatal(err)
	}
	wrapper := &partialBatch{API: m.api}
	m.api = wrapper
	chunks, err := m.GetChunks(context.Background(), []s.ChunkKey{c.Key, c.Key}, true)
	if err != nil || len(chunks) != 1 || wrapper.calls != 2 {
		t.Fatal(chunks, err, wrapper.calls)
	}
	wrapper.forever = true
	m.maxAttempts = 2
	if _, err := m.GetChunks(context.Background(), []s.ChunkKey{c.Key}, true); !errors.Is(err, s.ErrUnavailable) {
		t.Fatal("unprocessed key was reported absent", err)
	}
}
