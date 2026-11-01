package recovery

import (
	"context"
	"strconv"

	s "github.com/KinasTomes/miles/internal/storage"
)

const membershipFanout = 128
const maxMembershipLevel = 8

type membershipNode struct {
	Version  int                   `json:"r2_version"`
	History  string                `json:"history"`
	Epoch    uint64                `json:"epoch"`
	Level    int                   `json:"level"`
	Count    uint64                `json:"count"`
	Members  []Member              `json:"members,omitempty"`
	Children []AttachmentReference `json:"children,omitempty"`
}
type treeEntry struct {
	ref   AttachmentReference
	count uint64
}
type membershipBuilder struct {
	objects         ImmutableStore
	prefix, history string
	epoch           uint64
	members         []Member
	levels          [maxMembershipLevel + 1][]treeEntry
}

func (b *membershipBuilder) store(ctx context.Context, n membershipNode) (treeEntry, error) {
	body, err := Encode(n)
	if err != nil {
		return treeEntry{}, err
	}
	r := AttachmentReference{Key: b.prefix + "membership/" + digest(body) + ".json", Reference: ref(body)}
	err = putVerified(ctx, b.objects, r.Key, body)
	return treeEntry{r, n.Count}, err
}
func (b *membershipBuilder) push(ctx context.Context, level int, e treeEntry) error {
	if level > maxMembershipLevel {
		return s.ErrInvalid
	}
	b.levels[level] = append(b.levels[level], e)
	if len(b.levels[level]) < membershipFanout {
		return nil
	}
	return b.branch(ctx, level)
}
func (b *membershipBuilder) branch(ctx context.Context, level int) error {
	n := membershipNode{Version: 1, History: b.history, Epoch: b.epoch, Level: level + 1, Children: []AttachmentReference{}}
	for _, child := range b.levels[level] {
		n.Count += child.count
		n.Children = append(n.Children, child.ref)
	}
	if n.Count > maxInteger || n.Level > maxMembershipLevel {
		return s.ErrInvalid
	}
	e, err := b.store(ctx, n)
	if err != nil {
		return err
	}
	b.levels[level] = nil
	return b.push(ctx, level+1, e)
}
func (b *membershipBuilder) leaf(ctx context.Context) error {
	n := membershipNode{Version: 1, History: b.history, Epoch: b.epoch, Members: b.members, Count: uint64(len(b.members))}
	e, err := b.store(ctx, n)
	if err != nil {
		return err
	}
	b.members = nil
	return b.push(ctx, 0, e)
}
func (b *membershipBuilder) add(ctx context.Context, m Member) error {
	b.members = append(b.members, m)
	if len(b.members) == membershipFanout {
		return b.leaf(ctx)
	}
	return nil
}
func (b *membershipBuilder) finish(ctx context.Context) (AttachmentReference, error) {
	empty := true
	for _, level := range b.levels {
		empty = empty && len(level) == 0
	}
	if len(b.members) > 0 || empty {
		if err := b.leaf(ctx); err != nil {
			return AttachmentReference{}, err
		}
	}
	for level := 0; level <= maxMembershipLevel; level++ {
		if len(b.levels[level]) == 0 {
			continue
		}
		higher := false
		for _, l := range b.levels[level+1:] {
			higher = higher || len(l) > 0
		}
		if len(b.levels[level]) == 1 && !higher {
			return b.levels[level][0].ref, nil
		}
		if err := b.branch(ctx, level); err != nil {
			return AttachmentReference{}, err
		}
	}
	return AttachmentReference{}, s.ErrInvalid
}

type membershipVerifier struct {
	objects     ImmutableStore
	history     string
	epoch       uint64
	last, chain string
}

