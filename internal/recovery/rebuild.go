package recovery

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/lifecycle"
	"github.com/KinasTomes/miles/internal/manifest"
	"github.com/KinasTomes/miles/internal/pack"
	"github.com/KinasTomes/miles/internal/recovery/format"
	s "github.com/KinasTomes/miles/internal/storage"
)

type Rebuilt struct {
	Records   []s.IndexRecord
	Chunks    map[s.ChunkKey]s.Chunk
	Packs     map[s.PackRef]s.PackInfo
	Live      map[s.ChunkKey]bool
	Heads     []s.ObjectHead
	Manifests map[string][]s.ManifestPage
}

func projection(e Effect) (string, string, json.RawMessage, error) {
	kind, id, ok := strings.Cut(e.Key, "/")
	if !ok {
		return "", "", nil, s.ErrInvalid
	}
	if kind == "PolicyHistory" {
		id, _, ok = strings.Cut(id, "/")
		if !ok {
			return "", "", nil, s.ErrInvalid
		}
		kind = "SystemConfig"
	}
	raw, err := hex.DecodeString(id)
	if err != nil || hex.EncodeToString(raw) != id {
		return "", "", nil, s.ErrInvalid
	}
	if e.Deleted {
		return kind, string(raw), nil, nil
	}
	body, err := base64.StdEncoding.Strict().DecodeString(e.Value)
	if err != nil {
		return "", "", nil, s.ErrInvalid
	}
	var p format.Projection
	if err = strictJSON(body, &p); err != nil || p.Version != 1 || p.Entity != kind {
		return "", "", nil, s.ErrInvalid
	}
	return kind, string(raw), p.Value, nil
}
func strictJSON(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if d.Decode(v) != nil || d.Decode(new(any)) != io.EOF {
		return s.ErrInvalid
	}
	return nil
}

