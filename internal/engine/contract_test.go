package engine

// tests/fixtures/engine pins FastCDC v1, chunk identity, codec choice, dedup
// lookup and commit transactions of docs/contracts/chunking-and-commit.md.
// Chunking, compression and lookup vectors exercise production code. Commit
// transactions retain their W1 reference until the backend work in W02/W04.

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"os"
	"testing"

	"github.com/KinasTomes/miles/internal/storage"
)

type params struct {
	Min int `json:"min_bytes"`
	Avg int `json:"avg_bytes"`
	Max int `json:"max_bytes"`
}

type action struct{ Table, Item, Op string }

const helloKey = "default#2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"

// transaction lists the TransactWriteItems actions for the fixture identities.
// A ConditionCheck on an item that is also written is merged into that write.
func transaction(operation, caller string) []action {
	if operation == "chunk_insert" || operation == "chunk_cas" {
		op := "Put"
		if operation == "chunk_cas" {
			op = "Update"
		}
		return []action{{"Chunks", helloKey, op}, {"Packs", "pack-a/gen-1", "ConditionCheck"}, {"Leases", "lease-1", "ConditionCheck"}, {"Outbox", "op-1", "Put"}}
	}
	const owner = "owner"
	a := []action{{"Objects", "demo/file", "Put"}, {"Users", owner, "Update"}, {"QuotaReservations", "r-1", "Update"}, {"Outbox", "op-1", "Put"}, {"Credentials", caller + "-key", "ConditionCheck"}}
	if caller != owner {
		a = append(a, action{"Users", caller, "ConditionCheck"})
	}
	a = append(a, action{"Buckets", "demo", "ConditionCheck"})
	if caller != owner {
		a = append(a, action{"BucketGrants", "bucket-1/" + caller, "ConditionCheck"})
	}
	a = append(a, action{"Leases", "lease-1", "ConditionCheck"})
	if operation == "complete" {
		a = append(a, action{"Uploads", "upload-1", "Update"})
	}
	return a
}

func load(t *testing.T, name string, v any) {
	t.Helper()
	b, err := os.ReadFile("../../tests/fixtures/engine/" + name)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatal(err)
	}
}

func rat(t *testing.T, n json.Number) *big.Rat {
	t.Helper()
	r, ok := new(big.Rat).SetString(n.String())
	if !ok {
		t.Fatalf("decimal %q", n)
	}
	return r
}

// stream returns the first n bytes of SHA-256(seed || u64be(0)) || SHA-256(seed || u64be(1)) || ...
func stream(seed string, n int) []byte {
	out := make([]byte, 0, n+sha256.Size)
	buf := append([]byte(seed), make([]byte, 8)...)
	for k := uint64(0); len(out) < n; k++ {
		binary.BigEndian.PutUint64(buf[len(seed):], k)
		h := sha256.Sum256(buf)
		out = append(out, h[:]...)
	}
	return out[:n]
}

type inputSpec struct {
	Hex    *string
	Zeros  int
	Seed   string
	Length int
	Repeat int
	Insert *struct {
		Offset int
		Hex    string
	}
	Delete *struct{ Offset, Length int }
}

func (in inputSpec) bytes(t *testing.T) []byte {
	t.Helper()
	switch {
	case in.Hex != nil:
		b, err := hex.DecodeString(*in.Hex)
		if err != nil {
			t.Fatal(err)
		}
		return b
	case in.Zeros > 0:
		return make([]byte, in.Zeros)
	}
	b := stream(in.Seed, in.Length)
	for i := 1; i < in.Repeat; i++ {
		b = append(b, b[:in.Length]...)
	}
	if in.Insert != nil {
		x, err := hex.DecodeString(in.Insert.Hex)
		if err != nil {
			t.Fatal(err)
		}
		b = append(b[:in.Insert.Offset], append(x, b[in.Insert.Offset:]...)...)
	}
	if in.Delete != nil {
		b = append(b[:in.Delete.Offset], b[in.Delete.Offset+in.Delete.Length:]...)
	}
	return b
}

type chunkVector struct {
	Length int
	SHA256 string
}

