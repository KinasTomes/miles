package cache

import (
	"bytes"
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/KinasTomes/miles/internal/storage"
)

type diskEntry struct {
	key  storage.CacheKey
	size int64
	sum  [32]byte
}

// Disk is disposable, single-process storage in a dedicated directory. The
// exclusive owner marker deliberately fails closed after a crash; recovery must
// remove that marker only after proving the previous process is stopped.
// Entries are never trusted as authoritative data and are discarded on reopen.
type Disk struct {
	mu        sync.Mutex
	root      string
	limits    Limits
	entries   map[storage.CacheKey]*list.Element
	order     list.List
	used      int64
	evictions uint64
	closed    bool
}

func NewDisk(root string, limits Limits) (*Disk, error) {
	if root == "" || limits.CapacityBytes <= 0 || limits.MaxEntries <= 0 || limits.MaxCopyBytes <= 0 {
		return nil, storage.ErrInvalid
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, storage.ErrInvalid
	}
	lock, err := os.OpenFile(filepath.Join(root, "owner.lock"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	if err = lock.Close(); err != nil {
		return nil, err
	}
	fail := func(err error) (*Disk, error) { _ = os.Remove(filepath.Join(root, "owner.lock")); return nil, err }
	files, err := os.ReadDir(root)
	if err != nil {
		return fail(err)
	}
	// Validate the complete directory before deleting anything. It must contain
	// only our owner marker and hashed disposable entries, never user files.
	for _, f := range files {
		if f.Name() == "owner.lock" {
			continue
		}
		name := f.Name()
		if len(name) != 69 || filepath.Ext(name) != ".blob" || f.Type()&os.ModeSymlink != 0 || f.IsDir() {
			return fail(storage.ErrInvalid)
		}
		if _, err := hex.DecodeString(name[:64]); err != nil {
			return fail(storage.ErrInvalid)
		}
	}
	for _, f := range files {
		if f.Name() != "owner.lock" {
			if err = os.Remove(filepath.Join(root, f.Name())); err != nil {
				return fail(err)
			}
		}
	}
	return &Disk{root: root, limits: limits, entries: map[storage.CacheKey]*list.Element{}}, nil
}
func (d *Disk) path(k storage.CacheKey) string {
	b, _ := json.Marshal(k)
	sum := sha256.Sum256(b)
	return filepath.Join(d.root, hex.EncodeToString(sum[:])+".blob")
}
func (d *Disk) remove(e *list.Element) error {
	v := e.Value.(diskEntry)
	if err := os.Remove(d.path(v.key)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	delete(d.entries, v.key)
	d.used -= v.size
	d.order.Remove(e)
	return nil
}
func (d *Disk) evict(ref storage.PackRef) error {
	for k, e := range d.entries {
		if k.Pack == ref {
			if err := d.remove(e); err != nil {
				return err
			}
		}
	}
	return nil
}
func (d *Disk) Get(ctx context.Context, k storage.CacheKey) ([]byte, bool, error) {
	if err := validKey(k); err != nil {
		return nil, false, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if d.closed {
		return nil, false, storage.ErrUnavailable
	}
	e := d.entries[k]
	if e == nil {
		return nil, false, nil
	}
	v := e.Value.(diskEntry)
	info, err := os.Lstat(d.path(k))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			_ = d.remove(e)
			return nil, false, nil
		}
		return nil, false, err
	}
	if !info.Mode().IsRegular() || info.Size() != v.size {
		_ = d.evict(k.Pack)
		return nil, false, storage.ErrConflict
	}
	f, err := os.Open(d.path(k))
	if err != nil {
		return nil, false, err
	}
	b := make([]byte, int(v.size))
	_, readErr := io.ReadFull(f, b)
	if readErr == nil {
		var extra [1]byte
		n, e := f.Read(extra[:])
		if n != 0 || e != io.EOF {
			readErr = storage.ErrConflict
		}
	}
	closeErr := f.Close()
	if err = errors.Join(readErr, closeErr); err != nil {
		if errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF) || errors.Is(readErr, storage.ErrConflict) {
			_ = d.evict(k.Pack)
			return nil, false, storage.ErrConflict
		}
		return nil, false, err
	}
	if int64(len(b)) != v.size || sha256.Sum256(b) != v.sum {
		_ = d.evict(k.Pack)
		return nil, false, storage.ErrConflict
	}
	if err = ctx.Err(); err != nil {
		return nil, false, err
	}
	d.order.MoveToFront(e)
	return b, true, nil
}
func (d *Disk) Put(ctx context.Context, k storage.CacheKey, b []byte) error {
	if err := validKey(k); err != nil {
		return err
	}
	if int64(len(b)) != k.Range.Length {
		return storage.ErrInvalid
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if d.closed {
		return storage.ErrUnavailable
	}
	if e := d.entries[k]; e != nil {
		v := e.Value.(diskEntry)
		if v.sum != sha256.Sum256(b) {
			_ = d.evict(k.Pack)
			return storage.ErrConflict
		}
		d.order.MoveToFront(e)
		return nil
	}
	n := int64(len(b))
	if n > d.limits.CapacityBytes || n > d.limits.MaxCopyBytes {
		return nil
	}
	for d.used > d.limits.CapacityBytes-n || len(d.entries) >= d.limits.MaxEntries {
		if err := d.remove(d.order.Back()); err != nil {
			return err
		}
		d.evictions++
	}
	// Exclusive create means no accidental overwrite of a file changed externally.
	f, err := os.OpenFile(d.path(k), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	written, writeErr := io.Copy(f, bytes.NewReader(b))
	err = errors.Join(writeErr, f.Close(), ctx.Err())
	if err != nil || written != n {
		if removeErr := os.Remove(d.path(k)); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			// Keep failed cleanup charged against capacity. A later read detects
			// this incomplete entry and eviction can retry the removal.
			d.entries[k] = d.order.PushFront(diskEntry{k, n, [32]byte{}})
			d.used += n
			err = errors.Join(err, removeErr)
		}
		if err != nil {
			return err
		}
		return io.ErrShortWrite
	}
	d.entries[k] = d.order.PushFront(diskEntry{k, n, sha256.Sum256(b)})
	d.used += n
	return nil
}
func (d *Disk) Evict(ctx context.Context, ref storage.PackRef) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if d.closed {
		return storage.ErrUnavailable
	}
	return d.evict(ref)
}
func (d *Disk) purge() error {
	for _, e := range d.entries {
		if err := d.remove(e); err != nil {
			return err
		}
	}
	return nil
}
func (d *Disk) Purge(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if d.closed {
		return storage.ErrUnavailable
	}
	return d.purge()
}
func (d *Disk) Occupancy() (int64, int, uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.used, len(d.entries), d.evictions
}
func (d *Disk) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil
	}
	if err := d.purge(); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(d.root, "owner.lock")); err != nil {
		return err
	}
	d.closed = true
	return nil
}
