// Package auth holds the authorization snapshots of API v2 2.2.3. A node
// reuses what it read from authoritative metadata about a principal (user,
// credential or bearer token, bucket, grant) for at most MaxAge of its
// monotonic clock, so a revoke, disable, grant change or bucket deletion
// committed at T reaches every admission on every node from T + MaxAge on.
// Writes never publish on a snapshot: their metadata transaction checks the
// same items again (storage.MetadataStore.CommitObject).
package auth

import (
	"context"
	"sync"
	"time"

	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/storage"
)

// MaxAge bounds the age of every snapshot (API v2 2.2.3).
const MaxAge = 5 * time.Second

// minSweep is the entry count below which the cache does not look for
// expired entries.
const minSweep = 64

// Cache keeps what Load read from authoritative metadata until MaxAge of
// clock time after the read, or until the expiry Load returned (a bearer
// token's expires_at) if that comes first: valid_until of API v2 2.2.3. A
// hit does not refresh the age and a failed load is not kept, so the next
// request reads again. Snapshots are local to the node that read them.
type Cache[K comparable, V any] struct {
	clock clock.Clock
	// load reads key consistently from authoritative metadata. A zero
	// expiry means the value does not expire on its own.
	load func(ctx context.Context, key K) (value V, expires time.Time, err error)

	mu      sync.Mutex
	entries map[K]entry[V]
	sweepAt int // entry count that triggers the next removal of expired entries
}

type entry[V any] struct {
	value      V
	validUntil clock.Instant
}

func NewCache[K comparable, V any](clk clock.Clock, load func(ctx context.Context, key K) (V, time.Time, error)) *Cache[K, V] {
	return &Cache[K, V]{clock: clk, load: load, entries: map[K]entry[V]{}, sweepAt: minSweep}
}

// Get returns the snapshot of key, reading it again once the cached one
// reached its bound.
func (c *Cache[K, V]) Get(ctx context.Context, key K) (V, error) {
	// The read happens after now, so a bound counted from now is never late.
	now := c.clock.Now()
	c.mu.Lock()
	e, ok := c.entries[key]
	c.mu.Unlock()
	if ok && before(now.Instant, e.validUntil) {
		return e.value, nil
	}
	v, expires, err := c.load(ctx, key)
	if err != nil {
		var zero V
		return zero, err
	}
	bound := MaxAge
	if !expires.IsZero() {
		bound = min(bound, expires.Sub(now.WallUTC))
	}
	validUntil, err := now.Instant.Add(bound)
	if bound <= 0 || err != nil {
		return v, nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= c.sweepAt {
		for k, e := range c.entries {
			if !before(now.Instant, e.validUntil) {
				delete(c.entries, k)
			}
		}
		c.sweepAt = max(minSweep, 2*len(c.entries))
	}
	c.entries[key] = entry[V]{value: v, validUntil: validUntil}
	return v, nil
}

// before reports a < b; instants of another clock domain never compare as
// before, so such an entry is read again.
func before(a, b clock.Instant) bool {
	c, err := a.Compare(b)
	return err == nil && c < 0
}

// Authorizer is the authoritative rights projection of
// storage.MetadataStore.Authorize.
type Authorizer interface {
	Authorize(ctx context.Context, credentialID, bucket string) (storage.Authorization, error)
}

// Snapshots serves Authorize through a Cache: the authorization snapshot of
// an access key on a bucket, with the credential, its user, the bucket and
// the grant read together and expiring together.
type Snapshots struct {
	cache *Cache[snapshotKey, storage.Authorization]
}

type snapshotKey struct{ credentialID, bucket string }

func NewSnapshots(store Authorizer, clk clock.Clock) *Snapshots {
	return &Snapshots{cache: NewCache(clk, func(ctx context.Context, k snapshotKey) (storage.Authorization, time.Time, error) {
		a, err := store.Authorize(ctx, k.credentialID, k.bucket)
		return a, time.Time{}, err
	})}
}

func (s *Snapshots) Authorize(ctx context.Context, credentialID, bucket string) (storage.Authorization, error) {
	return s.cache.Get(ctx, snapshotKey{credentialID, bucket})
}
