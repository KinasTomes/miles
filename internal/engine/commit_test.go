package engine

// Reference model of docs/contracts/chunking-and-commit.md §6.1, §6.4 and §6.6:
// commit transaction conditions and effects, outcome reconciliation by op_id,
// cleanup per operation, ClientRequestToken encoding and refcount batches.
// The DynamoDB/Badger adapters and the refcount worker (W04-HAI-01,
// W02-HAI-03, W05-HAI-01) must reproduce commit.json scenarios, tokens and
// refcount; their tests then replace this model.

import (
	"cmp"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"testing"
)

var opIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// requestToken is op_id followed by the attempt as 4 lowercase hex digits: always 36 characters.
func requestToken(opID string, attempt int) (string, bool) {
	if !opIDPattern.MatchString(opID) || attempt < 0 || attempt > 0xffff {
		return "", false
	}
	return fmt.Sprintf("%s%04x", opID, attempt), true
}

type headState struct {
	Revision, Size int64
	Op             string
	Deleted        bool
}
type ownerState struct {
	Limit, Used, Reserved, Staged int64
	ActiveUploads                 int64 `json:"active_uploads"`
}
type ledgerState struct {
	State          string
	Revision, R, S int64
}
type world struct {
	Head    headState
	Owner   ownerState
	Ledgers map[string]*ledgerState
	Upload  string
	Events  map[string]int64 // op -> committed head revision
}
type commitOp struct {
	Operation, Caller string
	Size              int64
	Takeover          bool
}

// txParams is everything a transaction attempt conditions on; a new set needs a new token.
type txParams struct{ Expected, Base, Ledger int64 }
type opWriter struct {
	params        txParams
	attempt, sent int
}
type sim struct {
	world
	ops         map[string]commitOp
	writers     map[string]*opWriter
	tokens      map[string]txParams // tokens of applied transactions inside the idempotency window
	revoked     map[string]bool
	fenced      map[string]bool
	unavailable bool
}

func (m *sim) token(name string) string { return fmt.Sprintf("%s/%d", name, m.writers[name].attempt) }

func (m *sim) read(name string) txParams {
	return txParams{m.Head.Revision, m.Head.Size, m.Ledgers[name].Revision} // a tombstone has size 0.
}

// denied re-reads authorization in the fixed priority of §6.4.
func (m *sim) denied(o commitOp) string {
	switch {
	case m.revoked["credential"] || m.revoked["user"]:
		return "AccessDenied"
	case m.revoked["bucket"]:
		return "NoSuchBucket"
	case m.revoked["grant"] && o.Caller != "owner":
		return "AccessDenied"
	}
	return ""
}

func (m *sim) quotaHolds(name string, p txParams) bool {
	l, q := m.Ledgers[name], m.Owner
	return q.Used+m.ops[name].Size-p.Base+q.Reserved-l.R <= q.Limit && q.Reserved >= l.R && q.Staged >= l.S
}

// holds evaluates every condition of the §6.3 transaction.
func (m *sim) holds(name string, p txParams) bool {
	o, l := m.ops[name], m.Ledgers[name]
	_, done := m.Events[name]
	return m.Head.Revision == p.Expected && m.quotaHolds(name, p) &&
		l.State == "Active" && l.Revision == p.Ledger && !m.fenced[name] && !done &&
		m.denied(o) == "" && (o.Operation != "complete" || m.Upload == "Completing")
}

func (m *sim) apply(name string, p txParams) {
	o, l := m.ops[name], m.Ledgers[name]
	m.Head = headState{p.Expected + 1, o.Size, name, o.Operation == "delete"}
	m.Owner.Used += o.Size - p.Base
	m.Owner.Reserved -= l.R
	m.Owner.Staged -= l.S
	if o.Operation == "complete" {
		m.Owner.ActiveUploads--
		m.Upload = "Completed"
	}
	l.State, l.Revision = "Committed", l.Revision+1
	m.Events[name] = p.Expected + 1
}

// execute is one TransactWriteItems call.
func (m *sim) execute(name string, loseAck bool) string {
	w := m.writers[name]
	w.sent++
	if m.unavailable {
		return "unknown"
	}
	if p, ok := m.tokens[m.token(name)]; ok {
		if p == w.params {
			return "success" // DynamoDB returns success without applying again.
		}
		return "mismatch"
	}
	if !m.holds(name, w.params) {
		return "canceled"
	}
	m.apply(name, w.params)
	m.tokens[m.token(name)] = w.params
	if loseAck {
		return "unknown"
	}
	return "success"
}

