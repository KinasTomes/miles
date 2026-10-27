// These are reference golden tests for a PROPOSED single-bundle subset, not a
// production R2 serializer/recovery engine. Full field schemas require review.
package recovery_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"
)

type artifact struct {
	Body   string
	Length int
	SHA256 string
}
type candidate struct{ Page, Descriptor, Membership, Seal, Checkpoint artifact }
type effect struct {
	Key      string
	Revision uint64
	Value    string
}
type page struct {
	Effects           []effect
	Epoch             uint64
	History, Mutation string
	Page              uint64
	Version           uint64 `json:"r2_version"`
}
type descriptor struct {
	History, Mutation string
	Pages             []struct {
		Length int
		SHA256 string
	}
	Version uint64 `json:"r2_version"`
}
type membership struct {
	History string
	Epoch   uint64
	Events  []struct{ Descriptor, Mutation string }
	Version uint64 `json:"r2_version"`
}
type seal struct {
	History, Membership, Previous string
	Epoch                         uint64
	Version                       uint64 `json:"r2_version"`
}
type checkpoint struct {
	History, Projection string
	State               []effect
	Terminal            string `json:"terminal_seal"`
	Version             uint64 `json:"r2_version"`
}

var invalid = errors.New("invalid candidate chain")

func digest(body string) string { h := sha256.Sum256([]byte(body)); return hex.EncodeToString(h[:]) }
func verify(a artifact, v any) error {
	if len(a.Body) > 256<<10 || len(a.Body) != a.Length || digest(a.Body) != a.SHA256 {
		return invalid
	}
	return json.Unmarshal([]byte(a.Body), v)
}
func load(t *testing.T) candidate {
	t.Helper()
	b, e := os.ReadFile("../../tests/fixtures/recovery/candidate.json")
	if e != nil {
		t.Fatal(e)
	}
	var c candidate
	if e = json.Unmarshal(b, &c); e != nil {
		t.Fatal(e)
	}
	return c
}

// Verify the full fixture chain BEFORE returning a staging projection. No
// partial bundle state becomes visible. Only this fixture's exact inventory is
// supported: this is not an unknown-field/JCS production decoder.
func replay(c candidate, prior string) (map[string]effect, error) {
	var p page
	var d descriptor
	var m membership
	var s seal
	var cp checkpoint
	for _, x := range []struct {
		a artifact
		v any
	}{{c.Page, &p}, {c.Descriptor, &d}, {c.Membership, &m}, {c.Seal, &s}, {c.Checkpoint, &cp}} {
		if e := verify(x.a, x.v); e != nil {
			return nil, e
		}
	}
	if p.Version != 1 || d.Version != 1 || m.Version != 1 || s.Version != 1 || cp.Version != 1 {
		return nil, invalid
	}
	if p.History == "" || p.Mutation == "" || p.History != d.History || p.History != m.History || p.History != s.History || p.History != cp.History || p.Mutation != d.Mutation || p.Page != 0 || p.Epoch != m.Epoch || p.Epoch != s.Epoch {
		return nil, invalid
	}
	if len(d.Pages) != 1 || d.Pages[0].SHA256 != c.Page.SHA256 || d.Pages[0].Length != c.Page.Length || len(m.Events) != 1 || m.Events[0].Mutation != p.Mutation || m.Events[0].Descriptor != c.Descriptor.SHA256 {
		return nil, invalid
	}
	if s.Membership != c.Membership.SHA256 || s.Previous != prior || cp.Terminal != c.Seal.SHA256 || cp.Projection != "historical" || !reflect.DeepEqual(cp.State, p.Effects) {
		return nil, invalid
	}
	state := map[string]effect{}
	for _, e := range p.Effects {
		if e.Key == "" || e.Revision != 1 {
			return nil, invalid
		}
		if _, ok := state[e.Key]; ok {
			return nil, invalid
		}
		state[e.Key] = e
	}
	return state, nil
}

func TestCandidateGoldenChainAndReplay(t *testing.T) {
	c := load(t)
	got, e := replay(c, "genesis")
	if e != nil {
		t.Fatal(e)
	}
	want := map[string]effect{"object/bucket-1/k": {Key: "object/bucket-1/k", Revision: 1, Value: "manifest-1"}, "quota/owner": {Key: "quota/owner", Revision: 1, Value: "4"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
	// Repeated verified reconstruction yields the same projection, not deltas.
	again, e := replay(c, "genesis")
	if e != nil || !reflect.DeepEqual(got, again) {
		t.Fatal(again, e)
	}
}
func TestCandidateNegativeChain(t *testing.T) {
	for _, name := range []string{"missing page", "truncated page", "bad checksum", "missing membership", "descriptor mismatch", "chain gap", "checkpoint mismatch"} {
		t.Run(name, func(t *testing.T) {
			c := load(t)
			prior := "genesis"
			switch name {
			case "missing page":
				c.Page = artifact{}
			case "truncated page":
				c.Page.Body = c.Page.Body[:len(c.Page.Body)-1]
			case "bad checksum":
				c.Page.SHA256 = "bad"
			case "missing membership":
				c.Membership = artifact{}
			case "descriptor mismatch":
				c.Descriptor = c.Page
			case "chain gap":
				prior = "other-seal"
			case "checkpoint mismatch":
				c.Checkpoint = c.Page
			}
			state, e := replay(c, prior)
			if e == nil || state != nil {
				t.Fatal("partial/accepted replay", state, e)
			}
		})
	}
}