// Rebuild materializes namespace and immutable dependencies, then derives quota
// and reference counters. Historical credential status remains in the snapshot;
// the installation records disable every recovered credential and bearer token.
func (w Checkpointer) Rebuild(ctx context.Context, root AttachmentReference) (Rebuilt, error) {
	var zero Rebuilt
	image, err := w.Load(ctx, root)
	if err != nil {
		return zero, err
	}
	out := Rebuilt{Chunks: map[s.ChunkKey]s.Chunk{}, Packs: map[s.PackRef]s.PackInfo{}, Live: map[s.ChunkKey]bool{}, Manifests: map[string][]s.ManifestPage{}}
	users := map[string]s.User{}
	buckets := map[string]s.Bucket{}
	quotas := map[string]s.LogicalQuota{}
	namespaces := map[string]s.BucketNamespace{}
	records := map[string]s.IndexRecord{}
	add := func(kind string, ids []string, v any) error {
		b, e := json.Marshal(v)
		if e != nil {
			return e
		}
		key := kind + "/" + strings.Join(ids, "\x00")
		if old, ok := records[key]; ok && !bytes.Equal(old.Body, b) {
			return s.ErrConflict
		}
		records[key] = s.IndexRecord{Kind: kind, IDs: ids, Body: b}
		return nil
	}
	// Lifecycle/protection facts have independent revision domains. Fold them
	// only after all core Packs projections have been loaded.
	var facts []func() error
	keys := make([]string, 0, len(image.Rows))
	for k := range image.Rows {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		row := image.Rows[key]
		kind, id, value, e := projection(row)
		if e != nil {
			return zero, e
		}
		if row.Deleted {
			continue
		}
		switch kind {
		case "Users":
			var v s.User
			if strictJSON(value, &v) != nil || v.ID != id || v.Revision != row.Revision {
				return zero, s.ErrInvalid
			}
			users[id] = v
			e = add("users", []string{id}, v)
		case "UsersQuota":
			var v struct {
				Owner string
				Limit int64
			}
			if strictJSON(value, &v) != nil || v.Owner != id || v.Limit < 0 {
				return zero, s.ErrInvalid
			}
			q := quotas[id]
			q.OwnerID = id
			q.Limit = v.Limit
			quotas[id] = q
		case "Buckets":
			var v s.Bucket
			if strictJSON(value, &v) != nil || v.Name != id || v.Revision != row.Revision {
				return zero, s.ErrInvalid
			}
			buckets[v.Name] = v
			e = add("buckets", []string{id}, v)
		case "BucketGrants":
			var v s.Grant
			if strictJSON(value, &v) != nil || v.BucketID+"/"+v.UserID != id || v.Revision != row.Revision {
				return zero, s.ErrInvalid
			}
			e = add("grants", []string{v.BucketID, v.UserID}, v)
		case "Credentials":
			var v struct {
				Credential s.Credential
				Security   s.CredentialSecurity
			}
			if strictJSON(value, &v) != nil || v.Credential.ID != id || v.Credential.Revision != row.Revision {
				return zero, s.ErrInvalid
			}
			v.Credential.Active = false
			e = add("credentials", []string{id}, v.Credential)
			if e == nil {
				e = add("credential-security", []string{id}, v.Security)
			}
		case "AdminTokens":
			var v s.AdminToken
			if strictJSON(value, &v) != nil || v.ID != id || v.Revision != row.Revision {
				return zero, s.ErrInvalid
			}
			v.Active = false
			e = add("admin-tokens", []string{id}, v)
		case "Objects", "RetainedManifest":
			var v s.ObjectHead
			if strictJSON(value, &v) != nil || v.ValidateManifestDescriptor() != nil || v.Revision != row.Revision {
				return zero, s.ErrInvalid
			}
			if kind == "Objects" {
				b, _ := json.Marshal(v.Object)
				if string(b) != id {
					return zero, s.ErrInvalid
				}
				out.Heads = append(out.Heads, v)
			}
		case "Chunks":
			var v s.Chunk
			if strictJSON(value, &v) != nil || v.Validate() != nil || v.Key.Domain+"#"+v.Key.Hash != id || v.LocationRevision != row.Revision {
				return zero, s.ErrInvalid
			}
			out.Chunks[v.Key] = v
		case "Packs":
			var v struct {
				Version  uint32
				Ref      s.PackRef
				Size     int64
				Class    s.StorageClass
				State    string
				Revision uint64
			}
			if strictJSON(value, &v) != nil || v.Version != 1 || v.Ref.ID+"/"+v.Ref.Generation != id || v.Size <= 0 || v.Size > pack.MaxPackBytes || v.Class.Validate() != nil || v.Revision != row.Revision {
				return zero, s.ErrInvalid
			}
			out.Packs[v.Ref] = s.PackInfo{Version: 1, Ref: v.Ref, Size: v.Size, Class: v.Class, State: v.State, StateRevision: v.Revision}
		case "PackProtection":
			var v struct {
				Version        uint32
				Ref            s.PackRef
				ProtectedUntil time.Time
			}
			if strictJSON(value, &v) != nil || v.Version != 1 || v.Ref.ID+"/"+v.Ref.Generation != id {
				return zero, s.ErrInvalid
			}
			facts = append(facts, func() error {
				p, ok := out.Packs[v.Ref]
				if !ok {
					return s.ErrInvalid
				}
				if p.ProtectedUntil.Before(v.ProtectedUntil) {
					p.ProtectedUntil = v.ProtectedUntil
				}
				out.Packs[v.Ref] = p
				return nil
			})
		case "PackLifecycleFacts":
			var v struct {
				Version                          uint32
				Ref                              s.PackRef
				RestoredUntil, MinRetentionUntil time.Time
			}
			if strictJSON(value, &v) != nil || v.Version != 1 || v.Ref.ID+"/"+v.Ref.Generation != id {
				return zero, s.ErrInvalid
			}
			facts = append(facts, func() error {
				p, ok := out.Packs[v.Ref]
				if !ok || row.Revision > p.StateRevision {
					return s.ErrInvalid
				}
				p.RestoredUntil = v.RestoredUntil
				p.MinRetentionUntil = v.MinRetentionUntil
				out.Packs[v.Ref] = p
				return nil
			})
		case "PackLifecycle":
			var v lifecycle.Action
			if strictJSON(value, &v) != nil || v.Version != 1 || v.ID != id || v.Revision != row.Revision {
				return zero, s.ErrInvalid
			}
			// Preserve reconciliation facts as inert records, never old fences.
			v.Fence = s.WriterFence{}
			v.Chunks = nil
			e = add("recovery-lifecycle", []string{id}, v)
			if v.ResultPack.Ref.ID != "" {
				dest := v.ResultPack
				dest.Writer = s.WriterFence{}
				facts = append(facts, func() error {
					p, ok := out.Packs[dest.Ref]
					if !ok || dest.StateRevision > p.StateRevision {
						out.Packs[dest.Ref] = dest
					} else {
						if p.ProtectedUntil.Before(dest.ProtectedUntil) {
							p.ProtectedUntil = dest.ProtectedUntil
						}
						out.Packs[dest.Ref] = p
					}
					return nil
				})
			}
		case "SystemConfig":
			var v s.PolicySnapshot
			if strictJSON(value, &v) != nil || v.Version != 1 || v.Revision != row.Revision || !json.Valid(v.Document) {
				return zero, s.ErrInvalid
			}
			if id == "global-policy" {
				validator, _ := config.NewPolicyValidator(config.DSGPolicyLimits())
				if _, err := validator.DecodeGlobal(v); err != nil {
					return zero, s.ErrInvalid
				}
				e = add("policies", []string{fmt.Sprintf("%016x", v.Revision)}, v)
				if e == nil && !strings.HasPrefix(key, "PolicyHistory/") {
					e = add("policy-current", nil, v.Revision)
				}
			} else if encoded, ok := strings.CutPrefix(id, "bucket-cache/"); ok {
				raw, err := hex.DecodeString(encoded)
				if err != nil {
					return zero, s.ErrInvalid
				}
				validator, _ := config.NewPolicyValidator(config.DSGPolicyLimits())
				if _, err := validator.DecodeBucket(string(raw), v.Revision, v.Document); err != nil {
					return zero, s.ErrInvalid
				}
				e = add("bucket-cache-history", []string{string(raw), fmt.Sprintf("%016x", v.Revision)}, v)
				if e == nil && !strings.HasPrefix(key, "PolicyHistory/") {
					e = add("bucket-cache-current", []string{string(raw)}, v.Revision)
				}
			} else {
				return zero, s.ErrInvalid
			}
		case "CompletedReceipt":
			var v s.CompletionReceipt
			if strictJSON(value, &v) != nil || v.Version != 1 || v.UploadID != id || v.ExpiresAt.IsZero() {
				return zero, s.ErrInvalid
			}
			e = add("full-receipt", []string{id}, v)
			if e == nil {
				e = add("receipts", []string{id}, s.CompletedReceipt{Version: v.Version, UploadID: v.UploadID, BucketID: v.Object.BucketID, Key: v.Object.Key, Initiator: v.Initiator, OpID: v.OpID, InputDigest: v.InputDigest, ManifestID: v.Head.ManifestID, ETag: v.Head.ETag, Checksum: v.Head.Checksum, ExpiresAt: v.ExpiresAt})
			}
		case "CompletedUpload", "Uploads":
			var v s.MultipartUpload
			if strictJSON(value, &v) != nil || v.ID != id || v.Revision != row.Revision || kind == "CompletedUpload" && v.State != "Completed" {
				return zero, s.ErrInvalid
			}
			if v.State != "Completed" && v.State != "Aborted" {
				v.State = "Aborted"
				v.Revision++
				v.Prepared = s.ObjectHead{}
			}
			v.Writer = s.WriterFence{}
			v.ClosedWriter = s.WriterFence{}
			v.LeaseUntil = time.Time{}
			v.Publishers = 0
			v.PublishedBytes = 0
			v.ProvisionalBytes = 0
			e = add("upload-record", []string{id}, v)
			if e == nil {
				e = add("uploads", []string{id}, s.Completion{UploadID: v.ID, BucketID: v.Object.BucketID, Key: v.Object.Key, Initiator: v.Initiator, Revision: v.Revision, Fence: v.Fence, State: v.State})
			}
		case "QuotaReservations", "ProducerScope", "UsersUsage":
			// Old ledgers, ownership and usage are not execution authority in a
			// new installation. Unjournaled active uploads are absent/aborted.
		default:
			return zero, fmt.Errorf("%w: unsupported recovery projection %s", s.ErrInvalid, kind)
		}
		if e != nil {
			return zero, e
		}
	}
	for _, f := range facts {
		if e := f(); e != nil {
			return zero, e
		}
	}
	if _, ok := records["policy-current/"]; !ok {
		return zero, fmt.Errorf("%w: missing current global policy", s.ErrInvalid)
	}
	for _, dep := range image.Dependencies {
		if e := walkAttachment(ctx, w.Objects, dep.Root, dep.Event, func(kind byte, owner string, n uint32, raw []byte) error {
			if kind == 1 {
				p, e := manifest.Decode(raw, 1, owner, n)
				if e != nil {
					return e
				}
				for _, prior := range out.Manifests[owner] {
					if prior.Number == n {
						a, _ := json.Marshal(prior)
						b, _ := json.Marshal(p)
						if !bytes.Equal(a, b) {
							return s.ErrConflict
						}
						return nil
					}
				}
				out.Manifests[owner] = append(out.Manifests[owner], p)
				return add("manifest-page", []string{owner, strconv.FormatUint(uint64(n), 10)}, p)
			}
			return add("completion-page", []string{strconv.Itoa(int(kind)), owner, strconv.FormatUint(uint64(n), 10)}, raw)
		}); e != nil {
			return zero, e
		}
	}
	for name, b := range buckets {
		if _, ok := users[b.OwnerID]; !ok {
			return zero, s.ErrInvalid
		}
		namespaces[name] = s.BucketNamespace{Version: 1}
	}
	refs := map[s.ChunkKey]int64{}
	for _, h := range out.Heads {
		b, ok := buckets[h.Object.Bucket]
		if !ok || b.ID != h.Object.BucketID || !b.Active {
			if h.Deleted {
				continue
			}
			return zero, s.ErrInvalid
		}
		if h.Domain != b.Domain {
			return zero, s.ErrInvalid
		}
		if e := add("heads", []string{h.Object.Bucket, h.Object.BucketID, h.Object.Key}, h); e != nil {
			return zero, e
		}
		if h.Deleted {
			continue
		}
		if h.Size < 0 || h.Size > s.MaxObjectBytes {
			return zero, s.ErrInvalid
		}
		q, ok := quotas[b.OwnerID]
		if !ok {
			return zero, s.ErrInvalid
		}
		if q.Used > int64(s.MaxPolicyRevision)-h.Size {
			return zero, s.ErrInvalid
		}
		q.Used += h.Size
		quotas[b.OwnerID] = q
		n := namespaces[b.Name]
		n.LiveObjects++
		namespaces[b.Name] = n
		pages := out.Manifests[h.ManifestID]
		sort.Slice(pages, func(i, j int) bool { return pages[i].Number < pages[j].Number })
		if uint32(len(pages)) != h.PageCount {
			return zero, s.ErrInvalid
		}
		offset := int64(0)
		count := uint64(0)
		for i, p := range pages {
			if p.Number != uint32(i) || p.Domain != h.Domain {
				return zero, s.ErrInvalid
			}
			if h.PageIndex[i] != uint64(offset) {
				return zero, s.ErrInvalid
			}
			for _, r := range p.Chunks {
				c, ok := out.Chunks[r.Key]
				if !ok || c.RawBytes != r.RawBytes || r.Key.Domain != h.Domain || r.LogicalOffset != offset {
					return zero, s.ErrInvalid
				}
				offset += r.RawBytes
				count++
				out.Live[r.Key] = true
				refs[r.Key]++
			}
		}
		if offset != h.Size || count != h.ChunkCount {
			return zero, s.ErrInvalid
		}
	}
	for id := range users {
		q, ok := quotas[id]
		if !ok {
			return zero, fmt.Errorf("%w: missing quota baseline", s.ErrInvalid)
		}
		if q.Used > q.Limit {
			return zero, s.ErrQuotaExceeded
		}
		if e := add("quotas", []string{id}, q); e != nil {
			return zero, e
		}
	}
	for name, n := range namespaces {
		if e := add("bucket-namespace", []string{name}, n); e != nil {
			return zero, e
		}
	}
	for key, c := range out.Chunks {
		p, ok := out.Packs[c.Location.Pack]
		if !ok || c.Location.Range.Validate(p.Size) != nil {
			return zero, s.ErrInvalid
		}
		if e := add("chunks", []string{key.Domain, key.Hash}, c); e != nil {
			return zero, e
		}
		if e := add("recovery-refcount", []string{key.Domain, key.Hash}, refs[key]); e != nil {
			return zero, e
		}
	}
	for ref, p := range out.Packs {
		p.Writer = s.WriterFence{}
		if e := add("packs", []string{ref.ID, ref.Generation}, p); e != nil {
			return zero, e
		}
	}
	keys = keys[:0]
	for k := range records {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out.Records = append(out.Records, records[k])
	}
	return out, nil
}

