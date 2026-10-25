package contracttest_test

import (
	"testing"
	"time"

	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/KinasTomes/miles/internal/storage/contracttest"
	"github.com/KinasTomes/miles/internal/storage/fake"
)

func TestMetadataContract(t *testing.T) {
	contracttest.RunMetadata(t, func(t *testing.T, f contracttest.Fixture) contracttest.MetadataHarness {
		seed := fake.Seed{Now: func() time.Time { return f.Now }, Buckets: []s.Bucket{f.Bucket}, Users: f.Users, Credentials: f.Credentials, Grants: []s.Grant{f.Grant}, Quotas: []s.LogicalQuota{f.Quota}, Packs: f.Packs, Writers: []s.WriterLease{f.Lease}}
		if f.Upload != nil {
			seed.Uploads = []s.Completion{*f.Upload}
		}
		store := fake.NewMetadata(seed)
		return contracttest.MetadataHarness{Store: store, SetPack: store.SetPack, SetBucket: store.SetBucket, DeleteBucket: store.DeleteBucket, SetCredential: store.SetCredential, SetUser: store.SetUser, SetGrant: store.SetGrant, SetWriter: store.SetWriter, SetQuota: store.SetQuota, FailCommit: store.FailCommit, FailReconcile: store.FailReconcile}
	})
}
func TestPackContract(t *testing.T) {
	contracttest.RunPacks(t, func(t *testing.T, now func() time.Time) contracttest.PackHarness {
		store := fake.NewPacks(now)
		return contracttest.PackHarness{Store: store, SidecarInfo: store.SidecarInfo, CompleteRestore: store.CompleteRestore}
	})
}
func TestCacheContract(t *testing.T) {
	contracttest.RunCache(t, func(t *testing.T) s.Cache { return fake.NewCache() })
}
