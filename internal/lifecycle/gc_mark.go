package lifecycle

import (
	"context"
	"fmt"
	"sort"

	"github.com/KinasTomes/miles/internal/engine"
	s "github.com/KinasTomes/miles/internal/storage"
)

type RootInventory interface {
	s.ObservationStore
	GetManifestPage(context.Context, string, uint32) (s.ManifestPage, error)
	GetPartPage(context.Context, string, uint32) (s.ManifestPage, error)
	ObserveUploadParts(context.Context, string, uint32, int) (s.PartListPage, error)
	ListPublishers(context.Context, string, string, int) (s.PublisherPage, error)
}

type RootReason uint8

const (
	LiveHead RootReason = 1 << iota
	UnfinishedUpload
	ReadOrWriterPin
	RecoveryCut
)

type Mark struct {
	Packs      map[s.PackRef]s.PackInfo
	Roots      map[s.PackRef]RootReason
	Locations  map[s.ChunkKey]s.Chunk
	Refcounts  map[s.ChunkKey]uint64
	Referenced map[s.ChunkKey]bool
	LiveBytes  map[s.PackRef]int64
}

// Marker is read-only. Its report becomes stable for mutation only while
// the caller owns maintenance and has quiesced producers and resolving views.
type Marker struct {
	Store                     RootInventory
	Pinned                    func(context.Context, s.PackRef) (bool, error)
	RecoveryRoots             func(context.Context) ([]s.PackRef, error)
	MaxRecords, MaxReferences int
}