// VerifyPayload checks exact canonical ranges, CRC, codec and content hash.
// GLACIER may return ErrCold: accepting restore is not verified readability.
func (r Rebuilt) VerifyPayload(ctx context.Context, objects s.PackStore) error {
	return r.verifyPayload(ctx, objects, false)
}
func (r Rebuilt) verifyPayload(ctx context.Context, objects s.PackStore, allowArchive bool) error {
	if objects == nil {
		return s.ErrInvalid
	}
	for key := range r.Live {
		c := r.Chunks[key]
		if c.RawBytes > 4<<20 || c.Location.Range.Length > pack.MaxPackBytes {
			return s.ErrInvalid
		}
		physical, e := objects.HeadPack(ctx, c.Location.Pack)
		if e != nil {
			return e
		}
		if physical.Ref != c.Location.Pack || c.Location.Range.Validate(physical.Size) != nil || physical.Class.Validate() != nil {
			return s.ErrInvalid
		}
		reader, e := objects.RangeGet(ctx, c.Location.Pack, c.Location.Range)
		if e != nil {
			if allowArchive && physical.Class == s.Glacier && errors.Is(e, s.ErrCold) {
				continue
			}
			return e
		}
		if reader == nil {
			return s.ErrUnavailable
		}
		body, e := io.ReadAll(io.LimitReader(reader, c.Location.Range.Length+1))
		closed := reader.Close()
		if e != nil || closed != nil {
			return errors.Join(e, closed)
		}
		entry := pack.Entry{Key: key, Codec: c.Location.Codec, Offset: uint64(c.Location.Range.Offset), StoredLen: uint32(c.Location.Range.Length), RawLength: uint32(c.RawBytes), CRC32C: c.Location.CRC32C}
		if _, e = pack.ReadBlob(entry, body); e != nil {
			return e
		}
	}
	return nil
}
