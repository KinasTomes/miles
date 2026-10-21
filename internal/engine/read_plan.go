package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/KinasTomes/miles/internal/storage"
)

var (
	ErrInvalidReadRange   = errors.New("invalid normalized logical read range")
	ErrInvalidManifest    = errors.New("invalid logical manifest")
	ErrIncompleteManifest = errors.New("incomplete logical manifest")
)

// ManifestValidation records checks possible with today's decoded shared models.
// DescriptorVerified remains false: ObjectHead has no authoritative page_count,
// chunk_count or page_index, and ManifestPage has no serialized envelope/checksum.
// Observed totals are not independent completeness evidence. A successful plan
// must not be used as proof of page authenticity, R1 root binding or read rights.
type ManifestValidation struct {
	ObservedPages      int
	ObservedChunks     int64
	LogicalBytes       int64
	DescriptorVerified bool
}

// PlannedChunk is an ordered logical slice of a complete raw chunk. Ref is a
// value copy of the shared model; repeated hashes at distinct offsets are kept.
// Resolve canonical locations and pin their exact generations separately before
// fetching the full stored chunk, verifying it and emitting this raw slice.
type PlannedChunk struct {
	Ref   storage.ChunkRef
	Slice storage.ByteRange
}

// ReadPlan binds only the supplied logical head identity/revision. It carries no
// physical generation, lease, authority, or checksum-verification claim.
// These local planning APIs are proposals for consumer review, not frozen ports.
type ReadPlan struct {
	Object       storage.ObjectKey
	HeadRevision uint64
	ManifestID   string
	Range        storage.ByteRange
	Chunks       []PlannedChunk
	Validation   ManifestValidation
}

func invalidManifest(detail string) error {
	return fmt.Errorf("%w: %w: %s", ErrInvalidManifest, storage.ErrInvalid, detail)
}
func incompleteManifest(detail string) error {
	return fmt.Errorf("%w: %w: %s", ErrIncompleteManifest, storage.ErrNotFound, detail)
}

func validateReadHead(expected storage.ObjectKey, head storage.ObjectHead, requested *storage.ByteRange) (storage.ByteRange, error) {
	if err := expected.Validate(); err != nil {
		return storage.ByteRange{}, invalidManifest("expected object identity")
	}
	if head.Object != expected {
		return storage.ByteRange{}, invalidManifest("head object/bucket incarnation mismatch")
	}
	if head.Deleted {
		return storage.ByteRange{}, storage.ErrNotFound
	}
	if head.Version != storage.ModelVersion || head.Revision == 0 || head.Size < 0 || head.Size > storage.MaxObjectBytes {
		return storage.ByteRange{}, invalidManifest("head version, revision or size")
	}
	// Reuse shared domain/manifest validation without inventing a second model.
	if err := (storage.ManifestPage{Version: storage.ModelVersion, ManifestID: head.ManifestID, Domain: head.Domain}).Validate(); err != nil {
		return storage.ByteRange{}, invalidManifest("head manifest/domain")
	}
	if requested == nil {
		return storage.ByteRange{Length: head.Size}, nil
	}
	span := *requested
	if err := span.Validate(head.Size); err != nil {
		return storage.ByteRange{}, fmt.Errorf("%w: %w", ErrInvalidReadRange, err)
	}
	return span, nil
}

