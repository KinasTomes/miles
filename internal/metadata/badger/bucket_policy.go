package badger

import (
	"context"
	"github.com/KinasTomes/miles/internal/metadata/operations"
	s "github.com/KinasTomes/miles/internal/storage"
	db "github.com/dgraph-io/badger/v4"
)

var _ s.BucketPolicyStore = (*Store)(nil)

func (m *Store) GetBucketPolicy(ctx context.Context, bucket, id string, revision uint64) (out s.PolicySnapshot, err error) {
	err = m.view(ctx, func(t *db.Txn) error {
		out, err = operations.ReadBucketPolicy(multipartTx{m: m, t: t, ctx: ctx}, bucket, id, revision)
		return err
	})
	return
}
func (m *Store) PutBucketPolicy(ctx context.Context, r s.BucketPolicyUpdate) (out s.PolicySnapshot, err error) {
	err = m.update(ctx, func(t *db.Txn) error {
		if err := m.fence(t, r.Writer); err != nil {
			return err
		}
		out, err = operations.WriteBucketPolicy(multipartTx{m: m, t: t, ctx: ctx}, ctx, r)
		if err == nil && m.beforePublish != nil {
			err = m.beforePublish()
		}
		return err
	})
	if err == nil && m.afterPublish != nil {
		err = m.afterPublish()
	}
	if err != nil {
		return s.PolicySnapshot{}, err
	}
	return out, nil
}