func TestChunkingVectors(t *testing.T) {
	var f struct {
		Gear  struct{ SHA256, First, Last string }
		Masks []struct {
			Avg          int `json:"avg_bytes"`
			Small, Large string
		}
		Params map[string]params
		Cases  []struct {
			Name, Params string
			Input        inputSpec
			ChangedFrom  string `json:"changed_from"`
			Changed      int
			Chunks       []chunkVector
		}
		Identity []struct {
			Domain, Hex, Hash string
			PartitionKey      string `json:"partition_key"`
		}
		Policy []struct {
			Name string
			params
			Valid bool
		}
	}
	load(t, "chunking.json", &f)
	table := make([]byte, 0, 256*8)
	for _, g := range fastCDCGear {
		table = binary.BigEndian.AppendUint64(table, g)
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(table)); got != f.Gear.SHA256 || fmt.Sprintf("%016x", fastCDCGear[0]) != f.Gear.First || fmt.Sprintf("%016x", fastCDCGear[255]) != f.Gear.Last {
		t.Fatalf("gear table sha256 %s", got)
	}
	for _, m := range f.Masks {
		small, large := fastCDCMasks(m.Avg)
		if fmt.Sprintf("%016x", small) != m.Small || fmt.Sprintf("%016x", large) != m.Large {
			t.Errorf("masks(%d) = %016x %016x", m.Avg, small, large)
		}
	}
	byName := map[string][]chunkVector{}
	for _, c := range f.Cases {
		t.Run(c.Name, func(t *testing.T) {
			p, ok := f.Params[c.Params]
			actual := ChunkingParams{Min: p.Min, Avg: p.Avg, Max: p.Max}
			if !ok || actual.Validate() != nil {
				t.Fatalf("params %q", c.Params)
			}
			data := c.Input.bytes(t)
			chunker, err := NewChunker(bytes.NewReader(data), "default", actual)
			if err != nil {
				t.Fatal(err)
			}
			var got []chunkVector
			for {
				chunk, err := chunker.Next(t.Context())
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				got = append(got, chunkVector{len(chunk.Bytes), chunk.Key.Hash})
			}
			if len(got) != len(c.Chunks) {
				t.Fatalf("%d chunks, want %d", len(got), len(c.Chunks))
			}
			for i, want := range c.Chunks {
				if got[i] != want {
					t.Errorf("chunk %d = %+v, want %+v", i, got[i], want)
				}
				if want.Length > p.Max || i < len(c.Chunks)-1 && want.Length < p.Min {
					t.Errorf("chunk %d length %d outside [%d, %d]", i, want.Length, p.Min, p.Max)
				}
			}
		})
		byName[c.Name] = c.Chunks
	}
	for _, c := range f.Cases {
		if c.ChangedFrom == "" {
			continue
		}
		seen := map[string]bool{}
		for _, b := range byName[c.ChangedFrom] {
			seen[b.SHA256] = true
		}
		changed := 0
		for _, b := range c.Chunks {
			if !seen[b.SHA256] {
				changed++
			}
		}
		if changed != c.Changed {
			t.Errorf("%s: %d chunks changed, want %d", c.Name, changed, c.Changed)
		}
	}
	for _, id := range f.Identity {
		raw, err := hex.DecodeString(id.Hex)
		if err != nil {
			t.Fatal(err)
		}
		key, err := HashChunk(id.Domain, raw)
		if err != nil {
			t.Fatal(err)
		}
		pk, err := key.PartitionKey()
		if err != nil || key.Hash != id.Hash || pk != id.PartitionKey {
			t.Errorf("identity %s: pk %q err %v", id.Domain, pk, err)
		}
	}
	for _, c := range f.Policy {
		if got := (ChunkingParams{Min: c.Min, Avg: c.Avg, Max: c.Max}).Validate() == nil; got != c.Valid {
			t.Errorf("chunking policy %s: valid=%v", c.Name, got)
		}
	}
}

func TestCompressionVectors(t *testing.T) {
	var f struct {
		Decisions []struct {
			Name           string
			Enabled        bool
			MinSavingRatio json.Number `json:"min_saving_ratio"`
			RawLen         int64       `json:"raw_len"`
			StoredLen      *int64      `json:"stored_len"`
			Codec          string
		}
		Policy []struct {
			Name, Codec    string
			Level          int
			MinSavingRatio json.Number `json:"min_saving_ratio"`
			Valid          bool
		}
	}
	load(t, "compression.json", &f)
	for _, d := range f.Decisions {
		got, err := StoredCodec(d.Enabled, rat(t, d.MinSavingRatio), d.RawLen, d.StoredLen)
		if err != nil || got != d.Codec {
			t.Errorf("%s: codec %s, want %s", d.Name, got, d.Codec)
		}
	}
	for _, p := range f.Policy {
		policy := CompressionPolicy{Enabled: true, Codec: p.Codec, Level: p.Level, MinSavingRatio: rat(t, p.MinSavingRatio)}
		if got := policy.Validate() == nil; got != p.Valid {
			t.Errorf("compression policy %s: valid=%v", p.Name, got)
		}
	}
}

func TestCommitVectors(t *testing.T) {
	var f struct {
		Lookup []struct {
			Name, State, Class, Pin, Action string
			Found                           bool
		}
		Transactions []struct {
			Name, Operation, Caller string
			Actions                 []action
		}
	}
	load(t, "commit.json", &f)
	for _, l := range f.Lookup {
		if got := DecideLookup(l.Found, l.State, storage.StorageClass(l.Class), l.Pin != "rejected"); string(got) != l.Action {
			t.Errorf("lookup %s: %s, want %s", l.Name, got, l.Action)
		}
	}
	for _, tx := range f.Transactions {
		got := transaction(tx.Operation, tx.Caller)
		if fmt.Sprint(got) != fmt.Sprint(tx.Actions) {
			t.Errorf("%s:\n got %v\nwant %v", tx.Name, got, tx.Actions)
		}
		items, events := map[[2]string]bool{}, 0
		for _, a := range tx.Actions {
			k := [2]string{a.Table, a.Item}
			if items[k] {
				t.Errorf("%s: two actions on %v; DynamoDB rejects the transaction", tx.Name, k)
			}
			items[k] = true
			if a.Table == "Outbox" && a.Op == "Put" {
				events++
			}
		}
		if len(tx.Actions) > 100 || events != 1 {
			t.Errorf("%s: %d actions, %d outbox events", tx.Name, len(tx.Actions), events)
		}
	}
}
