// Package badger provides a durable, single-node development MetadataStore.
// It is not a distributed R1 coordinator or an R2 recovery implementation.
package badger

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	s "github.com/KinasTomes/miles/internal/storage"
	db "github.com/dgraph-io/badger/v4"
)

// Options intentionally does not expose unsafe in-memory or asynchronous writes.
type Options struct {
	Path string
	Now  func() time.Time
}
type Store struct {
	db            *db.DB
	now           func() time.Time
	writes        sync.Mutex
	beforePublish func() error
	afterPublish  func() error
	reconcile     func() error
}
type record struct {
	Schema uint32
	Value  json.RawMessage
}

func Open(o Options) (*Store, error) {
	if o.Path == "" {
		return nil, s.ErrInvalid
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	d, err := db.Open(db.DefaultOptions(o.Path).WithSyncWrites(true).WithDetectConflicts(true).WithLogger(nil))
	if err != nil {
		return nil, backend(err)
	}
	m := &Store{db: d, now: o.Now}
	err = m.update(context.Background(), func(t *db.Txn) error {
		v, err := get[uint32](t, "schema")
		if errors.Is(err, s.ErrNotFound) {
			it := t.NewIterator(db.DefaultIteratorOptions)
			it.Rewind()
			populated := it.Valid()
			it.Close()
			if populated {
				return fmt.Errorf("%w: missing schema in populated database", s.ErrUnavailable)
			}
			return put(t, "schema", uint32(1))
		}
		if err != nil {
			return err
		}
		if v != 1 {
			return fmt.Errorf("%w: unsupported metadata schema %d", s.ErrInvalid, v)
		}
		return nil
	})
	if err != nil {
		_ = d.Close()
		return nil, err
	}
	return m, nil
}
func (m *Store) Close() error { return backend(m.db.Close()) }
func backend(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, db.ErrKeyNotFound) {
		return s.ErrNotFound
	}
	if errors.Is(err, db.ErrConflict) {
		return s.ErrConflict
	}
	if errors.Is(err, db.ErrTxnTooBig) {
		return fmt.Errorf("%w: transaction too large", s.ErrInvalid)
	}
	return fmt.Errorf("%w: %v", s.ErrUnavailable, err)
}

// A local writer gate prevents retry storms. Badger still supplies the atomic
// snapshot/commit and process lock; no authoritative records live in this mutex.
func (m *Store) update(ctx context.Context, fn func(*db.Txn) error) error {
	m.writes.Lock()
	defer m.writes.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	t := m.db.NewTransaction(true)
	defer t.Discard()
	if err := fn(t); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return backend(t.Commit())
}
func (m *Store) view(ctx context.Context, fn func(*db.Txn) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	t := m.db.NewTransaction(false)
	defer t.Discard()
	if err := fn(t); err != nil {
		return err
	}
	return ctx.Err()
}

// Hex components cannot collide with separators and preserve UTF-8 byte order.
func key(table string, parts ...string) string {
	for _, p := range parts {
		table += "/" + hex.EncodeToString([]byte(p))
	}
	return table
}
func get[T any](t *db.Txn, k string) (T, error) {
	var v T
	item, err := t.Get([]byte(k))
	if err != nil {
		return v, backend(err)
	}
	err = item.Value(func(b []byte) error {
		var r record
		if json.Unmarshal(b, &r) != nil || r.Schema != 1 || len(r.Value) == 0 || bytes.Equal(bytes.TrimSpace(r.Value), []byte("null")) {
			return fmt.Errorf("%w: corrupt record %s", s.ErrUnavailable, k)
		}
		if err := json.Unmarshal(r.Value, &v); err != nil {
			return fmt.Errorf("%w: decode %s: %v", s.ErrUnavailable, k, err)
		}
		if c, ok := any(&v).(*s.Chunk); ok {
			// Required CRC field: omitted/null must not silently become valid zero.
			var fields struct{ Location struct{ CRC32C *uint32 } }
			if json.Unmarshal(r.Value, &fields) != nil || fields.Location.CRC32C == nil || c.Validate() != nil || chunkKey(c.Key) != k {
				return fmt.Errorf("%w: corrupt chunk record", s.ErrUnavailable)
			}
		}
		return nil
	})
	return v, err
}
func optional[T any](t *db.Txn, k string) (T, error) {
	v, err := get[T](t, k)
	if errors.Is(err, s.ErrNotFound) {
		err = nil
	}
	return v, err
}
func put[T any](t *db.Txn, k string, v T) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("%w: encode record", s.ErrInvalid)
	}
	b, err = json.Marshal(record{1, b})
	if err != nil {
		return err
	}
	return backend(t.Set([]byte(k), b))
}
func exists(t *db.Txn, k string) (bool, error) {
	_, err := t.Get([]byte(k))
	if errors.Is(err, db.ErrKeyNotFound) {
		return false, nil
	}
	return err == nil, backend(err)
}
func scan(t *db.Txn, prefix string, fn func(string) error) error {
	opts := db.DefaultIteratorOptions
	opts.PrefetchValues = false
	it := t.NewIterator(opts)
	defer it.Close()
	p := []byte(prefix)
	for it.Seek(p); it.ValidForPrefix(p); it.Next() {
		if err := fn(string(it.Item().KeyCopy(nil))); err != nil {
			return err
		}
	}
	return nil
}
func read[T any](m *Store, ctx context.Context, k string) (T, error) {
	var v T
	err := m.view(ctx, func(t *db.Txn) error { var e error; v, e = get[T](t, k); return e })
	return v, err
}