func (m Marker) Collect(ctx context.Context) (*Mark, error) {
	if m.Store == nil || m.Pinned == nil || m.RecoveryRoots == nil || m.MaxRecords < 1 || m.MaxReferences < 1 {
		return nil, s.ErrInvalid
	}
	out := &Mark{Packs: map[s.PackRef]s.PackInfo{}, Roots: map[s.PackRef]RootReason{}, Locations: map[s.ChunkKey]s.Chunk{}, Refcounts: map[s.ChunkKey]uint64{}, Referenced: map[s.ChunkKey]bool{}, LiveBytes: map[s.PackRef]int64{}}
	buckets := map[string]s.Bucket{}
	records, refs := 0, 0
	reserve := func(n int) error {
		if n > m.MaxRecords-records {
			return s.ErrUnavailable
		}
		records += n
		return nil
	}
	add := func(ref s.PackRef, reason RootReason) error {
		if ref.Validate() != nil {
			return s.ErrUnavailable
		}
		out.Roots[ref] |= reason
		return nil
	}
	reference := func(ref s.ChunkRef, reason RootReason) error {
		refs++
		if refs > m.MaxReferences {
			return s.ErrUnavailable
		}
		chunk, ok := out.Locations[ref.Key]
		if !ok || chunk.RawBytes != ref.RawBytes {
			return fmt.Errorf("%w: missing or inconsistent canonical chunk", s.ErrUnavailable)
		}
		out.Referenced[ref.Key] = true
		if reason == LiveHead {
			if out.Refcounts[ref.Key] == 0 {
				out.LiveBytes[chunk.Location.Pack] += chunk.Location.Range.Length
			}
			out.Refcounts[ref.Key]++
		}
		return add(chunk.Location.Pack, reason)
	}
	walkHead := func(h s.ObjectHead, reason RootReason) error {
		if h.ValidateManifestDescriptor() != nil || reserve(int(h.PageCount)) != nil {
			return s.ErrUnavailable
		}
		pages := make([]s.ManifestPage, 0, h.PageCount)
		for n := uint32(0); n < h.PageCount; n++ {
			p, e := m.Store.GetManifestPage(ctx, h.ManifestID, n)
			if e != nil {
				return e
			}
			if len(p.Chunks) > m.MaxReferences-refs {
				return s.ErrUnavailable
			}
			for _, r := range p.Chunks {
				if e = reference(r, reason); e != nil {
					return e
				}
			}
			pages = append(pages, p)
		}
		_, e := engine.PlanRead(h.Object, h, pages, nil)
		return e
	}
	for _, kind := range []string{"buckets", "packs", "chunks", "heads", "uploads"} {
		cursor := ""
		seen := map[string]bool{}
		for {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if seen[cursor] || len(seen) >= m.MaxRecords {
				return nil, s.ErrUnavailable
			}
			seen[cursor] = true
			p, e := m.Store.ObservePage(ctx, s.ObservationRequest{Kind: kind, Limit: 100, Cursor: cursor})
			if e != nil {
				return nil, e
			}
			if e = reserve(len(p.Buckets) + len(p.Packs) + len(p.Chunks) + len(p.Heads) + len(p.Uploads)); e != nil {
				return nil, e
			}
			for _, b := range p.Buckets {
				if _, ok := buckets[b.Name]; ok {
					return nil, s.ErrUnavailable
				}
				buckets[b.Name] = b
			}
			for _, pack := range p.Packs {
				if _, ok := out.Packs[pack.Ref]; ok || pack.Ref.Validate() != nil {
					return nil, s.ErrUnavailable
				}
				out.Packs[pack.Ref] = pack
				pinned, e := m.Pinned(ctx, pack.Ref)
				if e != nil {
					return nil, e
				}
				if pinned {
					out.Roots[pack.Ref] |= ReadOrWriterPin
				}
			}
			for _, c := range p.Chunks {
				pack, ok := out.Packs[c.Location.Pack]
				if !ok || c.Validate() != nil || c.Location.Range.Validate(pack.Size) != nil {
					return nil, s.ErrUnavailable
				}
				if _, ok := out.Locations[c.Key]; ok {
					return nil, s.ErrUnavailable
				}
				out.Locations[c.Key] = c
			}
			for _, h := range p.Heads {
				b, ok := buckets[h.Object.Bucket]
				if h.Deleted || !ok || b.ID != h.Object.BucketID {
					continue
				}
				if b.Domain != h.Domain {
					return nil, s.ErrUnavailable
				}
				if e = walkHead(h, LiveHead); e != nil {
					return nil, e
				}
			}
			for _, u := range p.Uploads {
				switch u.State {
				case "Completed", "Aborted":
					continue
				case "Active", "Completing", "Aborting":
				default:
					return nil, s.ErrUnavailable
				}
				if u.ID == "" || u.Object.Validate() != nil || u.Domain == "" {
					return nil, s.ErrUnavailable
				}
				publishedBytes := int64(0)
				after := uint32(0)
				for {
					parts, e := m.Store.ObserveUploadParts(ctx, u.ID, after, 100)
					if e != nil {
						return nil, e
					}
					if e = reserve(1 + len(parts.Parts)); e != nil {
						return nil, e
					}
					last := after
					for _, part := range parts.Parts {
						if part.UploadID != u.ID || part.Number <= last || part.Number > 10000 || part.ManifestID == "" || part.Size < 0 || part.Size > 5<<30 || part.Pages > s.MaxManifestPages || reserve(int(part.Pages)) != nil {
							return nil, s.ErrUnavailable
						}
						last = part.Number
						offset := int64(0)
						for n := uint32(0); n < part.Pages; n++ {
							page, e := m.Store.GetPartPage(ctx, part.ManifestID, n)
							if e != nil {
								return nil, e
							}
							if page.ManifestID != part.ManifestID || page.Number != n || page.Domain != u.Domain || page.Validate() != nil {
								return nil, s.ErrUnavailable
							}
							for _, r := range page.Chunks {
								if r.LogicalOffset != offset || r.RawBytes > part.Size-offset {
									return nil, s.ErrUnavailable
								}
								offset += r.RawBytes
								if e = reference(r, UnfinishedUpload); e != nil {
									return nil, e
								}
							}
						}
						if offset != part.Size || part.Size > u.PublishedBytes-publishedBytes {
							return nil, s.ErrUnavailable
						}
						publishedBytes += part.Size
					}
					if parts.Next == 0 {
						break
					}
					if parts.Next <= after || parts.Next != last {
						return nil, s.ErrUnavailable
					}
					after = parts.Next
				}
				if publishedBytes != u.PublishedBytes {
					return nil, s.ErrUnavailable
				}
				// A frozen completion retains all of its published part heads. Include
				// the prepared object manifest as well when the snapshot has been bound.
				if u.Prepared.ManifestID != "" {
					if u.Prepared.Object != u.Object || u.Prepared.Domain != u.Domain {
						return nil, s.ErrUnavailable
					}
					prepared := u.Prepared
					// Prepared heads have no committed revision yet.
					prepared.Revision = 1
					if e = walkHead(prepared, UnfinishedUpload); e != nil {
						return nil, e
					}
				}
				openPublishers := uint32(0)
				publisherCursor := ""
				for {
					page, e := m.Store.ListPublishers(ctx, u.ID, publisherCursor, 100)
					if e != nil {
						return nil, e
					}
					if e = reserve(1 + len(page.Publishers)); e != nil {
						return nil, e
					}
					for _, publisher := range page.Publishers {
						if publisher.UploadID != u.ID {
							return nil, s.ErrUnavailable
						}
						switch publisher.State {
						case "Published", "Cancelled":
							continue
						case "Open":
							openPublishers++
						default:
							return nil, s.ErrUnavailable
						}
						if e = reserve(len(publisher.Packs)); e != nil {
							return nil, e
						}
						for _, ref := range publisher.Packs {
							if e = add(ref, UnfinishedUpload); e != nil {
								return nil, e
							}
						}
					}
					if page.Next == "" {
						break
					}
					if page.Next <= publisherCursor {
						return nil, s.ErrUnavailable
					}
					publisherCursor = page.Next
				}
				if openPublishers != u.Publishers {
					return nil, s.ErrUnavailable
				}
			}
			if p.Next == "" {
				break
			}
			cursor = p.Next
		}
	}
	roots, e := m.RecoveryRoots(ctx)
	if e != nil {
		return nil, e
	}
	if e = reserve(len(roots)); e != nil {
		return nil, e
	}
	for _, ref := range roots {
		if e = add(ref, RecoveryCut); e != nil {
			return nil, e
		}
	}
	return out, ctx.Err()
}

// CanonicalFor keeps every canonical winner on the source, including currently
// unreferenced chunks that remain eligible for PUT reuse.
func (m *Mark) CanonicalFor(source s.PackRef) []s.Chunk {
	var rows []s.Chunk
	for _, c := range m.Locations {
		if c.Location.Pack == source {
			rows = append(rows, c)
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Key.Domain != rows[j].Key.Domain {
			return rows[i].Key.Domain < rows[j].Key.Domain
		}
		return rows[i].Key.Hash < rows[j].Key.Hash
	})
	return rows
}
