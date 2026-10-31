package recovery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	badger "github.com/KinasTomes/miles/internal/metadata/badger"
	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/KinasTomes/miles/internal/storage/contracttest"
	"github.com/KinasTomes/miles/internal/storage/fake"
)

type memoryInputs map[string][]byte

type eventOverride struct {
	s.OutboxExportStore
	event s.OutboxEvent
}

func (m eventOverride) GetOutbox(context.Context, string) (s.OutboxEvent, error) { return m.event, nil }

func (m memoryInputs) Open(_ context.Context, k string) (io.ReadCloser, error) {
	b, ok := m[k]
	if !ok {
		return nil, s.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

type memoryObjects struct {
	objects        map[string][]byte
	calls, fail    int
	after, corrupt bool
}

var crash = errors.New("worker crash")

func (m *memoryObjects) Create(_ context.Context, k string, b []byte) error {
	m.calls++
	if m.calls == m.fail && !m.after {
		return crash
	}
	if old, ok := m.objects[k]; ok && !bytes.Equal(old, b) {
		return s.ErrConflict
	}
	m.objects[k] = bytes.Clone(b)
	if m.calls == m.fail && m.after {
		return crash
	}
	return nil
}
func (m *memoryObjects) Read(_ context.Context, k string) ([]byte, error) {
	b, ok := m.objects[k]
	if !ok {
		return nil, s.ErrNotFound
	}
	if m.corrupt {
		return []byte("corrupt"), nil
	}
	return bytes.Clone(b), nil
}

type metadataStore interface {
	s.MetadataStore
	s.OutboxExportStore
}

func producerFixture(t *testing.T, backend string) (metadataStore, contracttest.Fixture, func() metadataStore) {
	t.Helper()
	f := contracttest.NewFixture()
	if backend == "fake" {
		m := fake.NewMetadata(fake.Seed{Now: func() time.Time { return f.Now }, Buckets: []s.Bucket{f.Bucket}, Users: f.Users, Credentials: f.Credentials, Grants: []s.Grant{f.Grant}, Quotas: []s.LogicalQuota{f.Quota}, Packs: f.Packs, Writers: []s.WriterLease{f.Lease}})
		return m, f, func() metadataStore { return m }
	}
	path := t.TempDir()
	var m *badger.Store
	open := func() {
		var err error
		m, err = badger.Open(badger.Options{Path: path, Now: func() time.Time { return f.Now }})
		if err != nil {
			t.Fatal(err)
		}
	}
	open()
	if err := m.Bootstrap(context.Background(), badger.Bootstrap{Buckets: []s.Bucket{f.Bucket}, Users: f.Users, Credentials: f.Credentials, Grants: []s.Grant{f.Grant}, Quotas: []s.LogicalQuota{f.Quota}, Packs: f.Packs, Writers: []s.WriterLease{f.Lease}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := m.Close(); err != nil {
			t.Error(err)
		}
	})
	return m, f, func() metadataStore {
		if err := m.Close(); err != nil {
			t.Fatal(err)
		}
		open()
		return m
	}
}
func commitInput(t *testing.T, m metadataStore, f contracttest.Fixture, label string, revision uint64) (s.OutboxEvent, []byte) {
	t.Helper()
	r := f.Request(label, revision)
	input := Input{Pages: []EventPage{{Effects: []Effect{{Key: "object/file", Revision: revision + 1, Value: "manifest-" + label}}, Epoch: r.Event.Epoch, Event: r.Event.ID, History: r.Event.HistoryID, Number: 0, Version: 1}, {Effects: []Effect{{Key: "quota/owner", Revision: revision + 1, Value: "4"}}, Epoch: r.Event.Epoch, Event: r.Event.ID, History: r.Event.HistoryID, Number: 1, Version: 1}}}
	body, err := Encode(input)
	if err != nil {
		t.Fatal(err)
	}
	r.Event.PayloadSHA256 = digest(body)
	contracttest.Prepare(t, m, f, r, max(0, 4-int64(revision)*4))
	if _, err = m.CommitObject(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	e, err := m.GetOutbox(context.Background(), r.Event.ID)
	if err != nil {
		t.Fatal(err)
	}
	return e, body
}

func TestExportEffectSemantics(t *testing.T) {
	for _, backend := range []string{"fake", "badger"} {
		for _, scenario := range []string{"duplicate-within-page", "duplicate-across-pages", "tombstone-with-value", "valid-multipage", "valid-tombstone"} {
			t.Run(backend+"/"+scenario, func(t *testing.T) {
				ctx := context.Background()
				m, f, _ := producerFixture(t, backend)
				r := f.Request("effect-semantics", 0)
				first := Effect{Key: "object/file", Revision: 1, Value: "manifest"}
				input := Input{Pages: []EventPage{
					{Effects: []Effect{first}, Epoch: r.Event.Epoch, Event: r.Event.ID, History: r.Event.HistoryID, Number: 0, Version: 1},
					{Effects: []Effect{{Key: "quota/owner", Revision: 1, Value: "4"}}, Epoch: r.Event.Epoch, Event: r.Event.ID, History: r.Event.HistoryID, Number: 1, Version: 1},
				}}
				switch scenario {
				case "duplicate-within-page":
					input.Pages[1].Effects = append(input.Pages[1].Effects, input.Pages[1].Effects[0])
				case "duplicate-across-pages":
					input.Pages[1].Effects[0] = first
				case "tombstone-with-value":
					input.Pages[1].Effects[0].Deleted = true
				case "valid-tombstone":
					input.Pages[1].Effects[0].Deleted = true
					input.Pages[1].Effects[0].Value = ""
				}
				body, err := Encode(input)
				if err != nil {
					t.Fatal(err)
				}
				r.Event.PayloadSHA256 = digest(body)
				contracttest.Prepare(t, m, f, r, 4)
				if _, err = m.CommitObject(ctx, r); err != nil {
					t.Fatal(err)
				}
				event, err := m.GetOutbox(ctx, r.Event.ID)
				if err != nil || event.PayloadSHA256 != digest(body) {
					t.Fatal("committed input digest", err)
				}
				sink := &memoryObjects{objects: map[string][]byte{}}
				w := Exporter{Metadata: m, Payloads: memoryInputs{event.PayloadPointer: body}, Objects: sink}
				err = w.Export(ctx, event.ID)
				valid := strings.HasPrefix(scenario, "valid-")
				if valid {
					if err != nil {
						t.Fatal(err)
					}
					if sink.calls != 3 || len(sink.objects) != 3 {
						t.Fatal("missing valid artifacts")
					}
					if _, err = m.GetOutboxExport(ctx, event.ID); err != nil {
						t.Fatal("missing valid export progress", err)
					}
					return
				}
				if !errors.Is(err, s.ErrInvalid) {
					t.Fatalf("expected ErrInvalid, got %v", err)
				}
				if sink.calls != 0 || len(sink.objects) != 0 {
					t.Fatal("invalid bundle attempted artifact writes")
				}
				if _, err = m.GetOutboxExport(ctx, event.ID); !errors.Is(err, s.ErrNotFound) {
					t.Fatal("invalid bundle has export progress", err)
				}
				if got, err := m.GetOutbox(ctx, event.ID); err != nil || !reflect.DeepEqual(got, event) {
					t.Fatal("source event changed", err)
				}
			})
		}
	}
}

func TestExporterCrashRetryAndRestart(t *testing.T) {
	for _, backend := range []string{"fake", "badger"} {
		for nth := 1; nth <= 3; nth++ {
			for _, after := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/write%d/after=%t", backend, nth, after), func(t *testing.T) {
					ctx := context.Background()
					m, f, reopen := producerFixture(t, backend)
					e, body := commitInput(t, m, f, "first", 0)
					sink := &memoryObjects{objects: map[string][]byte{}, fail: nth, after: after}
					w := Exporter{Metadata: m, Payloads: memoryInputs{e.PayloadPointer: body}, Objects: sink}
					if err := w.Export(ctx, e.ID); !errors.Is(err, crash) {
						t.Fatalf("fault: %v", err)
					}
					if _, err := m.GetOutboxExport(ctx, e.ID); !errors.Is(err, s.ErrNotFound) {
						t.Fatalf("premature flag: %v", err)
					}
					m = reopen()
					w.Metadata = m
					sink.fail = 0
					if err := w.Export(ctx, e.ID); err != nil {
						t.Fatal(err)
					}
					first, err := m.GetOutboxExport(ctx, e.ID)
					if err != nil {
						t.Fatal(err)
					}
					if len(sink.objects) != 3 {
						t.Fatal("missing/duplicate artifacts")
					}
					var descriptor Descriptor
					if err := Decode(sink.objects[first.DescriptorKey], &descriptor); err != nil || len(descriptor.Pages) != 2 {
						t.Fatal("descriptor", err)
					}
					if err := w.Export(ctx, e.ID); err != nil {
						t.Fatal(err)
					}
					m = reopen()
					again, err := m.GetOutboxExport(ctx, e.ID)
					if err != nil || !reflect.DeepEqual(first, again) {
						t.Fatal("durability", err)
					}
					if _, err := m.GetOutbox(ctx, e.ID); err != nil {
						t.Fatal("source event lost", err)
					}
					changed := first
					changed.Event.Revision++
					if err := m.MarkOutboxExported(ctx, changed); !errors.Is(err, s.ErrConflict) {
						t.Fatal("stale event accepted", err)
					}
					changed = first
					changed.DescriptorSHA256 = strings.Repeat("c", 64)
					if err := m.MarkOutboxExported(ctx, changed); !errors.Is(err, s.ErrConflict) {
						t.Fatal("different descriptor accepted", err)
					}
				})
			}
		}
	}
}
func TestExportIntegrityAndMissingPayload(t *testing.T) {
	for _, mode := range []string{"missing", "digest", "page-binding", "readback", "collision"} {
		t.Run(mode, func(t *testing.T) {
			m, f, _ := producerFixture(t, "fake")
			e, body := commitInput(t, m, f, "integrity", 0)
			source := memoryInputs{e.PayloadPointer: body}
			sink := &memoryObjects{objects: map[string][]byte{}}
			var metadata s.OutboxExportStore = m
			switch mode {
			case "missing":
				delete(source, e.PayloadPointer)
			case "digest":
				source[e.PayloadPointer] = []byte("{}")
			case "page-binding":
				var in Input
				_ = Decode(body, &in)
				in.Pages[0].Event = strings.Repeat("a", 64)
				b, _ := Encode(in)
				source[e.PayloadPointer] = b
				modified := e
				modified.PayloadSHA256 = digest(b)
				metadata = eventOverride{m, modified}
			case "readback":
				sink.corrupt = true
			case "collision":
				sink.objects[eventRoot(e)+"pages/0.json"] = []byte("different")
			}
			w := Exporter{Metadata: metadata, Payloads: source, Objects: sink}
			if err := w.Export(context.Background(), e.ID); err == nil {
				t.Fatal("integrity failure accepted")
			}
			if _, err := m.GetOutboxExport(context.Background(), e.ID); !errors.Is(err, s.ErrNotFound) {
				t.Fatal("marked exported", err)
			}
			if mode == "page-binding" && len(sink.objects) != 0 {
				t.Fatal("invalid page wrote artifacts")
			}
		})
	}
}
func TestOutboxPaginationAndHistoryIsolation(t *testing.T) {
	for _, backend := range []string{"fake", "badger"} {
		t.Run(backend, func(t *testing.T) {
			m, f, _ := producerFixture(t, backend)
			e, b := commitInput(t, m, f, "one", 0)
			e2, b2 := commitInput(t, m, f, "two", 1)
			w := Exporter{Metadata: m, Payloads: memoryInputs{e.PayloadPointer: b, e2.PayloadPointer: b2}, Objects: &memoryObjects{objects: map[string][]byte{}}}
			q := s.OutboxQuery{HistoryID: e.HistoryID, Epoch: e.Epoch, Limit: 1}
			next, err := w.RunPage(context.Background(), q)
			if err != nil || next == "" {
				t.Fatal(next, err)
			}
			q.After = next
			next, err = w.RunPage(context.Background(), q)
			if err != nil || next != "" {
				t.Fatal(next, err)
			}
			q.After = ""
			q.HistoryID = strings.Repeat("f", 32)
			page, err := m.ListOutbox(context.Background(), q)
			if err != nil || len(page.Events) != 0 {
				t.Fatal("history leak", err)
			}
			if page.Next == "" {
				t.Fatal("filtered scan failed to return bounded continuation")
			}
			q.After = page.Next
			page, err = m.ListOutbox(context.Background(), q)
			if err != nil || len(page.Events) != 0 || page.Next != "" {
				t.Fatal("filtered continuation", err)
			}
		})
	}
}
func TestCanonicalSchemaAndMembership(t *testing.T) {
	h := strings.Repeat("1", 32)
	id := strings.Repeat("a", 64)
	r := Reference{Length: 12, SHA256: strings.Repeat("b", 64)}
	c := Catalog{r, h, strings.Repeat("2", 32), 1}
	b, err := Encode(c)
	if err != nil {
		t.Fatal(err)
	}
	var got Catalog
	if Decode(b, &got) != nil || got.Validate() != nil {
		t.Fatal("catalog")
	}
	for _, bad := range []string{string(b) + "\n", strings.Replace(string(b), `"r2_version":1`, `"r2_version":2`, 1), strings.Replace(string(b), `"r2_version":1`, `"r2_version":1,"extra":true`, 1), strings.Replace(string(b), `"r2_version":1`, `"r2_version":1,"r2_version":1`, 1)} {
		var x Catalog
		err := Decode([]byte(bad), &x)
		if err == nil {
			err = x.Validate()
		}
		if err == nil {
			t.Fatal("invalid schema accepted", bad)
		}
	}
	v := Inventory{Epoch: 0, Events: []Member{{r, id}}, History: h, Version: 1, State: "Closed"}
	if v.ValidateExact(h, 0, []string{id}) != nil {
		t.Fatal("exact membership")
	}
	for _, ids := range [][]string{nil, {strings.Repeat("c", 64)}, {id, id}} {
		if v.ValidateExact(h, 0, ids) == nil {
			t.Fatal("wrong membership accepted")
		}
	}
	v.State = "Open"
	if v.ValidateExact(h, 0, []string{id}) == nil {
		t.Fatal("open cut accepted")
	}
	v.State = "Closed"
	v.Events = []Member{}
	if v.ValidateExact(h, 0, []string{}) != nil {
		t.Fatal("explicit empty inventory")
	}
	if _, err := Encode(map[string]any{"unicode": "Ti\u1ebfng Vi\u1ec7t"}); err != nil {
		t.Fatal("canonical Unicode rejected", err)
	}
	if Decode(bytes.Repeat([]byte("x"), MaxMetadataBytes+1), new(Input)) == nil {
		t.Fatal("oversize accepted")
	}
}

