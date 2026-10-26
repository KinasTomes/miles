package fake

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math/big"
	"reflect"
	"sort"
	"strings"

	s "github.com/KinasTomes/miles/internal/storage"
)

func (m *Metadata) ListAccessibleBuckets(ctx context.Context, r s.BucketListRequest) (s.BucketListPage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return s.BucketListPage{}, err
	}
	if r.PrincipalID == "" || r.Limit < 1 || r.Limit > 1000 {
		return s.BucketListPage{}, s.ErrInvalid
	}
	if u, ok := m.users[r.PrincipalID]; !ok || !u.Active {
		return s.BucketListPage{}, s.ErrAccessDenied
	}
	buckets := []s.Bucket{}
	for _, b := range m.buckets {
		g := m.grants[[2]string{b.ID, r.PrincipalID}]
		if b.Active && b.Name > r.After && (b.OwnerID == r.PrincipalID || g.Read || g.Write) {
			buckets = append(buckets, b)
		}
	}
	sort.Slice(buckets, func(i, j int) bool { return buckets[i].Name < buckets[j].Name })
	out := s.BucketListPage{Buckets: buckets}
	if len(buckets) > r.Limit {
		out.Buckets = buckets[:r.Limit]
		out.NextAfter = out.Buckets[len(out.Buckets)-1].Name
	}
	return out, nil
}
func (m *Metadata) ListObjects(ctx context.Context, r s.ListObjectsRequest) (s.ObjectListPage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return s.ObjectListPage{}, err
	}
	if r.PrincipalID == "" || r.Bucket == "" || r.BucketID == "" || r.MaxKeys < 0 || r.MaxKeys > 1000 {
		return s.ObjectListPage{}, s.ErrInvalid
	}
	if u, ok := m.users[r.PrincipalID]; !ok || !u.Active {
		return s.ObjectListPage{}, s.ErrAccessDenied
	}
	b, ok := m.buckets[r.Bucket]
	if !ok || !b.Active || b.ID != r.BucketID {
		return s.ObjectListPage{}, s.ErrNotFound
	}
	if b.OwnerID != r.PrincipalID && !m.grants[[2]string{b.ID, r.PrincipalID}].Read {
		return s.ObjectListPage{}, s.ErrAccessDenied
	}
	cursor := s.ObjectListCursor{PrincipalID: r.PrincipalID, Bucket: r.Bucket, BucketID: r.BucketID, Prefix: r.Prefix, Delimiter: r.Delimiter, LastScannedKey: r.StartAfter}
	if r.Cursor != nil {
		c := r.Cursor
		if c.PrincipalID != r.PrincipalID || c.Bucket != r.Bucket || c.BucketID != r.BucketID || c.Prefix != r.Prefix || c.Delimiter != r.Delimiter {
			return s.ObjectListPage{}, s.ErrInvalid
		}
		cursor = *c
	}
	out := s.ObjectListPage{Objects: []s.ObjectHead{}, CommonPrefixes: []string{}}
	if r.MaxKeys == 0 {
		return out, nil
	}
	keys := []s.ObjectKey{}
	for k := range m.heads {
		if k.Bucket == r.Bucket && k.BucketID == r.BucketID && k.Key > cursor.LastScannedKey && strings.HasPrefix(k.Key, r.Prefix) {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].Key < keys[j].Key })
	for _, key := range keys {
		head := m.heads[key]
		if head.Deleted {
			cursor.LastScannedKey = key.Key
			continue
		}
		common := ""
		if r.Delimiter != "" {
			if i := strings.Index(strings.TrimPrefix(key.Key, r.Prefix), r.Delimiter); i >= 0 {
				common = key.Key[:len(r.Prefix)+i+len(r.Delimiter)]
			}
		}
		if common != "" && (common == cursor.LastCommonPrefix || (r.Cursor == nil && common <= r.StartAfter)) {
			cursor.LastScannedKey = key.Key
			continue
		}
		if len(out.Objects)+len(out.CommonPrefixes) == r.MaxKeys {
			out.Next = &cursor
			return out, nil
		}
		cursor.LastScannedKey = key.Key
		if common != "" {
			out.CommonPrefixes = append(out.CommonPrefixes, common)
			cursor.LastCommonPrefix = common
		} else {
			out.Objects = append(out.Objects, copyOf(head))
			cursor.LastCommonPrefix = ""
		}
	}
	return out, nil
}
func (m *Metadata) UpdateAccessStats(ctx context.Context, updates []s.PackAccess) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, u := range updates {
		if err := u.Pack.Validate(); err != nil {
			return err
		}
		if u.LastAccess.IsZero() {
			return s.ErrInvalid
		}
		if _, ok := m.packs[u.Pack]; !ok {
			return s.ErrNotFound
		}
	}
	for _, u := range updates {
		p := m.packs[u.Pack]
		if u.LastAccess.After(p.LastAccess) {
			p.LastAccess = u.LastAccess
			m.packs[u.Pack] = p
		}
	}
	return nil
}
func packLess(a, b s.PackRef) bool {
	return a.ID < b.ID || (a.ID == b.ID && a.Generation < b.Generation)
}
func (m *Metadata) ListPacksByState(ctx context.Context, r s.PackListRequest) (s.PackListPage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return s.PackListPage{}, err
	}
	switch r.State {
	case "Warm", "Cold", "Restoring", "Restored", "Transitioning", "Retiring", "Deleting":
	default:
		return s.PackListPage{}, s.ErrInvalid
	}
	if r.Limit < 1 || r.Limit > 1000 {
		return s.PackListPage{}, s.ErrInvalid
	}
	if r.After != nil {
		if err := r.After.Validate(); err != nil {
			return s.PackListPage{}, err
		}
	}
	packs := []s.PackInfo{}
	for _, p := range m.packs {
		if p.State == r.State && (r.After == nil || packLess(*r.After, p.Ref)) {
			packs = append(packs, p)
		}
	}
	sort.Slice(packs, func(i, j int) bool { return packLess(packs[i].Ref, packs[j].Ref) })
	out := s.PackListPage{Packs: packs}
	if len(packs) > r.Limit {
		out.Packs = packs[:r.Limit]
		last := out.Packs[len(out.Packs)-1].Ref
		out.NextAfter = &last
	}
	return out, nil
}

