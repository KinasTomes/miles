package storage

import "math"

// AdjustReservationQuota computes the replacement counters without changing
// either row. Callers persist the returned quota and ledger in one transaction.
func AdjustReservationQuota(q LogicalQuota, r Reservation, bytes, staged int64) (LogicalQuota, error) {
	if bytes < 0 || bytes > MaxObjectBytes || staged < 0 || staged > MaxObjectBytes ||
		(staged > r.BaseSize && bytes < staged-r.BaseSize) {
		return LogicalQuota{}, ErrInvalid
	}
	if q.Limit < 0 || q.Used < 0 || q.Reserved < r.Bytes || q.Staged < r.StagedBytes ||
		q.Used > q.Limit || q.Reserved > q.Limit-q.Used {
		return LogicalQuota{}, ErrInvalid
	}
	without := q.Reserved - r.Bytes
	if bytes > q.Limit-q.Used-without {
		return LogicalQuota{}, ErrQuotaExceeded
	}
	cap := int64(math.MaxInt64)
	if q.Limit <= math.MaxInt64-MaxObjectBytes {
		cap = q.Limit + MaxObjectBytes
	}
	stagedWithout := q.Staged - r.StagedBytes
	if stagedWithout > cap || staged > cap-stagedWithout {
		return LogicalQuota{}, ErrQuotaExceeded
	}
	q.Reserved = without + bytes
	q.Staged = stagedWithout + staged
	return q, nil
}
