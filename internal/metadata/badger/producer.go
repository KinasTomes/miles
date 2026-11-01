package badger

import (
	"bytes"
	"context"
	"github.com/KinasTomes/miles/internal/manifest"
	"github.com/KinasTomes/miles/internal/metadata/operations"
	s "github.com/KinasTomes/miles/internal/storage"
	db "github.com/dgraph-io/badger/v4"
	"io"
	"strconv"
	"strings"
)

func (t multipartTx) Admit(c s.MutationContext) error {
	if err := c.Validate(); err != nil {
		return err
	}
	return t.m.fence(t.t, c.Writer)
}

// Local Badger has no distributed epoch closure; its exporter remains usable.
func (t multipartTx) RecordRecoveryEvent(s.MutationContext, s.OutboxEvent) error { return nil }
func (m *Store) MutateControl(ctx context.Context, r s.ControlMutation) (s.MutationResult, error) {
	return m.multipartOps().Mutate(ctx, r)
}
func (m *Store) DeleteBucket(ctx context.Context, r s.BucketDelete) error {
	return m.multipartOps().DeleteBucket(ctx, r)
}
func (m *Store) GetMutation(ctx context.Context, id string) (s.MutationResult, error) {
	var result s.MutationResult
	err := m.view(ctx, func(t *db.Txn) error {
		var e error
		result, e = get[s.MutationResult](t, key("producer-outcome", id))
		return e
	})
	return result, err
}
func (m *Store) PutPayload(ctx context.Context, id string, body []byte) error {
	c, ok := s.MutationFromContext(ctx)
	if !ok {
		return s.ErrInvalid
	}
	return m.update(ctx, func(t *db.Txn) error {
		tx := multipartTx{m: m, t: t, ctx: ctx}
		if err := tx.Admit(c); err != nil {
			return err
		}
		return operations.ImmutablePayload(tx, id, body)
	})
}
func (m *Store) Open(ctx context.Context, id string) (io.ReadCloser, error) {
	if strings.HasPrefix(id, "attachment/") {
		parts := strings.Split(id, "/")
		if len(parts) != 4 {
			return nil, s.ErrInvalid
		}
		kind, e := strconv.ParseUint(parts[1], 10, 8)
		n, e2 := strconv.ParseUint(parts[3], 10, 32)
		if e != nil || e2 != nil {
			return nil, s.ErrInvalid
		}
		var body []byte
		var err error
		if kind == 1 {
			page, e := m.GetManifestPage(ctx, parts[2], uint32(n))
			if e != nil {
				return nil, e
			}
			body, err = manifest.Encode(1, page)
		} else {
			body, err = m.ReadCompletionPage(ctx, byte(kind), parts[2], uint32(n))
		}
		if err != nil {
			return nil, err
		}
		return io.NopCloser(bytes.NewReader(body)), nil
	}

	var body []byte
	err := m.view(ctx, func(t *db.Txn) error { var e error; body, e = get[[]byte](t, key("payload", id)); return e })
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(body)), nil
}

var _ s.ProducerStore = (*Store)(nil)

func (m *Store) producerGate(t operations.Tx, ctx context.Context, w s.WriterFence) error {
	c, ok := s.MutationFromContext(ctx)
	if !ok {
		if m.requireProducers {
			return s.ErrFenced
		}
		return nil
	}
	if w != (s.WriterFence{}) && w != c.Writer {
		return s.ErrFenced
	}
	return t.Admit(c)
}
