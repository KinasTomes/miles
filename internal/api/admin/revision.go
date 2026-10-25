package admin

import (
	"errors"
	"fmt"
	"strconv"
)

// MaxRevision is the largest revision: 2^53-1, exact in JSON numbers.
const MaxRevision = 1<<53 - 1

// ErrPrecondition wraps every Precondition failure; all are 400
// invalid_request (API §6.5.2). The wrapped message names the field.
var ErrPrecondition = errors.New("invalid precondition")

// Precondition resolves the expected revision of a CAS write from the body
// expected_revision (nil when absent) and the If-Match header values
// (API §6.5.2): If-Match exactly one strong ETag "<decimal>" in
// 1..MaxRevision, canonical decimal, no "*", "W/" or list; at least one
// present; both present must be equal. expected_revision is 1..MaxRevision,
// or 0 when create is set: the route may create the resource (PUT grant)
// and 0 means it must not exist yet. Routes on a resource that always
// exists (policy, bucket policy, user) reject 0 (clock-and-policy §6.3).
func Precondition(expected *uint64, ifMatch []string, create bool) (uint64, error) {
	if expected != nil && (*expected > MaxRevision || *expected == 0 && !create) {
		low := 1
		if create {
			low = 0
		}
		return 0, fmt.Errorf("%w: expected_revision out of range %d..%d", ErrPrecondition, low, uint64(MaxRevision))
	}
	if len(ifMatch) > 1 {
		return 0, fmt.Errorf("%w: If-Match must be one header", ErrPrecondition)
	}
	if len(ifMatch) == 0 {
		if expected == nil {
			return 0, fmt.Errorf("%w: expected_revision or If-Match required", ErrPrecondition)
		}
		return *expected, nil
	}
	rev, ok := parseETag(ifMatch[0])
	if !ok {
		return 0, fmt.Errorf("%w: If-Match must be one strong ETag \"<revision>\" in 1..%d", ErrPrecondition, uint64(MaxRevision))
	}
	if expected != nil && *expected != rev {
		return 0, fmt.Errorf("%w: expected_revision and If-Match disagree", ErrPrecondition)
	}
	return rev, nil
}

func parseETag(v string) (uint64, bool) {
	if len(v) < 3 || v[0] != '"' || v[len(v)-1] != '"' {
		return 0, false
	}
	d := v[1 : len(v)-1]
	if d[0] == '0' { // also rejects "0"
		return 0, false
	}
	for i := range len(d) {
		if d[i] < '0' || d[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseUint(d, 10, 64)
	if err != nil || n > MaxRevision {
		return 0, false
	}
	return n, true
}

// ETag formats a revision for the ETag response header.
func ETag(rev uint64) string { return `"` + strconv.FormatUint(rev, 10) + `"` }