type progressFault struct {
	metadataStore
	after bool
}

func (m progressFault) MarkOutboxExported(ctx context.Context, p s.OutboxExport) error {
	if m.after {
		if err := m.metadataStore.MarkOutboxExported(ctx, p); err != nil {
			return err
		}
	}
	return crash
}
func TestExportProgressLostACK(t *testing.T) {
	for _, backend := range []string{"fake", "badger"} {
		for _, after := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/after=%t", backend, after), func(t *testing.T) {
				ctx := context.Background()
				m, f, reopen := producerFixture(t, backend)
				e, body := commitInput(t, m, f, "mark", 0)
				sink := &memoryObjects{objects: map[string][]byte{}}
				w := Exporter{Metadata: progressFault{m, after}, Payloads: memoryInputs{e.PayloadPointer: body}, Objects: sink}
				if err := w.Export(ctx, e.ID); !errors.Is(err, crash) {
					t.Fatal(err)
				}
				if len(sink.objects) != 3 {
					t.Fatal("progress before artifacts")
				}
				m = reopen()
				w.Metadata = m
				if err := w.Export(ctx, e.ID); err != nil {
					t.Fatal("retry unknown progress", err)
				}
				if _, err := m.GetOutbox(ctx, e.ID); err != nil {
					t.Fatal("source lost", err)
				}
				if _, err := m.GetOutboxExport(ctx, e.ID); err != nil {
					t.Fatal("missing progress", err)
				}
			})
		}
	}
}

