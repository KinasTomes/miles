package recovery_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// Reference schema deliberately accepts an ASCII/integer subset of JCS, not a
// competing production serializer. Unsupported Unicode/numbers fail closed.
type reference struct {
	Length int    `json:"length"`
	SHA    string `json:"sha256"`
}
type row struct {
	Deleted  bool   `json:"deleted"`
	Key      string `json:"key"`
	Revision uint64 `json:"revision"`
	Value    string `json:"value"`
}
type eventPage struct {
	Effects []row  `json:"effects"`
	Epoch   uint64 `json:"epoch"`
	Event   string `json:"event"`
	History string `json:"history"`
	Number  int    `json:"number"`
	Version int    `json:"r2_version"`
}
type eventDescriptor struct {
	Action   string      `json:"action"`
	Entity   string      `json:"entity"`
	Epoch    uint64      `json:"epoch"`
	History  string      `json:"history"`
	ID       string      `json:"id"`
	Logical  string      `json:"logical_op"`
	Mutation string      `json:"mutation"`
	Pages    []reference `json:"pages"`
	Version  int         `json:"r2_version"`
}
type member struct {
	Descriptor reference `json:"descriptor"`
	ID         string    `json:"id"`
}
type memberPage struct {
	Epoch   uint64   `json:"epoch"`
	Events  []member `json:"events"`
	History string   `json:"history"`
	Version int      `json:"r2_version"`
}
type epochSeal struct {
	Epoch      uint64    `json:"epoch"`
	History    string    `json:"history"`
	Membership reference `json:"membership"`
	Previous   string    `json:"previous"`
	Version    int       `json:"r2_version"`
}
type checkpointPage struct {
	History    string    `json:"history"`
	Projection string    `json:"projection"`
	Version    int       `json:"r2_version"`
	Seal       reference `json:"seal"`
	State      []row     `json:"state"`
	Target     uint64    `json:"target_epoch"`
}
type journal struct {
	History    string
	Objects    map[string]string
	Seals      []reference
	Checkpoint reference
	Expected   []row
}