// fail applies the cleanup table of §6.4 to an attempt proved uncommitted.
func (m *sim) fail(name, code string) string {
	o, l := m.ops[name], m.Ledgers[name]
	if o.Operation == "complete" && (code == "QuotaExceeded" || code == "AccessDenied" && !o.Takeover) {
		m.Upload = "Active" // reopened under the completion fence; reservation and counters stay.
		return code
	}
	if o.Operation == "complete" {
		m.Upload = "Aborting"
		m.Owner.ActiveUploads--
	}
	m.Owner.Reserved -= l.R
	m.Owner.Staged -= l.S
	l.State, l.Revision = "Released", l.Revision+1
	return code
}

type commitStep struct {
	Commit, Retry string
	LoseAck       bool   `json:"lose_ack"`
	TokenExpired  bool   `json:"token_expired"`
	Expected      *int64 // stale expected revision for the first attempt
	Set           *struct {
		Limit       *int64
		Revoke      string
		FenceLost   string `json:"fence_lost"`
		Unavailable bool
	}
	Expect *struct {
		Result   string
		Revision int64
		Attempts int
		Upload   *string
		Owner    *ownerState
		Ledgers  map[string]*ledgerState
	}
}

// commit runs the §6.4 writer loop and returns the outcome and the committed head revision.
func (m *sim) commit(name string, s commitStep) (string, int64) {
	o, w := m.ops[name], m.writers[name]
	if s.Commit != "" {
		if o.Operation == "complete" && m.Upload == "Active" {
			m.Upload = "Completing" // client retry freezes again with the same op_id.
		}
		if w.sent > 0 {
			w.attempt++
		}
		w.params = m.read(name)
		if s.Expected != nil {
			w.params.Expected, w.params.Base = *s.Expected, 0
		}
	}
	for {
		if s.TokenExpired {
			delete(m.tokens, m.token(name))
		}
		switch m.execute(name, s.LoseAck) {
		case "success":
			return "committed", w.params.Expected + 1
		case "unknown":
			if !m.unavailable {
				return "unknown", 0 // keep token and parameters, retry later.
			}
		}
		// Reconcile by op_id before using any cancellation reason.
		if m.unavailable {
			return "ServiceUnavailable", 0
		}
		if revision, ok := m.Events[name]; ok {
			return "already_committed", revision
		}
		l := m.Ledgers[name]
		if l.State != "Active" || l.Revision != w.params.Ledger || m.fenced[name] || o.Operation == "complete" && m.Upload != "Completing" {
			return "ServiceUnavailable", 0 // ownership lost: no publish, no release.
		}
		if code := m.denied(o); code != "" {
			return m.fail(name, code), 0
		}
		if m.Head.Revision != w.params.Expected || m.quotaHolds(name, w.params) {
			w.attempt++ // head moved or transient conflict: new parameter set, new token.
			w.params = m.read(name)
			continue
		}
		return m.fail(name, "QuotaExceeded"), 0
	}
}

func (w world) String() string {
	b, _ := json.Marshal(w)
	return string(b)
}

func TestCommitScenarios(t *testing.T) {
	var f struct {
		Scenarios []struct {
			Name         string
			State, Final world
			Ops          map[string]commitOp
			Steps        []commitStep
		}
		Tokens []struct {
			OpID    string `json:"op_id"`
			Attempt int
			Token   *string
		}
	}
	load(t, "commit.json", &f)
	for _, sc := range f.Scenarios {
		t.Run(sc.Name, func(t *testing.T) {
			m := &sim{world: sc.State, ops: sc.Ops, writers: map[string]*opWriter{}, tokens: map[string]txParams{}, revoked: map[string]bool{}, fenced: map[string]bool{}}
			for name := range sc.Ops {
				m.writers[name] = &opWriter{}
			}
			for i, s := range sc.Steps {
				if s.Set != nil {
					if s.Set.Limit != nil {
						m.Owner.Limit = *s.Set.Limit
					}
					m.revoked[s.Set.Revoke] = s.Set.Revoke != ""
					m.fenced[s.Set.FenceLost] = s.Set.FenceLost != ""
					m.unavailable = m.unavailable || s.Set.Unavailable
					continue
				}
				name := s.Commit + s.Retry
				result, revision := m.commit(name, s)
				e := s.Expect
				if result != e.Result || revision != e.Revision || m.writers[name].sent != e.Attempts {
					t.Errorf("step %d: %s revision %d attempts %d, want %s %d %d", i, result, revision, m.writers[name].sent, e.Result, e.Revision, e.Attempts)
				}
				if e.Upload != nil && m.Upload != *e.Upload || e.Owner != nil && m.Owner != *e.Owner {
					t.Errorf("step %d: state %v", i, m.world)
				}
				for k, v := range e.Ledgers {
					if *m.Ledgers[k] != *v {
						t.Errorf("step %d: ledger %s = %+v, want %+v", i, k, *m.Ledgers[k], *v)
					}
				}
			}
			if !reflect.DeepEqual(m.world, sc.Final) {
				t.Errorf("final\n got %v\nwant %v", m.world, sc.Final)
			}
		})
	}
	for _, v := range f.Tokens {
		got, ok := requestToken(v.OpID, v.Attempt)
		if ok != (v.Token != nil) || ok && (got != *v.Token || len(got) != 36) {
			t.Errorf("token(%s, %d) = %q %v", v.OpID, v.Attempt, got, ok)
		}
	}
}

