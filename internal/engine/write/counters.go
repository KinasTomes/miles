package write

import (
	"sync/atomic"
	"time"
)

// Counters accumulates physical write outcomes of one process since a fixed
// interval start. They are statistics/metrics observations, never quota,
// billing or GC input. Revived counts each distinct identity whose existing
// canonical location a published pack replaced (archive, Restored or
// unreadable state); orphaned counts race-loser blobs left in the writer's own
// pack, which stay protected garbage until GC eligibility.
type Counters struct {
	since                                          time.Time
	revived, revivedBytes, orphaned, orphanedBytes atomic.Uint64
}

type CounterSnapshot struct {
	Since                                                      time.Time
	RevivedChunks, RevivedBytes, OrphanedChunks, OrphanedBytes uint64
}

func NewCounters(since time.Time) *Counters { return &Counters{since: since.UTC()} }

func (c *Counters) RecordPhysical(revivedChunks, revivedBytes, orphanedChunks, orphanedBytes uint64) {
	if c == nil {
		return
	}
	c.revived.Add(revivedChunks)
	c.revivedBytes.Add(revivedBytes)
	c.orphaned.Add(orphanedChunks)
	c.orphanedBytes.Add(orphanedBytes)
}

func (c *Counters) add(r Result) {
	if c == nil {
		return
	}
	c.RecordPhysical(r.RevivedChunks, r.RevivedBytes, r.OrphanedChunks, r.OrphanedBytes)
}

func (c *Counters) Snapshot() CounterSnapshot {
	return CounterSnapshot{Since: c.since, RevivedChunks: c.revived.Load(), RevivedBytes: c.revivedBytes.Load(), OrphanedChunks: c.orphaned.Load(), OrphanedBytes: c.orphanedBytes.Load()}
}
