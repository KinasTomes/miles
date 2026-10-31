package dynamodb

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

type r1PinEntry struct {
	Version  uint32
	Fence    s.WriterFence
	Pack     s.PackRef
	Released bool
}

func (p r1PinEntry) Validate() error {
	if p.Version != s.ModelVersion || p.Fence.LeaseID == "" || p.Fence.Epoch == 0 || p.Pack.Validate() != nil {
		return s.ErrInvalid
	}
	return nil
}
func r1PinPrefix(f s.WriterFence) string { return fmt.Sprintf("%016x/pin/", f.Epoch) }
func r1Cursor(item Item, resource, prefix string) (string, error) {
	pk, ok := item["resource_id"].(*types.AttributeValueMemberS)
	sk, skOK := item["lease_id"].(*types.AttributeValueMemberS)
	if !ok || pk == nil || pk.Value != resource || !skOK || sk == nil || !strings.HasPrefix(sk.Value, prefix) || len(sk.Value) != len(prefix)+64 {
		return "", s.ErrUnavailable
	}
	return sk.Value, nil
}
func r1PinEntryKey(f s.WriterFence, p s.PackRef) Key {
	b, _ := json.Marshal(p)
	h := sha256.Sum256(b)
	k := stringKey("Leases", "resource_id", "r1/view/"+f.LeaseID)
	k.Attributes["lease_id"] = text(r1PinPrefix(f) + hex.EncodeToString(h[:]))
	return k
}

// Per-lease membership is paged in Leases, not a giant list on the producer.
// Each batch uses at most 98 native transaction actions (32*3 + lease + gate).
func (m *Store) PinR1(ctx context.Context, f s.WriterFence, refs []s.PackRef) error {
	if len(refs) == 0 || len(refs) > 32 {
		return s.ErrInvalid
	}
	return m.update(ctx, func(t *transaction) error {
		l, err := m.r1Check(t, f)
		if err != nil {
			return err
		}
		if !l.Resolving {
			return s.ErrFenced
		}
		for _, p := range refs {
			if p.Validate() != nil {
				return s.ErrInvalid
			}
			entry, err := r1Read[r1PinEntry](t, r1PinEntryKey(f, p))
			if err == nil {
				if entry.Fence != f || entry.Pack != p || entry.Released {
					return s.ErrUnavailable
				}
				continue
			}
			if !errors.Is(err, s.ErrNotFound) {
				return err
			}
			info, err := get[s.PackInfo](t, packKey(p))
			if err != nil {
				return err
			}
			switch info.State {
			case "Warm", "Cold", "Restoring", "Restored":
			default:
				return s.ErrR1Busy
			}
			pins, err := r1Read[r1Pins](t, r1PinKey(p))
			if errors.Is(err, s.ErrNotFound) {
				pins = r1Pins{Version: s.ModelVersion, Pack: p}
			} else if err != nil {
				return err
			}
			if pins.Pack != p {
				return s.ErrUnavailable
			}
			pins.Count++
			l.PinCount++
			if err = r1Put(t, r1PinKey(p), pins); err != nil {
				return err
			}
			if err = r1Put(t, r1PinEntryKey(f, p), r1PinEntry{Version: s.ModelVersion, Fence: f, Pack: p}); err != nil {
				return err
			}
		}
		l.Revision++
		return r1Put(t, r1LeaseKey(f.LeaseID), l)
	})
}

// Closed authority freezes pin membership before consistent base Query. Cursor
// and decrements commit together. A lost ACK/retry cannot double-decrement or
// skip a pin, and Active/resolving protection stays until the final batch.
func (m *Store) ReleaseR1(ctx context.Context, f s.WriterFence) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		before, err := m.GetR1Lease(ctx, f.LeaseID)
		if err != nil {
			return err
		}
		if before.Fence != f {
			return s.ErrFenced
		}
		if before.State == "Released" {
			return nil
		}
		if before.State != "Closed" || len(before.Pending) != 0 {
			return s.ErrR1Busy
		}
		resource := "r1/view/" + f.LeaseID
		in := &ddb.QueryInput{TableName: aws.String(m.tables["Leases"]), ConsistentRead: aws.Bool(true), Limit: aws.Int32(32), KeyConditionExpression: aws.String("#pk = :pk AND begins_with(#sk, :prefix)"), ExpressionAttributeNames: map[string]string{"#pk": "resource_id", "#sk": "lease_id"}, ExpressionAttributeValues: Item{":pk": text(resource), ":prefix": text(r1PinPrefix(f))}}
		if before.PinCursor != "" {
			in.ExclusiveStartKey = Item{"resource_id": text(resource), "lease_id": text(before.PinCursor)}
		}
		page, err := m.api.Query(ctx, in)
		if err != nil {
			return backend(err)
		}
		if page == nil {
			return s.ErrUnavailable
		}
		next := ""
		if len(page.LastEvaluatedKey) > 0 {
			nextItem, err := r1Cursor(page.LastEvaluatedKey, resource, r1PinPrefix(f))
			if err != nil {
				return err
			}
			next = nextItem
			if next == before.PinCursor {
				return s.ErrUnavailable
			}
		}
		err = m.update(ctx, func(t *transaction) error {
			l, err := r1Read[s.R1Lease](t, r1LeaseKey(f.LeaseID))
			if err != nil {
				return err
			}
			if l.Fence != f {
				return s.ErrFenced
			}
			if l.State == "Released" {
				return nil
			}
			if l.State != "Closed" || len(l.Pending) != 0 {
				return s.ErrR1Busy
			}
			if l.PinCursor != before.PinCursor {
				return s.ErrConflict
			}
			for _, item := range page.Items {
				if _, err := r1Cursor(item, resource, r1PinPrefix(f)); err != nil {
					return err
				}
				entry, err := r1Decode[r1PinEntry](item)
				if err != nil {
					return err
				}
				if entry.Fence != f {
					return s.ErrUnavailable
				}
				k := r1PinEntryKey(f, entry.Pack)
				actual, err := r1Read[r1PinEntry](t, k)
				if err != nil {
					return err
				}
				if actual.Fence != f || actual.Pack != entry.Pack {
					return s.ErrUnavailable
				}
				if actual.Released {
					continue
				}
				pins, err := r1Read[r1Pins](t, r1PinKey(entry.Pack))
				if err != nil {
					return err
				}
				if pins.Pack != entry.Pack || pins.Count == 0 || l.PinCount == 0 {
					return s.ErrUnavailable
				}
				pins.Count--
				l.PinCount--
				actual.Released = true
				if err = r1Put(t, r1PinKey(entry.Pack), pins); err != nil {
					return err
				}
				if err = r1Put(t, k, actual); err != nil {
					return err
				}
			}
			l.PinCursor = next
			l.Revision++
			if next == "" {
				if l.PinCount != 0 {
					return s.ErrUnavailable
				}
				g, err := r1Read[s.R1Gate](t, r1GateKey())
				if err != nil {
					return err
				}
				if g.Incarnation != f.ServiceIncarnation || g.Active == 0 {
					return s.ErrUnavailable
				}
				g.Active--
				if l.Resolving {
					if g.Resolving == 0 {
						return s.ErrUnavailable
					}
					g.Resolving--
				}
				g.Revision++
				l.State = "Released"
				l.Resolving = false
				if err = r1Put(t, r1GateKey(), g); err != nil {
					return err
				}
			}
			return r1Put(t, r1LeaseKey(f.LeaseID), l)
		})
		if errors.Is(err, s.ErrConflict) {
			continue
		}
		if err != nil {
			return err
		}
	}
}