// refcountBatch is the §6.6 batch size: 99 Chunks updates plus the Outbox progress update.
const refcountBatch = 99

type keyFill struct {
	Prefix   string
	Count    int
	Refcount int64
}

func (f *keyFill) keys() []string {
	var out []string
	for i := 0; f != nil && i < f.Count; i++ {
		out = append(out, fmt.Sprintf("%s%03d", f.Prefix, i))
	}
	return out
}

type refEvent struct {
	Add, Remove map[string]int64
	Fill        *keyFill // +1 for each generated key
}
type keyDelta struct {
	Key   string
	Delta int64
}

// refBatches nets occurrences per key, drops zero deltas, sorts keys and cuts batches.
func refBatches(e refEvent) [][]keyDelta {
	d := map[string]int64{}
	for k, n := range e.Add {
		d[k] += n
	}
	for _, k := range e.Fill.keys() {
		d[k]++
	}
	for k, n := range e.Remove {
		d[k] -= n
	}
	var all []keyDelta
	for k, n := range d {
		if n != 0 {
			all = append(all, keyDelta{k, n})
		}
	}
	slices.SortFunc(all, func(a, b keyDelta) int { return cmp.Compare(a.Key, b.Key) })
	var out [][]keyDelta
	for len(all) > 0 {
		n := min(refcountBatch, len(all))
		out = append(out, all[:n])
		all = all[n:]
	}
	return out
}

func TestRefcountBatches(t *testing.T) {
	var f struct {
		Refcount []struct {
			Name   string
			Chunks map[string]int64
			Fill   *keyFill
			Events map[string]refEvent
			Steps  []struct {
				Apply, Resend, Cleanup string
				CrashAfter             *int `json:"crash_after"`
				Batch                  int
			}
			Expect struct {
				Chunks            map[string]int64
				Fill              *keyFill
				Progress, Batches map[string]int
			}
		}
	}
	load(t, "commit.json", &f)
	for _, c := range f.Refcount {
		t.Run(c.Name, func(t *testing.T) {
			// progress holds refcount_next of every Outbox item that exists; the commit wrote 0.
			chunks, progress, batches := map[string]int64{}, map[string]int{}, map[string][][]keyDelta{}
			for k, v := range c.Chunks {
				chunks[k] = v
			}
			for _, k := range c.Fill.keys() {
				chunks[k] = c.Fill.Refcount
			}
			for op, e := range c.Events {
				batches[op] = refBatches(e)
				progress[op] = 0
			}
			// exec is one transaction: Outbox attribute_exists(event_id) and refcount_next = b → b+1,
			// plus every Chunks update, or nothing. A missing item never matches and is never recreated.
			exec := func(op string, b int) bool {
				if next, ok := progress[op]; !ok || next != b {
					return false
				}
				for _, kd := range batches[op][b] {
					if _, ok := chunks[kd.Key]; !ok {
						t.Fatalf("Chunks row %s missing: invariant violated", kd.Key)
					}
				}
				for _, kd := range batches[op][b] {
					chunks[kd.Key] += kd.Delta
				}
				progress[op] = b + 1
				return true
			}
			// run reads refcount_next consistent; a missing item means the event was finished and cleaned up.
			run := func(op string, crashAfter *int) {
				for {
					b, ok := progress[op]
					if !ok || b == len(batches[op]) {
						return
					}
					if exec(op, b) && crashAfter != nil && b == *crashAfter {
						return
					}
				}
			}
			for _, s := range c.Steps {
				switch {
				case s.Cleanup != "":
					// Delete Outbox condition refcount_next = number of batches.
					if progress[s.Cleanup] == len(batches[s.Cleanup]) {
						delete(progress, s.Cleanup)
					}
				case s.Resend != "":
					if !exec(s.Resend, s.Batch) {
						run(s.Resend, nil)
					}
				default:
					run(s.Apply, s.CrashAfter)
				}
			}
			want := map[string]int64{}
			for k, v := range c.Expect.Chunks {
				want[k] = v
			}
			for _, k := range c.Expect.Fill.keys() {
				want[k] = c.Expect.Fill.Refcount
			}
			counts := map[string]int{}
			for op, b := range batches {
				counts[op] = len(b)
			}
			if !reflect.DeepEqual(chunks, want) || !reflect.DeepEqual(progress, c.Expect.Progress) || !reflect.DeepEqual(counts, c.Expect.Batches) {
				t.Errorf("chunks %v progress %v batches %v", chunks, progress, counts)
			}
		})
	}
}
