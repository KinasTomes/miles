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
	"errors"
	"sync"
	"time"

	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/storage"
)

// MaxAge bounds the age of every snapshot (API v2 2.2.3).
const MaxAge = 5 * time.Second

// ErrExpiredDuringLoad reports a value that reached Get past its own
// valid_until: the load outlived the bound of what it read, or the value
// was already expired when read. Get reads again once before failing, so
// callers fail closed on it like on any load error.
var ErrExpiredDuringLoad = errors.New("auth: load outlived valid_until")

// minSweep is the entry count below which the cache does not look for
// expired entries.
const minSweep = 64

// Cache keeps what Load read from authoritative metadata until MaxAge of
// clock time after the read, or until the expiry Load returned (a bearer
// token's expires_at) if that comes first: valid_until of API v2 2.2.3. A
// hit does not refresh the age and a failed load is not kept, so the next
// request reads again; a value that reaches Get past its bound is not
// returned either but read again, and once that reread fails the same way
// Get reports ErrExpiredDuringLoad. Snapshots are local to the node that
// read them.
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
// reached its bound. Every value it returns is within valid_until at
// return time: a load that returns past the bound of what it read is read
// again from a fresh now, without refreshing the age of the first read,
// and a reread that also outlives its bound reports ErrExpiredDuringLoad
// instead of the stale value.
func (c *Cache[K, V]) Get(ctx context.Context, key K) (V, error) {
	var zero V
	for range 2 {
		// The read happens after now, so a bound counted from now is
		// never late.
		now := c.clock.Now()
		c.mu.Lock()
		e, ok := c.entries[key]
		c.mu.Unlock()
		if ok && before(now.Instant, e.validUntil) {
			return e.value, nil
		}
		v, expires, err := c.load(ctx, key)
		if err != nil {
			return zero, err
		}
		bound := MaxAge
		if !expires.IsZero() {
			bound = min(bound, expires.Sub(now.WallUTC))
		}
		validUntil, err := now.Instant.Add(bound)
		if err != nil {
			return v, nil
		}
		// The bound counted from the read must still hold once the load
		// returns; an expired one, which bound <= 0 also is, is never
		// served or kept.
		if after := c.clock.Now().Instant; !before(after, validUntil) {
			continue
		}

		c.mu.Lock()
		if len(c.entries) >= c.sweepAt {
			for k, e := range c.entries {
				if !before(now.Instant, e.validUntil) {
					delete(c.entries, k)
				}
			}
			c.sweepAt = max(minSweep, 2*len(c.entries))
		}
		c.entries[key] = entry[V]{value: v, validUntil: validUntil}
		c.mu.Unlock()
		return v, nil
	}
	return zero, ErrExpiredDuringLoad
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