// Bootstrap initializes authoritative dev identities/configuration once. It is
// deliberately not an upsert/admin bypass and refuses an already seeded store.
type Bootstrap struct {
	Buckets     []s.Bucket
	Users       []s.User
	Credentials []s.Credential
	Grants      []s.Grant
	Quotas      []s.LogicalQuota
	Packs       []s.PackInfo
	Writers     []s.WriterLease
	Uploads     []s.Completion
}

// Bootstrapped reports whether one-time dev provisioning has committed.
func (m *Store) Bootstrapped(ctx context.Context) (bool, error) {
	var seeded bool
	err := m.view(ctx, func(t *db.Txn) error {
		var err error
		seeded, err = exists(t, "bootstrapped")
		return err
	})
	return seeded, err
}

func (m *Store) Bootstrap(ctx context.Context, b Bootstrap) error {
	return m.update(ctx, func(t *db.Txn) error {
		seeded, err := exists(t, "bootstrapped")
		if err != nil {
			return err
		}
		if seeded {
			return s.ErrConflict
		}
		for _, v := range b.Buckets {
			if v.Name == "" || v.ID == "" || v.OwnerID == "" {
				return s.ErrInvalid
			}
			if err := put(t, key("buckets", v.Name), v); err != nil {
				return err
			}
		}
		for _, v := range b.Users {
			if v.ID == "" {
				return s.ErrInvalid
			}
			if err := put(t, key("users", v.ID), v); err != nil {
				return err
			}
		}
		for _, v := range b.Credentials {
			if v.ID == "" || v.UserID == "" {
				return s.ErrInvalid
			}
			if err := put(t, key("credentials", v.ID), v); err != nil {
				return err
			}
		}
		for _, v := range b.Grants {
			if err := put(t, key("grants", v.BucketID, v.UserID), v); err != nil {
				return err
			}
		}
		for _, v := range b.Quotas {
			if v.OwnerID == "" || v.Limit < 0 || v.Used < 0 || v.Reserved < 0 || v.Staged < 0 || v.Used > v.Limit || v.Reserved > v.Limit-v.Used {
				return s.ErrInvalid
			}
			if err := put(t, key("quotas", v.OwnerID), v); err != nil {
				return err
			}
		}
		for _, v := range b.Packs {
			if v.Ref.Validate() != nil || v.Size < 0 {
				return s.ErrInvalid
			}
			if err := put(t, packKey(v.Ref), v); err != nil {
				return err
			}
		}
		for _, v := range b.Writers {
			if v.Fence.LeaseID == "" || v.Fence.ServiceIncarnation == "" || v.Fence.Epoch == 0 || v.ExpiresAt.IsZero() {
				return s.ErrInvalid
			}
			if err := put(t, key("writers", v.Fence.LeaseID), v); err != nil {
				return err
			}
		}
		for _, v := range b.Uploads {
			if err := put(t, key("uploads", v.UploadID), v); err != nil {
				return err
			}
		}
		return put(t, "bootstrapped", true)
	})
}
func chunkKey(k s.ChunkKey) string { return key("chunks", k.Domain, k.Hash) }
func packKey(k s.PackRef) string   { return key("packs", k.ID, k.Generation) }
func headKey(k s.ObjectKey) string { return key("heads", k.Bucket, k.BucketID, k.Key) }
func (m *Store) fence(t *db.Txn, w s.WriterFence) error {
	v, err := optional[s.WriterLease](t, key("writers", w.LeaseID))
	if err != nil {
		return err
	}
	if w.LeaseID == "" || w.ServiceIncarnation == "" || w.Epoch == 0 || v.Fence != w || !m.now().Before(v.ExpiresAt) {
		return s.ErrFenced
	}
	return nil
}
func (m *Store) authorize(t *db.Txn, credential, bucket string) (s.Authorization, error) {
	c, err := optional[s.Credential](t, key("credentials", credential))
	if err != nil {
		return s.Authorization{}, err
	}
	u, err := optional[s.User](t, key("users", c.UserID))
	if err != nil {
		return s.Authorization{}, err
	}
	if !c.Active || !u.Active {
		return s.Authorization{}, s.ErrAccessDenied
	}
	b, err := get[s.Bucket](t, key("buckets", bucket))
	if err != nil {
		if errors.Is(err, s.ErrNotFound) {
			return s.Authorization{}, s.ErrBucketNotFound
		}
		return s.Authorization{}, err
	}
	if !b.Active {
		return s.Authorization{}, s.ErrBucketNotFound
	}
	g, err := optional[s.Grant](t, key("grants", b.ID, u.ID))
	if err != nil {
		return s.Authorization{}, err
	}
	return s.Authorization{UserID: u.ID, CredentialID: c.ID, Bucket: b, Read: u.ID == b.OwnerID || g.Read, Write: u.ID == b.OwnerID || g.Write, SystemAdmin: c.SystemAdmin}, nil
}
