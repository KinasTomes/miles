package badger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/KinasTomes/miles/internal/recovery/format"
	"strconv"
	"time"

	"github.com/KinasTomes/miles/internal/manifest"
	"github.com/KinasTomes/miles/internal/metadata/operations"
	s "github.com/KinasTomes/miles/internal/storage"
	db "github.com/dgraph-io/badger/v4"
)

type multipartBridge struct{ m *Store }
type multipartTx struct {
	m   *Store
	t   *db.Txn
	ctx context.Context
}

func (m *Store) multipartOps() operations.Multipart {
	return operations.Multipart{Backend: multipartBridge{m}}
}
func (b multipartBridge) Update(ctx context.Context, fn func(operations.Tx) error) error {
	return b.m.update(ctx, func(t *db.Txn) error { return fn(multipartTx{m: b.m, t: t, ctx: ctx}) })
}
func (b multipartBridge) View(ctx context.Context, fn func(operations.Tx) error) error {
	return b.m.view(ctx, func(t *db.Txn) error { return fn(multipartTx{m: b.m, t: t, ctx: ctx}) })
}
func (b multipartBridge) Now() time.Time            { return b.m.now() }
func (b multipartBridge) Metadata() s.MetadataStore { return b.m }
func (b multipartBridge) Part(ctx context.Context, id string, n uint32) (s.ManifestPage, error) {
	return b.m.GetPartPage(ctx, id, n)
}
func (b multipartBridge) CompletionPage(ctx context.Context, k byte, id string, n uint32) ([]byte, error) {
	return b.m.ReadCompletionPage(ctx, k, id, n)
}
func logicalRecord(kind string, ids []string) string {
	if kind == "heads" {
		return headKey(s.ObjectKey{Bucket: ids[0], BucketID: ids[1], Key: ids[2]})
	}
	if kind == "part-head" {
		n, _ := strconv.ParseUint(ids[1], 10, 32)
		return key(kind, ids[0]) + fmt.Sprintf("/%08x", n)
	}
	return key(kind, ids...)
}
func (t multipartTx) Get(kind string, ids []string, out any) error {
	raw, err := get[json.RawMessage](t.t, logicalRecord(kind, ids))
	if err != nil {
		return err
	}
	if json.Unmarshal(raw, out) != nil {
		return s.ErrUnavailable
	}
	return nil
}
func (t multipartTx) Put(kind string, ids []string, value any) error {
	return put(t.t, logicalRecord(kind, ids), value)
}
func (t multipartTx) Authorize(c, b string) (s.Authorization, error) { return t.m.authorize(t.t, c, b) }

// Badger is the local development authority; it supplies no distributed proof.
func (t multipartTx) Fence(w s.WriterFence, _ string) error {
	if t.m.requireProducers {
		c, ok := s.MutationFromContext(t.ctx)
		if !ok || c.Writer != w {
			return s.ErrFenced
		}
		if err := t.Admit(c); err != nil {
			return err
		}
	}
	return t.m.fence(t.t, w)
}
func (t multipartTx) CloseOperation(op, reservation string) error {
	r, err := get[s.Reservation](t.t, key("reservations", reservation))
	if err != nil {
		return err
	}
	if r.OpID != op {
		return s.ErrConflict
	}
	if _, err := get[outcome](t.t, key("outcomes", op)); err == nil {
		return s.ErrConflict
	} else if !errors.Is(err, s.ErrNotFound) {
		return err
	}
	return put(t.t, key("multipart-op-closed", op), true)
}
func (t multipartTx) ResetOperation(op, reservation string) error {
	if _, err := get[outcome](t.t, key("outcomes", op)); err == nil {
		return s.ErrConflict
	} else if !errors.Is(err, s.ErrNotFound) {
		return err
	}
	bound, err := get[string](t.t, key("reservation-ops", op))
	if err == nil && bound != reservation {
		return s.ErrConflict
	}
	if err != nil && !errors.Is(err, s.ErrNotFound) {
		return err
	}
	return put(t.t, key("multipart-op-closed", op), false)
}
func (b multipartBridge) Parts(ctx context.Context, id string, after uint32, limit int) (s.PartListPage, error) {
	var result s.PartListPage
	if limit < 1 || limit > 1000 {
		return result, s.ErrInvalid
	}
	err := b.m.view(ctx, func(t *db.Txn) error {
		return scan(t, key("part-head", id)+"/", func(k string) error {
			p, err := get[s.UploadPart](t, k)
			if err != nil {
				return err
			}
			if p.Number <= after {
				return nil
			}
			if len(result.Parts) == limit {
				result.Next = result.Parts[limit-1].Number
				return stopScan
			}
			result.Parts = append(result.Parts, p)
			return nil
		})
	})
	if errors.Is(err, stopScan) {
		err = nil
	}
	return result, err
}
func (b multipartBridge) Publishers(ctx context.Context, id, after string, limit int) (s.PublisherPage, error) {
	var result s.PublisherPage
	if limit < 1 || limit > 1000 {
		return result, s.ErrInvalid
	}
	err := b.m.view(ctx, func(t *db.Txn) error {
		return scan(t, key("publisher", id)+"/", func(k string) error {
			p, err := get[s.PartPublisher](t, k)
			if err != nil {
				return err
			}
			if p.ID <= after {
				return nil
			}
			if len(result.Publishers) == limit {
				result.Next = result.Publishers[limit-1].ID
				return stopScan
			}
			result.Publishers = append(result.Publishers, p)
			return nil
		})
	})
	if errors.Is(err, stopScan) {
		err = nil
	}
	return result, err
}
func (m *Store) WriteCompletionPage(ctx context.Context, kind byte, id string, n uint32, body []byte, w s.WriterFence) error {
	if _, err := manifest.DecodeCompletionPage(body, kind, id, n); err != nil {
		return err
	}
	k := key("completion-pages", id) + fmt.Sprintf("/%08x", n)
	return m.update(ctx, func(t *db.Txn) error {
		if err := m.fence(t, w); err != nil {
			return err
		}
		old, err := get[[]byte](t, k)
		if err == nil {
			if !bytes.Equal(old, body) {
				return s.ErrConflict
			}
			return nil
		}
		if !errors.Is(err, s.ErrNotFound) {
			return err
		}
		return put(t, k, body)
	})
}
func (m *Store) ReadCompletionPage(ctx context.Context, kind byte, id string, n uint32) ([]byte, error) {
	var body []byte
	err := m.view(ctx, func(t *db.Txn) error {
		var e error
		body, e = get[[]byte](t, key("completion-pages", id)+fmt.Sprintf("/%08x", n))
		return e
	})
	if err != nil {
		return nil, err
	}
	_, err = manifest.DecodeCompletionPage(body, kind, id, n)
	return body, err
}

func (t multipartTx) Attach(kind byte, owner, domain string, pages uint32, length int64, digest string) (format.PayloadDependency, error) {
	return operations.Attach(t, kind, owner, domain, pages, length, digest, func(n uint32) ([]byte, error) {
		if kind == 1 {
			page, err := t.m.GetManifestPage(t.ctx, owner, n)
			if err != nil {
				return nil, err
			}
			if page.Domain != domain {
				return nil, s.ErrInvalid
			}
			return manifest.Encode(1, page)
		}
		return t.m.ReadCompletionPage(t.ctx, kind, owner, n)
	})
}
