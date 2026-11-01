package dynamodb

import (
	"bytes"
	"context"
	"github.com/KinasTomes/miles/internal/manifest"
	"github.com/KinasTomes/miles/internal/metadata/operations"
	s "github.com/KinasTomes/miles/internal/storage"
	"io"
	"strconv"
	"strings"
)

type ProducerAuthority interface {
	CheckProducer(context.Context, s.MutationContext, Observe) error
}

func (t multipartTx) Admit(c s.MutationContext) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if err := t.store.fence(t.transaction, c.Writer); err != nil {
		return err
	}
	a, ok := t.store.authority.(ProducerAuthority)
	if !ok {
		return s.ErrFenced
	}
	return a.CheckProducer(t.ctx, c, func(k Key) (Item, error) {
		row, err := t.observe(k)
		if err != nil {
			return nil, err
		}
		if row.after == nil {
			return nil, s.ErrNotFound
		}
		return copyItem(row.after), nil
	})
}
func (m *Store) MutateControl(ctx context.Context, r s.ControlMutation) (s.MutationResult, error) {
	return m.multipartOps().Mutate(ctx, r)
}
func (m *Store) DeleteBucket(ctx context.Context, r s.BucketDelete) error {
	return m.multipartOps().DeleteBucket(ctx, r)
}
func (m *Store) GetMutation(ctx context.Context, id string) (s.MutationResult, error) {
	return read[s.MutationResult](m, ctx, key("producer-outcome", id))
}
func (m *Store) PutPayload(ctx context.Context, id string, body []byte) error {
	c, ok := s.MutationFromContext(ctx)
	if !ok {
		return s.ErrInvalid
	}
	return m.update(ctx, func(t *transaction) error {
		tx := multipartTx{t}
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

	body, err := read[[]byte](m, ctx, key("payload", id))
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(body)), nil
}

var _ s.ProducerStore = (*Store)(nil)

func (m *Store) producerGate(t multipartTx, ctx context.Context, w s.WriterFence) error {
	c, ok := s.MutationFromContext(ctx)
	if !ok {
		if m.requireProducers {
			return s.ErrFenced
		}
		if _, shared := m.authority.(*SharedAuthority); shared {
			g, err := r1Read[s.R1Gate](t.transaction, r1GateKey())
			if err != nil {
				return err
			}
			if g.RecoveryEnabled {
				return s.ErrFenced
			}
		}
		return nil
	}
	if w != (s.WriterFence{}) && w != c.Writer {
		return s.ErrFenced
	}
	return t.Admit(c)
}