func journalFixture(t *testing.T) journal {
	t.Helper()
	b, e := os.ReadFile("../../tests/fixtures/recovery/journal.json")
	if e != nil {
		t.Fatal(e)
	}
	var j journal
	if e = json.Unmarshal(b, &j); e != nil {
		t.Fatal(e)
	}
	return j
}
func subset(v any, depth int) bool {
	if depth > 8 {
		return false
	}
	switch x := v.(type) {
	case string:
		for _, r := range x {
			if r < 32 || r > 126 || strings.ContainsRune("<>&", r) {
				return false
			}
		}
		return true
	case bool:
		return true
	case json.Number:
		n, e := strconv.ParseUint(string(x), 10, 53)
		return e == nil && strconv.FormatUint(n, 10) == string(x)
	case []any:
		if len(x) > 256 {
			return false
		}
		for _, v := range x {
			if !subset(v, depth+1) {
				return false
			}
		}
		return true
	case map[string]any:
		if len(x) > 256 {
			return false
		}
		for k, v := range x {
			if !subset(k, depth+1) || !subset(v, depth+1) {
				return false
			}
		}
		return true
	default:
		return false
	}
}
func canonical(body string) error {
	if len(body) == 0 || len(body) > 256<<10 || !utf8.ValidString(body) {
		return invalid
	}
	d := json.NewDecoder(strings.NewReader(body))
	d.UseNumber()
	var tree any
	if d.Decode(&tree) != nil || !subset(tree, 0) {
		return invalid
	}
	if d.Decode(new(any)) != io.EOF {
		return invalid
	}
	b, e := json.Marshal(tree)
	if e != nil || string(b) != body {
		return invalid
	}
	return nil
}
func (j journal) read(r reference, out any) error {
	b, ok := j.Objects[r.SHA]
	if !ok || r.Length != len(b) || digest(b) != r.SHA {
		return invalid
	}
	if e := canonical(b); e != nil {
		return e
	}
	d := json.NewDecoder(strings.NewReader(b))
	d.DisallowUnknownFields()
	if e := d.Decode(out); e != nil {
		return e
	}
	// Round-trip the exact schema to detect missing required zero-valued fields,
	// unknown keys, wrong casing and explicit null; all candidate fields required.
	encoded, e := json.Marshal(out)
	if e != nil {
		return e
	}
	var a, z any
	dec := json.NewDecoder(bytes.NewReader(encoded))
	dec.UseNumber()
	if dec.Decode(&a) != nil {
		return invalid
	}
	dec = json.NewDecoder(strings.NewReader(b))
	dec.UseNumber()
	if dec.Decode(&z) != nil || !reflect.DeepEqual(a, z) {
		return invalid
	}
	return nil
}
func hexID(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func eventIdentity(d eventDescriptor) string {
	b, _ := json.Marshal([]string{"miles-r2-event-v1", d.History, d.Mutation, d.Entity, d.Action})
	return digest(string(b))
}

func (j journal) bundle(m member, epoch uint64) (eventDescriptor, []row, error) {
	var d eventDescriptor
	if e := j.read(m.Descriptor, &d); e != nil {
		return d, nil, e
	}
	if d.Version != 1 || d.History != j.History || !hexID(d.History, 32) || !hexID(d.Mutation, 32) || d.Entity != "Objects" || d.Action != "object_commit" || d.Logical == "" || d.Epoch != epoch || d.ID != m.ID || d.ID != eventIdentity(d) || len(d.Pages) == 0 || len(d.Pages) > 256 {
		return d, nil, invalid
	}
	rows := []row{}
	seen := map[string]bool{}
	for i, r := range d.Pages {
		var p eventPage
		if e := j.read(r, &p); e != nil {
			return d, nil, e
		}
		if p.Version != 1 || p.Number != i || p.History != j.History || p.Event != d.ID || p.Epoch != epoch || len(p.Effects) == 0 {
			return d, nil, invalid
		}
		for _, x := range p.Effects {
			if x.Key == "" || x.Revision == 0 || x.Revision > 1<<53-1 || seen[x.Key] || x.Deleted && x.Value != "" {
				return d, nil, invalid
			}
			seen[x.Key] = true
			rows = append(rows, x)
		}
	}
	return d, rows, nil
}

type projection struct {
	rows    map[string]row
	applied map[string]string
}

func emptyProjection() projection { return projection{map[string]row{}, map[string]string{}} }

// Atomic bundle application: validate ALL effects before publishing any. This
// reference uses contiguous fixture revisions; production revision domains must
// use their reviewed inventories rather than assuming all domains contiguous.
func (p projection) apply(id, hash string, rows []row) error {
	if old, ok := p.applied[id]; ok {
		if old != hash {
			return invalid
		}
		return nil
	}
	seen := map[string]bool{}
	for _, r := range rows {
		if r.Key == "" || seen[r.Key] || r.Revision != p.rows[r.Key].Revision+1 {
			return invalid
		}
		seen[r.Key] = true
	}
	for _, r := range rows {
		p.rows[r.Key] = r
	}
	p.applied[id] = hash
	return nil
}
func ordered(p projection) []row {
	keys := []string{}
	for k := range p.rows {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := []row{}
	for _, k := range keys {
		out = append(out, p.rows[k])
	}
	return out
}
func (j journal) rebuild() (projection, error) {
	p := emptyProjection()
	previous := "genesis"
	if len(j.Seals) == 0 || len(j.Seals) > 256 {
		return projection{}, invalid
	}
	for i, r := range j.Seals {
		var s epochSeal
		if e := j.read(r, &s); e != nil {
			return projection{}, e
		}
		if s.Version != 1 || s.History != j.History || s.Epoch != uint64(i) || s.Previous != previous {
			return projection{}, invalid
		}
		var m memberPage
		if e := j.read(s.Membership, &m); e != nil {
			return projection{}, e
		}
		if m.Version != 1 || m.History != j.History || m.Epoch != s.Epoch {
			return projection{}, invalid
		}
		last := ""
		for _, event := range m.Events {
			if event.ID <= last {
				return projection{}, invalid
			}
			last = event.ID
			d, rows, e := j.bundle(event, s.Epoch)
			if e != nil {
				return projection{}, e
			}
			if e = p.apply(d.ID, event.Descriptor.SHA, rows); e != nil {
				return projection{}, e
			}
		}
		previous = r.SHA
	}
	var cp checkpointPage
	if e := j.read(j.Checkpoint, &cp); e != nil {
		return projection{}, e
	}
	if cp.Version != 1 || cp.History != j.History || cp.Projection != "historical" || cp.Target != uint64(len(j.Seals)-1) || cp.Seal != j.Seals[len(j.Seals)-1] || !reflect.DeepEqual(cp.State, ordered(p)) {
		return projection{}, invalid
	}
	return p, nil
}

func TestJournalGoldenMultiPageChainCheckpoint(t *testing.T) {
	j := journalFixture(t)
	p, e := j.rebuild()
	if e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(ordered(p), j.Expected) {
		t.Fatal(ordered(p))
	}
	if len(p.applied) != 2 {
		t.Fatal(p.applied)
	}
}
func TestJournalEveryMissingOrCorruptArtifact(t *testing.T) {
	j := journalFixture(t)
	for hash := range j.Objects {
		for _, mode := range []string{"missing", "truncated", "tampered"} {
			t.Run(hash[:8]+"/"+mode, func(t *testing.T) {
				x := journalFixture(t)
				switch mode {
				case "missing":
					delete(x.Objects, hash)
				case "truncated":
					x.Objects[hash] = x.Objects[hash][:len(x.Objects[hash])-1]
				case "tampered":
					x.Objects[hash] += " "
				}
				p, e := x.rebuild()
				if e == nil || p.rows != nil {
					t.Fatal("accepted/partial", e)
				}
			})
		}
	}
}
func TestJournalDuplicateConflictAndAtomicApply(t *testing.T) {
	p := emptyProjection()
	r := []row{{Key: "a", Revision: 1, Value: "x"}, {Key: "b", Revision: 1, Value: "y"}}
	if e := p.apply("id", "digest", r); e != nil {
		t.Fatal(e)
	}
	if e := p.apply("id", "digest", r); e != nil {
		t.Fatal(e)
	}
	before := ordered(p)
	if p.apply("id", "other", r) == nil {
		t.Fatal("conflicting duplicate accepted")
	}
	if p.apply("next", "digest2", []row{{Key: "a", Revision: 2}, {Key: "b", Revision: 3}}) == nil {
		t.Fatal("out of order accepted")
	}
	if !reflect.DeepEqual(before, ordered(p)) || len(p.applied) != 1 {
		t.Fatal("partial bundle")
	}
}
func TestJournalStrictSubset(t *testing.T) {
	for _, b := range []string{`{"a":1,"a":2}`, `{"a":null}`, `{"a":1.0}`, `{"a":9007199254740992}`, `{"a":-1}`, `{"a":1} {}`, `{"b":1,"a":2}`, `{"a":"\ud800"}`, `{"a":"é"}`, strings.Repeat(" ", 256<<10+1)} {
		t.Run(fmt.Sprint(len(b), b[:min(25, len(b))]), func(t *testing.T) {
			if canonical(b) == nil {
				t.Fatal("accepted", b)
			}
		})
	}
}
