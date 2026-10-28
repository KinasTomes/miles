package dynamodb

import (
	"context"
	"testing"

	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/KinasTomes/miles/internal/storage/contracttest"
	"github.com/aws/aws-sdk-go-v2/aws"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

func TestLocalMetadataContract(t *testing.T) {
	contracttest.RunMetadata(t, func(t *testing.T, f contracttest.Fixture) contracttest.MetadataHarness {
		m, client := fixtureStore(t, f)
		set := func(k string, v any) {
			t.Helper()
			if err := m.update(context.Background(), func(tx *transaction) error { return put(tx, k, v) }); err != nil {
				t.Fatal(err)
			}
		}
		fault := func(on bool) func() error {
			if !on {
				return nil
			}
			return func() error { return s.ErrUnavailable }
		}
		return contracttest.MetadataHarness{Store: m,
			SetBucket: func(v s.Bucket) { set(key("buckets", v.Name), v) },
			RemoveBucket: func(name string) {
				k, _ := resolve(key("buckets", name))
				if _, err := client.DeleteItem(context.Background(), &ddb.DeleteItemInput{TableName: aws.String(m.tables[k.key.Table]), Key: k.key.Attributes}); err != nil {
					t.Fatal(err)
				}
			},
			SetUser:       func(v s.User) { set(key("users", v.ID), v) },
			SetCredential: func(v s.Credential) { set(key("credentials", v.ID), v) },
			SetGrant:      func(v s.Grant) { set(key("grants", v.BucketID, v.UserID), v) },
			SetQuota:      func(v s.LogicalQuota) { set(key("quotas", v.OwnerID), v) },
			SetPack:       func(v s.PackInfo) { set(packKey(v.Ref), v) },
			SetWriter: func(v s.WriterLease) {
				if err := m.update(context.Background(), func(tx *transaction) error {
					row, err := tx.observe(writerKey(v.Fence.LeaseID))
					if err != nil {
						return err
					}
					row.after, err = nativeItem(v)
					if err != nil {
						return err
					}
					for n, v := range row.key.Attributes {
						row.after[n] = v
					}
					row.dirty = true
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			},
			FailCommit:    func(before, after bool) { m.beforePublish = fault(before); m.afterPublish = fault(after) },
			FailReconcile: func(on bool) { m.reconcile = fault(on) },
		}
	})
}