func (v *membershipVerifier) walk(ctx context.Context, r AttachmentReference, parentLevel int) (uint64, error) {
	if r.Key != epochPrefix(v.history, v.epoch)+"membership/"+r.Reference.SHA256+".json" {
		return 0, s.ErrInvalid
	}
	body, err := readArtifact(ctx, v.objects, r)
	if err != nil {
		return 0, err
	}
	var n membershipNode
	if Decode(body, &n) != nil || n.Version != 1 || n.History != v.history || n.Epoch != v.epoch || n.Level < 0 || n.Level > maxMembershipLevel || (parentLevel >= 0 && n.Level != parentLevel-1) || n.Count > maxInteger {
		return 0, s.ErrInvalid
	}
	count := uint64(0)
	if n.Level == 0 {
		if len(n.Children) != 0 || len(n.Members) > membershipFanout {
			return 0, s.ErrInvalid
		}
		for _, m := range n.Members {
			if m.ID <= v.last || !isHex(m.ID, 64) {
				return 0, s.ErrInvalid
			}
			e, err := verifiedEvent(ctx, v.objects, m, v.history, v.epoch)
			if err != nil {
				return 0, err
			}
			v.chain, err = s.RecoveryDigest(v.chain, e)
			if err != nil {
				return 0, err
			}
			v.last = m.ID
			count++
		}
	} else {
		if len(n.Members) != 0 || len(n.Children) == 0 || len(n.Children) > membershipFanout {
			return 0, s.ErrInvalid
		}
		for _, child := range n.Children {
			c, err := v.walk(ctx, child, n.Level)
			if err != nil {
				return 0, err
			}
			if c == 0 || c > maxInteger-count {
				return 0, s.ErrInvalid
			}
			count += c
		}
	}
	if count != n.Count {
		return 0, s.ErrInvalid
	}
	return count, nil
}
func VerifyEvent(ctx context.Context, objects ImmutableStore, m Member, h string, epoch uint64) error {
	_, err := verifiedEvent(ctx, objects, m, h, epoch)
	return err
}
func verifiedEvent(ctx context.Context, objects ImmutableStore, m Member, h string, epoch uint64) (s.OutboxEvent, error) {
	var zero s.OutboxEvent
	if !isHex(h, 32) || !isHex(m.ID, 64) || epoch > maxInteger {
		return zero, s.ErrInvalid
	}
	e := s.OutboxEvent{ID: m.ID, HistoryID: h, Epoch: epoch}
	body, err := readArtifact(ctx, objects, AttachmentReference{Key: eventRoot(e) + "descriptor.json", Reference: m.Descriptor})
	if err != nil {
		return zero, err
	}
	var d Descriptor
	if Decode(body, &d) != nil || d.Version != 1 || d.Source.Validate() != nil || d.ID != m.ID || d.History != h || d.Epoch != epoch || d.Source.ID != d.ID || d.Source.HistoryID != h || d.Source.Epoch != epoch || d.Action != d.Source.Action || d.Entity != d.Source.Entity || d.Mutation != d.Source.OpID || !isHex(d.Logical, 32) || len(d.Pages) == 0 {
		return zero, s.ErrInvalid
	}
	seen := map[string]bool{}
	for i, r := range d.Pages {
		body, err := readArtifact(ctx, objects, AttachmentReference{Key: eventRoot(e) + "pages/" + strconv.Itoa(i) + ".json", Reference: r})
		if err != nil {
			return zero, err
		}
		var p EventPage
		if Decode(body, &p) != nil || p.Version != 1 || p.Event != m.ID || p.History != h || p.Epoch != epoch || p.Number != i || len(p.Effects) == 0 {
			return zero, s.ErrInvalid
		}
		for _, effect := range p.Effects {
			if effect.Key == "" || effect.Revision == 0 || effect.Revision > maxInteger || seen[effect.Key] || effect.Deleted && effect.Value != "" {
				return zero, s.ErrInvalid
			}
			seen[effect.Key] = true
		}
	}
	for _, dep := range d.Dependencies {
		if err := VerifyAttachment(ctx, objects, dep, d.Source); err != nil {
			return zero, err
		}
	}
	// Source equality is also checked against the native Closed fingerprint.
	return d.Source, nil
}
