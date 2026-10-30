package dynamodb

import (
	"context"
	"github.com/KinasTomes/miles/internal/metadata/operations"
	s "github.com/KinasTomes/miles/internal/storage"
)

var _ s.PackProtectionStore = (*Store)(nil)

func (m *Store) ExtendPackProtection(ctx context.Context, r s.PackProtectionUpdate) (out s.PackInfo, err error) {
	err = m.update(ctx, func(tx *transaction) error {
		if err := m.fence(tx, r.Writer); err != nil {
			return err
		}
		t := multipartTx{tx}
		if err := m.producerGate(t, ctx, r.Writer); err != nil {
			return err
		}
		out, err = operations.ExtendProtection(t, ctx, r, m.now(), m.requireProducers)
		if err == nil && m.beforePublish != nil {
			err = m.beforePublish()
		}
		return err
	})
	if err == nil && m.afterPublish != nil {
		err = m.afterPublish()
	}
	if err != nil {
		return s.PackInfo{}, err
	}
	return out, nil
}