// Exact JSON-number comparison prevents policy no-ops from using float64.
func policyValue(v any) (any, error) {
	switch x := v.(type) {
	case json.Number:
		r, ok := new(big.Rat).SetString(string(x))
		if !ok {
			return nil, s.ErrInvalid
		}
		return r, nil
	case map[string]any:
		for k, child := range x {
			value, err := policyValue(child)
			if err != nil {
				return nil, err
			}
			x[k] = value
		}
	case []any:
		for i, child := range x {
			value, err := policyValue(child)
			if err != nil {
				return nil, err
			}
			x[i] = value
		}
	}
	return v, nil
}
func readPolicy(doc []byte) (any, error) {
	d := json.NewDecoder(bytes.NewReader(doc))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return nil, s.ErrInvalid
	}
	if _, ok := v.(map[string]any); !ok {
		return nil, s.ErrInvalid
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return nil, s.ErrInvalid
	}
	return policyValue(v)
}
func (m *Metadata) GetPolicy(ctx context.Context, revision uint64) (s.PolicySnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return s.PolicySnapshot{}, err
	}
	if revision == 0 {
		revision = m.currentPolicy
	}
	v, ok := m.policies[revision]
	if !ok {
		return s.PolicySnapshot{}, s.ErrNotFound
	}
	return copyOf(v), nil
}
func (m *Metadata) PutPolicy(ctx context.Context, r s.PolicyUpdate) (s.PolicySnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return s.PolicySnapshot{}, err
	}
	if err := m.fence(r.Writer); err != nil {
		return s.PolicySnapshot{}, err
	}
	c, ok := m.credentials[r.CredentialID]
	u := m.users[c.UserID]
	if !ok || !c.Active || !u.Active || !c.SystemAdmin {
		return s.PolicySnapshot{}, s.ErrAccessDenied
	}
	if m.currentPolicy != r.ExpectedRevision {
		return s.PolicySnapshot{}, s.ErrConflict
	}
	if r.Candidate.Version != s.ModelVersion || r.Candidate.Revision != 0 {
		return s.PolicySnapshot{}, s.ErrInvalid
	}
	candidate, err := readPolicy(r.Candidate.Document)
	if err != nil {
		return s.PolicySnapshot{}, err
	}
	if current, ok := m.policies[m.currentPolicy]; ok {
		old, err := readPolicy(current.Document)
		if err != nil {
			return s.PolicySnapshot{}, err
		}
		if reflect.DeepEqual(candidate, old) {
			return copyOf(current), nil
		}
	}
	if r.ExpectedRevision >= s.MaxPolicyRevision {
		return s.PolicySnapshot{}, s.ErrConflict
	}
	e := r.Event
	// Match the Badger mutation contract: PutPolicy cannot emit a delete event.
	if e == nil || e.Validate() != nil || e.Entity != "SystemConfig" || e.Action != "entity_put" || e.Revision != r.ExpectedRevision+1 {
		return s.PolicySnapshot{}, s.ErrInvalid
	}
	if _, ok := m.events[e.ID]; ok {
		return s.PolicySnapshot{}, s.ErrConflict
	}
	for _, event := range m.events {
		if event.OpID == e.OpID {
			return s.PolicySnapshot{}, s.ErrConflict
		}
	}
	if m.failBeforeCommit {
		return s.PolicySnapshot{}, s.ErrUnavailable
	}
	snapshot := copyOf(r.Candidate)
	snapshot.Revision = r.ExpectedRevision + 1
	m.policies[snapshot.Revision] = snapshot
	m.currentPolicy = snapshot.Revision
	m.events[e.ID] = *e
	if m.failAfterCommit {
		return s.PolicySnapshot{}, s.ErrUnavailable
	}
	return copyOf(snapshot), nil
}