// PlanRead validates every supplied page before returning any plan. requested is
// nil for the full object (including empty); otherwise it is an already-normalized
// nonempty half-open span [Offset, Offset+Length), entirely inside the object.
// HTTP syntax parsing/clamping is the API boundary's responsibility. Pages must
// arrive ordered from zero; the planner never sorts away duplicate/missing pages.
// Caller supplies an immutable snapshot and must not mutate inputs during calls.
func PlanRead(expected storage.ObjectKey, head storage.ObjectHead, pages []storage.ManifestPage, requested *storage.ByteRange) (ReadPlan, error) {
	span, err := validateReadHead(expected, head, requested)
	if err != nil {
		return ReadPlan{}, err
	}
	if len(pages) > 1024 {
		return ReadPlan{}, invalidManifest("more than 1024 pages")
	}
	if head.Size == 0 && len(pages) != 0 {
		return ReadPlan{}, invalidManifest("empty object has pages")
	}
	if head.Size > 0 && len(pages) == 0 {
		return ReadPlan{}, incompleteManifest("no pages")
	}
	var cursor, count int64
	observedRawBytes := make(map[storage.ChunkKey]int64)
	// Validate the whole supplied manifest, including chunks outside the range.
	for i, page := range pages {
		if page.ManifestID != head.ManifestID || page.Domain != head.Domain {
			return ReadPlan{}, invalidManifest("page manifest/domain binding")
		}
		if page.Number != uint32(i) {
			if page.Number > uint32(i) {
				return ReadPlan{}, incompleteManifest("missing or out-of-order page")
			}
			return ReadPlan{}, invalidManifest("duplicate or out-of-order page")
		}
		if err := page.Validate(); err != nil {
			return ReadPlan{}, invalidManifest("page structure or chunk identity")
		}
		if len(page.Chunks) == 0 {
			return ReadPlan{}, incompleteManifest("empty page")
		}
		for _, ref := range page.Chunks {
			if ref.RawBytes > MaxChunkBytes {
				return ReadPlan{}, invalidManifest("raw chunk exceeds v1 decode bound")
			}
			if ref.LogicalOffset != cursor {
				if ref.LogicalOffset > cursor {
					return ReadPlan{}, incompleteManifest("logical gap")
				}
				return ReadPlan{}, invalidManifest("overlapping or duplicate logical position")
			}
			// Checked subtraction: never add untrusted offsets/lengths before bounds.
			if ref.RawBytes > head.Size-cursor {
				return ReadPlan{}, invalidManifest("logical bytes exceed object size")
			}
			// Content identity has one raw length across all pages and logical
			// occurrences, including those outside the requested range.
			if rawBytes, seen := observedRawBytes[ref.Key]; seen {
				if rawBytes != ref.RawBytes {
					return ReadPlan{}, invalidManifest("conflicting raw length for chunk identity")
				}
			} else {
				observedRawBytes[ref.Key] = ref.RawBytes
			}
			cursor += ref.RawBytes
			count++
		}
	}
	if cursor != head.Size {
		return ReadPlan{}, incompleteManifest("logical bytes shorter than object size")
	}
	plan := ReadPlan{Object: head.Object, HeadRevision: head.Revision, ManifestID: head.ManifestID, Range: span,
		Validation: ManifestValidation{ObservedPages: len(pages), ObservedChunks: count, LogicalBytes: cursor}}
	end := span.Offset + span.Length // validated, bounded by head.Size
	for _, page := range pages {
		for _, ref := range page.Chunks {
			start := max(span.Offset, ref.LogicalOffset)
			stop := min(end, ref.LogicalOffset+ref.RawBytes)
			if start < stop {
				plan.Chunks = append(plan.Chunks, PlannedChunk{Ref: ref, Slice: storage.ByteRange{Offset: start - ref.LogicalOffset, Length: stop - start}})
			}
		}
	}
	return plan, nil
}

// ResolveReadPlan loads the full decoded manifest through the existing port; it
// is not page-index optimized. Caller must authorize and acquire/bind the R1
// read view before calling, and retain logical-root protection through resolution.
// Empty objects need no page I/O. Canonical location/pin resolution is not done.
func ResolveReadPlan(ctx context.Context, store storage.MetadataStore, expected storage.ObjectKey, head storage.ObjectHead, requested *storage.ByteRange) (ReadPlan, error) {
	if err := ctx.Err(); err != nil {
		return ReadPlan{}, err
	}
	if _, err := validateReadHead(expected, head, requested); err != nil {
		return ReadPlan{}, err
	}
	if store == nil {
		return ReadPlan{}, fmt.Errorf("%w: missing metadata store", storage.ErrInvalid)
	}
	var pages []storage.ManifestPage
	if head.Size > 0 {
		var err error
		pages, err = store.GetManifest(ctx, head.ManifestID)
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				return ReadPlan{}, fmt.Errorf("%w: %w", ErrIncompleteManifest, err)
			}
			return ReadPlan{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return ReadPlan{}, err
	}
	return PlanRead(expected, head, pages, requested)
}
