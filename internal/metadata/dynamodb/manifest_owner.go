package dynamodb

import (
	"context"
	"errors"
	"reflect"
	"strconv"

	"github.com/KinasTomes/miles/internal/manifest"
	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// ManifestAuthority must establish retained logical roots and dependency
// protection, not merely validate a writer's wall-clock expiry. All observations
// are checked atomically both when freezing/verifying and at final publication.
type ManifestAuthority interface {
	CheckManifest(context.Context, s.WriterFence, string, Observe) error
}
type manifestOwner struct {
	State, Domain, OpID string
	Writer              s.WriterFence
	Revision            uint64
	Size                int64
	PageCount           uint32
	ChunkCount          uint64
	Index               []byte
}

func advanceOwner(owner *manifestOwner) error {
	if owner.Revision == 0 || owner.Revision == ^uint64(0) {
		return s.ErrUnavailable
	}
	owner.Revision++
	return nil
}

func (m *Store) protect(t *transaction, w s.WriterFence, id string) error {
	authority, ok := m.authority.(ManifestAuthority)
	if !ok {
		return s.ErrFenced
	}
	return authority.CheckManifest(t.ctx, w, id, func(k Key) (Item, error) {
		row, err := t.observe(k)
		if err != nil {
			return nil, err
		}
		if row.after == nil {
			return nil, s.ErrNotFound
		}
		return copyItem(row.after), nil
	})
}

func (m *Store) prepareManifest(ctx context.Context, w s.WriterFence, h *s.ObjectHead) error {
	k := key("manifest-owner", h.ManifestID)
	if err := m.update(ctx, func(t *transaction) error {
		if err := m.fence(t, w); err != nil {
			return err
		}
		if err := m.protect(t, w, h.ManifestID); err != nil {
			return err
		}
		owner, err := get[manifestOwner](t, k)
		if errors.Is(err, s.ErrNotFound) {
			owner = manifestOwner{State: "Building", Domain: h.Domain, Writer: w, Revision: 1}
		} else if err != nil {
			return err
		}
		if owner.Domain != h.Domain || owner.State == "Published" || (owner.OpID != "" && owner.OpID != h.OpID) {
			return s.ErrConflict
		}
		if owner.Revision == 0 || owner.Revision == ^uint64(0) || (owner.State != "Building" && owner.State != "Closing" && owner.State != "Verified") {
			return s.ErrUnavailable
		}
		if owner.State == "Verified" {
			return nil
		}
		owner.State = "Closing"
		owner.Writer = w
		owner.OpID = h.OpID
		if err := advanceOwner(&owner); err != nil {
			return err
		}
		return put(t, k, owner)
	}); err != nil {
		return err
	}
	// Keep the original protection observations until the Verified CAS. Page and
	// chunk enumeration is outside the bounded transaction, under frozen membership.
	t := m.transaction(ctx)
	if err := m.fence(t, w); err != nil {
		return err
	}
	if err := m.protect(t, w, h.ManifestID); err != nil {
		return err
	}
	owner, err := get[manifestOwner](t, k)
	if err != nil {
		return err
	}
	if owner.OpID != h.OpID || owner.Domain != h.Domain {
		return s.ErrConflict
	}
	if owner.State == "Closing" {
		candidate := *h
		if err := m.verifyFrozenManifest(ctx, &candidate); err != nil {
			// A definite validation rejection before any publication attempt may
			// reopen staging at a NEW owner revision. Unknown I/O never reopens.
			if errors.Is(err, s.ErrInvalid) || errors.Is(err, s.ErrNotFound) {
				if reopenErr := m.reopenRejectedPreparation(ctx, w, h.ManifestID, h.OpID, owner.Revision); reopenErr != nil {
					return reopenErr
				}
			}
			return err
		}
		owner.Size, owner.PageCount, owner.ChunkCount = candidate.Size, candidate.PageCount, candidate.ChunkCount
		owner.Index, err = manifest.EncodePageIndex(candidate)
		if err != nil {
			return err
		}
		owner.State = "Verified"
		if err := advanceOwner(&owner); err != nil {
			return err
		}
		if err := put(t, k, owner); err != nil {
			return err
		}
		if err := t.commit(); err != nil {
			return err
		}
	} else if owner.State != "Verified" {
		return s.ErrConflict
	}
	index, err := manifest.DecodePageIndex(owner.Index, owner.PageCount)
	if err != nil {
		return err
	}
	if h.Size != owner.Size {
		return s.ErrInvalid
	}
	if (h.PageCount != 0 || h.ChunkCount != 0 || len(h.PageIndex) != 0) && (h.PageCount != owner.PageCount || h.ChunkCount != owner.ChunkCount || !reflect.DeepEqual(h.PageIndex, index)) {
		return s.ErrInvalid
	}
	h.PageCount, h.ChunkCount, h.PageIndex = owner.PageCount, owner.ChunkCount, index
	return nil
}

func (m *Store) verifyFrozenManifest(ctx context.Context, h *s.ObjectHead) error {
	capacity, err := manifest.RefCapacity(h.ManifestID, h.Domain)
	if err != nil {
		return err
	}
	var cursor Item
	var offset int64
	var count uint32
	var chunks uint64
	index := []uint64{}
	previous := capacity
	for {
		out, err := m.api.Query(ctx, &ddb.QueryInput{TableName: aws.String(m.tables["ManifestPages"]), ConsistentRead: aws.Bool(true), Limit: aws.Int32(1), KeyConditionExpression: aws.String("#owner = :owner"), ExpressionAttributeNames: map[string]string{"#owner": "manifest_id"}, ExpressionAttributeValues: Item{":owner": text(h.ManifestID)}, ExclusiveStartKey: cursor})
		if err != nil {
			return backend(err)
		}
		if out == nil {
			return s.ErrUnavailable
		}
		for _, item := range out.Items {
			if count >= manifest.MaxPages || previous != capacity {
				return s.ErrInvalid
			}
			body, err := CloneBinary(item["body"])
			if err != nil {
				return err
			}
			number, ok := item["page_no"].(*types.AttributeValueMemberN)
			if !ok || number.Value != stringNumber(count) {
				return s.ErrInvalid
			}
			page, err := manifest.Decode(body, manifest.ObjectPage, h.ManifestID, count)
			if err != nil {
				return err
			}
			if page.Domain != h.Domain {
				return s.ErrInvalid
			}
			previous = len(page.Chunks)
			index = append(index, uint64(offset))
			count++
			keys := make([]s.ChunkKey, len(page.Chunks))
			for i, r := range page.Chunks {
				keys[i] = r.Key
			}
			canonical, err := m.GetChunks(ctx, keys, true)
			if err != nil {
				return err
			}
			for _, r := range page.Chunks {
				c, ok := canonical[r.Key]
				if !ok {
					return s.ErrNotFound
				}
				if c.RawBytes != r.RawBytes || r.LogicalOffset != offset || offset > s.MaxObjectBytes-r.RawBytes {
					return s.ErrInvalid
				}
				offset += r.RawBytes
				chunks++
			}
		}
		cursor = out.LastEvaluatedKey
		if len(cursor) == 0 {
			break
		}
	}
	if count == 0 && h.Size != 0 {
		return s.ErrNotFound
	}
	if offset != h.Size {
		return s.ErrInvalid
	}
	if (h.PageCount != 0 || h.ChunkCount != 0 || len(h.PageIndex) != 0) && (h.PageCount != count || h.ChunkCount != chunks || !reflect.DeepEqual(h.PageIndex, index)) {
		return s.ErrInvalid
	}
	if count == 0 {
		index = nil
	}
	h.PageCount, h.ChunkCount, h.PageIndex = count, chunks, index
	return nil
}
func stringNumber(n uint32) string { return strconv.FormatUint(uint64(n), 10) }

func (m *Store) reopenRejectedPreparation(ctx context.Context, w s.WriterFence, id, op string, revision uint64) error {
	return m.update(ctx, func(t *transaction) error {
		if err := m.fence(t, w); err != nil {
			return err
		}
		owner, err := get[manifestOwner](t, key("manifest-owner", id))
		if err != nil {
			return err
		}
		if owner.State != "Closing" || owner.OpID != op || owner.Revision != revision || owner.Writer != w {
			return s.ErrConflict
		}
		operation, err := t.load(key("outcomes", op))
		if err != nil {
			return err
		}
		if operation.after != nil {
			if operation.after["pending_request"] != nil || operation.after["result"] != nil || operation.after["event_id"] != nil {
				return s.ErrUnavailable
			}
		}
		owner.State = "Building"
		owner.OpID = ""
		if err := advanceOwner(&owner); err != nil {
			return err
		}
		return put(t, key("manifest-owner", id), owner)
	})
}