func TestCatalogByteFixture(t *testing.T) {
	b, err := os.ReadFile("../../tests/fixtures/recovery/export-catalog.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Catalog   string   `json:"catalog"`
		Inventory string   `json:"inventory"`
		Members   []string `json:"members"`
	}
	if err = json.Unmarshal(b, &fixture); err != nil {
		t.Fatal(err)
	}
	var c Catalog
	var v Inventory
	if Decode([]byte(fixture.Catalog), &c) != nil || c.Validate() != nil || Decode([]byte(fixture.Inventory), &v) != nil || v.ValidateExact(c.History, 0, fixture.Members) != nil {
		t.Fatal("invalid fixture")
	}
	got, err := Encode(c)
	if err != nil || string(got) != fixture.Catalog {
		t.Fatal("catalog bytes changed", err)
	}
	got, err = Encode(v)
	if err != nil || string(got) != fixture.Inventory {
		t.Fatal("inventory bytes changed", err)
	}
}

func TestConfigProducerExportAndUnsupportedProducers(t *testing.T) {
	for _, backend := range []string{"fake", "badger"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			m, f, _ := producerFixture(t, backend)
			history := strings.Repeat("1", 32)
			op := strings.Repeat("5", 32)
			id, err := s.EventID(history, op, "SystemConfig", "entity_put")
			if err != nil {
				t.Fatal(err)
			}
			doc := `{"tiering":{"enabled":true,"warm_storage_class":"STANDARD","cold_storage_class":"GLACIER_IR","cold_after_days":30,"min_pack_live_ratio":0.8,"auto_restore":true,"auto_restore_days":1,"auto_restore_tier":"Standard"},"compression":{"enabled":true,"codec":"zstd","level":3,"min_saving_ratio":0.1},"chunking":{"min_bytes":262144,"avg_bytes":1048576,"max_bytes":4194304}}`
			body, err := Encode(Input{Pages: []EventPage{{Effects: []Effect{{Key: "SystemConfig/global-policy", Revision: 1, Value: doc}}, Epoch: 0, Event: id, History: history, Number: 0, Version: 1}}})
			if err != nil {
				t.Fatal(err)
			}
			e := s.OutboxEvent{Version: 1, ID: id, HistoryID: history, Epoch: 0, Entity: "SystemConfig", Action: "entity_put", EntityID: "global-policy", Revision: 1, OpID: op, PayloadPointer: "policy/input/first", PayloadSHA256: digest(body)}
			if _, err = m.PutPolicy(ctx, s.PolicyUpdate{Candidate: s.PolicySnapshot{Version: 1, Document: []byte(doc)}, CredentialID: "admin-key", Writer: f.Lease.Fence, Event: &e}); err != nil {
				t.Fatal(err)
			}
			w := Exporter{Metadata: m, Payloads: memoryInputs{e.PayloadPointer: body}, Objects: &memoryObjects{objects: map[string][]byte{}}}
			if err = w.Export(ctx, id); err != nil {
				t.Fatal(err)
			}
			// Runtime heartbeat entities remain outside authoritative business producers.
			// Native rights/location producers are covered by the backend suite.
			if _, err = s.EventID(history, op, "Nodes", "entity_put"); err == nil {
				t.Fatal("unexpected heartbeat producer support")
			}
			chunk := e
			chunk.Entity = "Chunks"
			chunk.Action = "location_set"
			chunk.EntityID = ""
			chunk.ID, err = s.EventID(history, op, chunk.Entity, chunk.Action)
			if err != nil {
				t.Fatal(err)
			}
			if chunk.Validate() == nil {
				t.Fatal("chunk outbox gap changed without integration review")
			}
		})
	}
}
