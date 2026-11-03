package recovery

import (
	"context"
	"sort"

	s "github.com/KinasTomes/miles/internal/storage"
)

// ProvisionAuditedHistory accepts the exact reviewed initial seed. It does not
// scan or bless a running database. Call only in the provisioning-only phase,
// before producer admission; opaque prototype genesis bodies are not migrated.
func ProvisionAuditedHistory(ctx context.Context, m s.RecoveryStore, objects ImmutableStore, history, incarnation string, rows []Effect, dependencies []Dependency) (Catalog, error) {
	var zero Catalog
	if m == nil || objects == nil || !isHex(history, 32) || !isHex(incarnation, 32) {
		return zero, s.ErrInvalid
	}
	ordered := append([]Effect{}, rows...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Key < ordered[j].Key })
	base := Snapshot{Version: 1, Mode: "historical", History: history, BeforeFirst: true, Pages: []AttachmentReference{}, Dependencies: []Dependency{}}
	w := Checkpointer{Objects: objects}
	if e := w.writeDependencies(ctx, &base, dependencies); e != nil {
		return zero, e
	}
	for at := 0; at < len(ordered); {
		page := snapshotPage{Version: 1, History: history, Number: len(base.Pages), Rows: []Effect{}}
		for at < len(ordered) && len(page.Rows) < 128 {
			if !validEffect(ordered[at]) || at > 0 && ordered[at-1].Key == ordered[at].Key {
				return zero, s.ErrInvalid
			}
			page.Rows = append(page.Rows, ordered[at])
			if _, e := Encode(page); e != nil {
				page.Rows = page.Rows[:len(page.Rows)-1]
				break
			}
			at++
		}
		if len(page.Rows) == 0 || len(base.Pages) >= MaxFanout {
			return zero, s.ErrInvalid
		}
		body, e := Encode(page)
		if e != nil {
			return zero, e
		}
		r := AttachmentReference{Key: "recovery/histories/" + history + "/pages/" + digest(body) + ".json", Reference: ref(body)}
		if e = putVerified(ctx, objects, r.Key, body); e != nil {
			return zero, e
		}
		base.Pages = append(base.Pages, r)
	}
	body, e := Encode(base)
	if e != nil {
		return zero, e
	}
	c := Catalog{Version: 1, History: history, Incarnation: incarnation, Genesis: ref(body)}
	if e = putVerified(ctx, objects, genesisKey(c), body); e != nil {
		return zero, e
	}
	w.Catalog = c
	if _, e = w.Rebuild(ctx, AttachmentReference{Key: genesisKey(c), Reference: c.Genesis}); e != nil {
		return zero, e
	}
	if e = ProvisionHistory(ctx, m, objects, c, body); e != nil {
		return zero, e
	}
	return c, nil
}
