package dynamodb

import (
	"context"
	"errors"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/smithy-go"
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

type splitBatch struct {
	API
	t       *testing.T
	calls   int
	pending map[string]types.KeysAndAttributes
}

func (p *splitBatch) BatchGetItem(ctx context.Context, in *ddb.BatchGetItemInput, options ...func(*ddb.Options)) (*ddb.BatchGetItemOutput, error) {
	p.calls++
	if p.calls == 1 {
		first := map[string]types.KeysAndAttributes{}
		p.pending = map[string]types.KeysAndAttributes{}
		for table, group := range in.RequestItems {
			if len(group.Keys) != 2 || !aws.ToBool(group.ConsistentRead) {
				p.t.Fatal("expected two distinct consistent keys", group)
			}
			firstGroup := group
			firstGroup.Keys = group.Keys[:1]
			group.Keys = group.Keys[1:]
			first[table], p.pending[table] = firstGroup, group
		}
		out, err := p.API.BatchGetItem(ctx, &ddb.BatchGetItemInput{RequestItems: first}, options...)
		if err != nil {
			return nil, err
		}
		if len(out.UnprocessedKeys) != 0 {
			p.t.Fatal("backend returned unexpected unprocessed keys before injection")
		}
		out.UnprocessedKeys = p.pending
		return out, nil
	}
	if !reflect.DeepEqual(p.pending, in.RequestItems) {
		p.t.Fatal("retry included a processed key or lost consistency")
	}
	if p.calls == 2 {
		return nil, &smithy.GenericAPIError{Code: "ThrottlingException", Message: "injected throttle"}
	}
	return p.API.BatchGetItem(ctx, in, options...)
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
	runBatchContract(t, f, m)
}

func runBatchContract(t *testing.T, f contracttest.Fixture, m *Store) {
	t.Helper()
	c := f.Chunk("default")
	if _, _, err := m.PutChunkIfAbsent(context.Background(), c, f.Lease.Fence); err != nil {
		t.Fatal(err)
	}
	base := m.api
	defer func() { m.api = base }()
	other := f.Chunk("isolated")
	if _, _, err := m.PutChunkIfAbsent(t.Context(), other, f.Lease.Fence); err != nil {
		t.Fatal(err)
	}
	split := &splitBatch{API: base, t: t}
	m.api = split
	got, err := m.GetChunks(t.Context(), []s.ChunkKey{c.Key, other.Key, c.Key}, true)
	if err != nil || got[c.Key] != c || got[other.Key] != other || split.calls != 3 {
		t.Fatal("injected partial then throttle did not finish both real reads", len(got), split.calls, err)
	}
	t.Log("verified partial response, pending-only retry, and injected throttle recovery")
	wrapper := &partialBatch{API: base}
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
