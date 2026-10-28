package dynamodb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"reflect"
	"testing"
	"time"

	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/KinasTomes/miles/internal/storage/contracttest"
	"github.com/aws/aws-sdk-go-v2/aws"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func localClient(t *testing.T) *ddb.Client {
	t.Helper()
	endpoint := os.Getenv("MILES_DDB_LOCAL_ENDPOINT")
	if endpoint == "" {
		if os.Getenv("MILES_DDB_LOCAL_REQUIRED") == "1" {
			t.Fatal("DDB Local endpoint required")
		}
		t.Skip("DDB Local not explicitly configured")
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Port() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		t.Fatal("explicit loopback endpoint required")
	}
	client := ddb.New(ddb.Options{Region: "ap-southeast-1", BaseEndpoint: aws.String(endpoint), Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		return aws.Credentials{AccessKeyID: "mileslocal", SecretAccessKey: "mileslocal"}, nil
	})})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for {
		if _, err := client.ListTables(ctx, &ddb.ListTablesInput{Limit: aws.Int32(1)}); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("DDB Local API readiness", ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
	return client
}

// This provider is test-only. It demonstrates that observed authority records
// participate in the metadata CAS; it is not a distributed R1 implementation.
type fixtureAuthority struct{ now func() time.Time }

func (a fixtureAuthority) Check(ctx context.Context, w s.WriterFence, observe Observe) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if w.LeaseID == "" || w.ServiceIncarnation == "" || w.Epoch == 0 {
		return s.ErrFenced
	}
	item, err := observe(writerKey(w.LeaseID))
	if err != nil {
		return s.ErrFenced
	}
	var lease s.WriterLease
	if unmarshalNative(&types.AttributeValueMemberM{Value: item}, reflect.ValueOf(&lease).Elem()) != nil {
		return s.ErrUnavailable
	}
	if lease.Fence != w || !a.now().Before(lease.ExpiresAt) {
		return s.ErrFenced
	}
	return nil
}
func (a fixtureAuthority) CheckManifest(ctx context.Context, w s.WriterFence, _ string, observe Observe) error {
	return a.Check(ctx, w, observe)
}

func writerKey(id string) Key {
	k := stringKey("Leases", "resource_id", "writer")
	k.Attributes["lease_id"] = text(id)
	return k
}

func fixtureStore(t *testing.T, f contracttest.Fixture) (*Store, *ddb.Client) {
	t.Helper()
	client := localClient(t)
	ctx := context.Background()
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	prefix := "miles_contract_" + hex.EncodeToString(random) + "_"
	inventory := map[string]string{}
	for name, spec := range Tables() {
		physical := prefix + name
		inventory[name] = physical
		definitions := []types.AttributeDefinition{{AttributeName: aws.String(spec.Partition), AttributeType: types.ScalarAttributeTypeS}}
		keys := []types.KeySchemaElement{{AttributeName: aws.String(spec.Partition), KeyType: types.KeyTypeHash}}
		if spec.Sort != "" {
			definitions = append(definitions, types.AttributeDefinition{AttributeName: aws.String(spec.Sort), AttributeType: types.ScalarAttributeType(spec.SortType)})
			keys = append(keys, types.KeySchemaElement{AttributeName: aws.String(spec.Sort), KeyType: types.KeyTypeRange})
		}
		if _, err := client.CreateTable(ctx, &ddb.CreateTableInput{TableName: aws.String(physical), AttributeDefinitions: definitions, KeySchema: keys, BillingMode: types.BillingModePayPerRequest}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if _, err := client.DeleteTable(ctx, &ddb.DeleteTableInput{TableName: aws.String(physical)}); err != nil {
				t.Error("scoped test table cleanup", err)
			}
		})
	}
	now := func() time.Time { return f.Now }
	store, err := New(Options{API: client, Tables: inventory, Now: now, Authority: fixtureAuthority{now}})
	if err != nil {
		t.Fatal(err)
	}
	err = store.update(ctx, func(tx *transaction) error {
		for _, user := range f.Users {
			if err := put(tx, key("users", user.ID), user); err != nil {
				return err
			}
		}
		for _, credential := range f.Credentials {
			if err := put(tx, key("credentials", credential.ID), credential); err != nil {
				return err
			}
		}
		if err := put(tx, key("buckets", f.Bucket.Name), f.Bucket); err != nil {
			return err
		}
		if err := put(tx, key("grants", f.Grant.BucketID, f.Grant.UserID), f.Grant); err != nil {
			return err
		}
		if err := put(tx, key("quotas", f.Quota.OwnerID), f.Quota); err != nil {
			return err
		}
		for _, pack := range f.Packs {
			if err := put(tx, packKey(pack.Ref), pack); err != nil {
				return err
			}
		}
		row, err := tx.observe(writerKey(f.Lease.Fence.LeaseID))
		if err != nil {
			return err
		}
		row.after, err = nativeItem(f.Lease)
		if err != nil {
			return err
		}
		for n, v := range row.key.Attributes {
			row.after[n] = v
		}
		row.dirty = true
		if f.Upload != nil {
			return put(tx, key("uploads", f.Upload.UploadID), *f.Upload)
		}
		return nil
	})
	if err != nil {
		t.Fatal("seed isolated fixture", err)
	}
	return store, client
}

func TestLocalFoundation(t *testing.T) {
	f := contracttest.NewFixture()
	m, _ := fixtureStore(t, f)
	ctx := context.Background()
	a, err := m.Authorize(ctx, f.Credentials[0].ID, f.Bucket.Name)
	if err != nil {
		t.Fatal(err)
	}
	if a.Bucket.ID != f.Bucket.ID {
		t.Fatal(a)
	}
	c := f.Chunk("default")
	winner, won, err := m.PutChunkIfAbsent(ctx, c, f.Lease.Fence)
	if err != nil || !won || !reflect.DeepEqual(winner, c) {
		t.Fatal(winner, won, err)
	}
	_, won, err = m.PutChunkIfAbsent(ctx, c, f.Lease.Fence)
	if err != nil || won {
		t.Fatal(won, err)
	}
	keys := make([]s.ChunkKey, 105)
	for i := range keys {
		chunk := c
		chunk.Key.Hash = fmt.Sprintf("%064x", i+1)
		keys[i] = chunk.Key
		if err := m.update(ctx, func(tx *transaction) error { return put(tx, chunkKey(chunk.Key), chunk) }); err != nil {
			t.Fatal(err)
		}
	}
	chunks, err := m.GetChunks(ctx, append(keys, keys[0]), true)
	if err != nil || len(chunks) != len(keys) {
		t.Fatal(len(chunks), err)
	}
	request := f.Request("foundation", 0)
	r := s.Reservation{Version: s.ModelVersion, OpID: request.Head.OpID, State: s.ReservationActive, Revision: 1, Deadline: f.Now.Add(time.Minute), ID: request.ReservationID, OwnerID: f.Bucket.OwnerID, Object: request.Head.Object, Bytes: 4, StagedBytes: 4, Writer: f.Lease.Fence}
	if err := m.ReserveLogicalQuota(ctx, r); err != nil {
		t.Fatal(err)
	}
	q, err := m.GetQuota(ctx, f.Quota.OwnerID)
	if err != nil || q.Reserved != f.Quota.Reserved+4 {
		t.Fatal(q, err)
	}
	if err := m.ReserveLogicalQuota(ctx, r); err != nil {
		t.Fatal(err)
	}
	q2, err := m.GetQuota(ctx, f.Quota.OwnerID)
	if err != nil || q2 != q {
		t.Fatal(q2, err)
	}
}
