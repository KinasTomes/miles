package operations

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	recovery "github.com/KinasTomes/miles/internal/recovery/format"
	s "github.com/KinasTomes/miles/internal/storage"
)

func digestBytes(body []byte) string { sum := sha256.Sum256(body); return hex.EncodeToString(sum[:]) }
func scopeKey(entity, identity string) string {
	return entity + "/" + hex.EncodeToString([]byte(identity))
}

// Produce persists the complete typed projection and event/outcome in the SAME
// native transaction as the business mutation. Payload is schema-tagged JSON
// inside Base64 effect values, so Unicode never gets lost in the existing ASCII
// journal envelope. This does not claim generic RFC8785 number conformance.
func Produce(t Tx, c s.MutationContext, entity, action, identity string, revision uint64, input []byte, effects []recovery.Effect, value any, dependencies ...recovery.PayloadDependency) (s.MutationResult, error) {
	var zero s.MutationResult
	if c.Entity != "" && (c.Entity != entity || c.Identity != identity) {
		return zero, s.ErrConflict
	}
	c.Entity = entity
	c.Identity = identity
	if !id(c.HistoryID) || !id(c.MutationID) || !id(c.LogicalOpID) || c.Epoch > s.MaxPolicyRevision || revision == 0 || revision > s.MaxPolicyRevision {
		return zero, s.ErrInvalid
	}
	if err := t.Admit(c); err != nil {
		return zero, err
	}
	scope := scopeKey(entity, identity)
	old, err := get[s.MutationResult](t, "producer-outcome", c.MutationID)
	if err == nil {
		if old.HistoryID != c.HistoryID || old.LogicalOpID != c.LogicalOpID || old.Epoch != c.Epoch || old.Scope != scope || old.InputSHA256 != digestBytes(input) {
			return zero, s.ErrConflict
		}
		return old, nil
	}
	if !errors.Is(err, s.ErrNotFound) {
		return zero, err
	}
	eventID, err := s.EventID(c.HistoryID, c.MutationID, entity, action)
	if err != nil {
		return zero, err
	}
	e := s.OutboxEvent{Version: 1, ID: eventID, HistoryID: c.HistoryID, Epoch: c.Epoch, Entity: entity, Action: action, EntityID: identity, Revision: revision, OpID: c.MutationID}
	scopeEffect, err := Projection("ProducerScope", c.MutationID, 1, recovery.ProducerScope{History: c.HistoryID, Logical: c.LogicalOpID, Mutation: c.MutationID, Entity: entity, Identity: identity, Action: action, Epoch: c.Epoch}, false)
	if err != nil {
		return zero, err
	}
	effects = append(effects, scopeEffect)
	if entity == "Objects" {
		head, ok := value.(s.ObjectHead)
		if !ok {
			return zero, s.ErrInvalid
		}
		e.EntityID = ""
		e.Object = head.Object
	}
	page := recovery.EventPage{Version: 1, Event: e.ID, History: e.HistoryID, Epoch: e.Epoch, Effects: effects}
	body, err := recovery.Encode(recovery.Input{Pages: []recovery.EventPage{page}, Dependencies: dependencies})
	if err != nil {
		return zero, err
	}
	e.PayloadPointer = "producer/" + c.HistoryID + "/" + c.MutationID + "/" + digestBytes(body)
	e.PayloadSHA256 = digestBytes(body)
	if e.Validate() != nil {
		return zero, s.ErrInvalid
	}
	if err = ImmutablePayload(t, e.PayloadPointer, body); err != nil {
		return zero, err
	}
	if _, err := get[s.OutboxEvent](t, "events", e.ID); err == nil {
		return zero, s.ErrConflict
	} else if !errors.Is(err, s.ErrNotFound) {
		return zero, err
	}
	resultBytes, err := json.Marshal(value)
	if err != nil {
		return zero, s.ErrInvalid
	}
	out := s.MutationResult{Version: 1, Scope: scope, InputSHA256: digestBytes(input), Event: e, Value: resultBytes, HistoryID: c.HistoryID, LogicalOpID: c.LogicalOpID, Epoch: c.Epoch}
	if err = put(t, "events", e, e.ID); err != nil {
		return zero, err
	}
	if err = put(t, "producer-outcome", out, c.MutationID); err != nil {
		return zero, err
	}
	return out, nil
}
func ImmutablePayload(t Tx, key string, body []byte) error {
	if key == "" || len(body) == 0 || len(body) > recovery.MaxMetadataBytes {
		return s.ErrInvalid
	}
	old, err := get[[]byte](t, "payload", key)
	if err == nil {
		if !bytes.Equal(old, body) {
			return s.ErrConflict
		}
		return nil
	}
	if !errors.Is(err, s.ErrNotFound) {
		return err
	}
	return put(t, "payload", bytes.Clone(body), key)
}
func Projection(entity, identity string, revision uint64, value any, deleted bool) (recovery.Effect, error) {
	if !recovery.ValidStrings(value) {
		return recovery.Effect{}, s.ErrInvalid
	}
	body, err := json.Marshal(struct {
		Version uint32
		Entity  string
		Value   any
	}{1, entity, value})
	if err != nil {
		return recovery.Effect{}, s.ErrInvalid
	}
	e := recovery.Effect{Key: scopeKey(entity, identity), Revision: revision, Deleted: deleted}
	if !deleted {
		e.Value = base64.StdEncoding.EncodeToString(body)
	}
	return e, nil
}
func (m Multipart) Mutate(ctx context.Context, r s.ControlMutation) (s.MutationResult, error) {
	var result s.MutationResult
	if r.ID == "" || len(r.ID) > 1024 || r.ExpectedRevision >= s.MaxPolicyRevision || r.Deleted && r.ExpectedRevision == 0 {
		return result, s.ErrInvalid
	}
	action := "entity_put"
	if r.Deleted {
		action = "entity_delete"
	}
	copy := r
	copy.Context = s.MutationContext{}
	input, _ := json.Marshal(copy)
	err := m.Update(ctx, func(t Tx) error {
		if err := t.Admit(r.Context); err != nil {
			return err
		}
		if (r.AdminTokenID == "") == (r.CredentialID == "") {
			return s.ErrInvalid
		}
		actorID, admin := "", true
		if r.AdminTokenID != "" {
			token, err := TokenActor(t, m.Now(), r.AdminTokenID)
			if err != nil {
				return err
			}
			actorID, admin = token.UserID, token.Scope == s.TokenScopeSystem
		} else {
			credential, err := get[s.Credential](t, "credentials", r.CredentialID)
			if err != nil {
				return err
			}
			if !credential.Active || !credential.SystemAdmin {
				return s.ErrAccessDenied
			}
			user, err := get[s.User](t, "users", credential.UserID)
			if err != nil {
				return err
			}
			if !user.Active {
				return s.ErrAccessDenied
			}
		}
		// Non-admin tokens may only manage grants on owned buckets and revoke
		// their own tokens; both are rechecked against the stored records below.
		if !admin && r.Entity != "BucketGrants" && (r.Entity != "AdminTokens" || !r.Deleted) {
			return s.ErrAccessDenied
		}
		identity := r.ID
		if r.Entity == "BucketGrants" {
			identity = r.ParentID + "/" + r.ID
		}
		oldOutcome, e := get[s.MutationResult](t, "producer-outcome", r.Context.MutationID)
		if e == nil {
			if oldOutcome.HistoryID != r.Context.HistoryID || oldOutcome.LogicalOpID != r.Context.LogicalOpID || oldOutcome.Epoch != r.Context.Epoch || oldOutcome.Scope != scopeKey(r.Entity, identity) || oldOutcome.InputSHA256 != digestBytes(input) {
				return s.ErrConflict
			}
			result = oldOutcome
			return nil
		}
		if !errors.Is(e, s.ErrNotFound) {
			return e
		}
		revision := r.ExpectedRevision + 1
		var value, previous any
		forceChange := false
		var effects []recovery.Effect
		read := func(kind string, target any, ids ...string) error {
			e := t.Get(kind, ids, target)
			if errors.Is(e, s.ErrNotFound) && r.ExpectedRevision == 0 {
				return nil
			}
			return e
		}
		var writes []func() error
		write := func(kind string, v any, ids ...string) error {
			copied := append([]string(nil), ids...)
			writes = append(writes, func() error { return put(t, kind, v, copied...) })
			return nil
		}
		switch r.Entity {
		case "Users":
			var old s.User
			if err := read("users", &old, r.ID); err != nil {
				return err
			}
			if old.Revision != r.ExpectedRevision {
				return s.ErrConflict
			}
			previous = old
			v := r.User
			if v.DefaultDomain == "" {
				v.DefaultDomain = "default"
			}
			if v.ID != r.ID || s.ValidateDomain(v.DefaultDomain) != nil {
				return s.ErrInvalid
			}
			v.Revision = revision
			if r.Deleted {
				v.Active = false
			}
			value = v
			if r.SetQuota {
				q, e := get[s.LogicalQuota](t, "quotas", r.ID)
				if errors.Is(e, s.ErrNotFound) {
					forceChange = true
					q = s.LogicalQuota{OwnerID: r.ID}
				} else if e != nil {
					return e
				}
				if r.QuotaLimit < 0 {
					return s.ErrInvalid
				}
				if q.Limit != r.QuotaLimit {
					forceChange = true
				}
				q.Limit = r.QuotaLimit
				if err := write("quotas", q, r.ID); err != nil {
					return err
				}
				effect, e := Projection("UsersQuota", r.ID, revision, struct {
					Owner string
					Limit int64
				}{r.ID, q.Limit}, false)
				if e != nil {
					return e
				}
				effects = append(effects, effect)
			}
			if err := write("users", v, r.ID); err != nil {
				return err
			}
		case "Buckets":
			var old s.Bucket
			if err := read("buckets", &old, r.ID); err != nil {
				return err
			}
			if old.Revision != r.ExpectedRevision {
				return s.ErrConflict
			}
			previous = old
			v := r.Bucket
			if v.Name != r.ID || (s.ObjectKey{Bucket: v.Name, BucketID: v.ID, Key: "x"}).Validate() != nil || v.OwnerID == "" || s.ValidateDomain(v.Domain) != nil {
				return s.ErrInvalid
			}
			if r.ExpectedRevision > 0 && (v.ID != old.ID || v.OwnerID != old.OwnerID || v.Domain != old.Domain) {
				return s.ErrConflict
			}
			v.Revision = revision

			if r.ExpectedRevision > 0 {
				v.CreatedAt = old.CreatedAt
			}
			if r.ExpectedRevision == 0 {
				if r.Deleted {
					return s.ErrInvalid
				}
				v.CreatedAt = m.Now()
				if err := write("bucket-namespace", s.BucketNamespace{Version: 1}, v.Name); err != nil {
					return err
				}
			}
			if r.Deleted {
				n, e := get[s.BucketNamespace](t, "bucket-namespace", r.ID)
				if e != nil {
					return s.ErrUnavailable
				}
				if n.LiveObjects != 0 || n.Uploads != 0 {
					return s.ErrConflict
				}
				v.Active = false
			}
			value = v
			if err := write("buckets", v, r.ID); err != nil {
				return err
			}
		case "BucketGrants":
			if r.ParentID == "" {
				return s.ErrInvalid
			}
			parent, err := get[s.Bucket](t, "buckets", r.Bucket.Name)
			if err != nil {
				return err
			}
			if (!parent.Active && !r.Deleted) || parent.ID != r.ParentID {
				return s.ErrBucketNotFound
			}
			if !admin && parent.OwnerID != actorID {
				return s.ErrAccessDenied
			}
			// Owner rights are implicit; a grant row for the owner is never valid.
			if r.ID == parent.OwnerID && !r.Deleted {
				return s.ErrInvalid
			}
			if _, err := get[s.User](t, "users", r.ID); err != nil {
				return err
			}
			var old s.Grant
			if err := read("grants", &old, r.ParentID, r.ID); err != nil {
				return err
			}
			if old.Revision != r.ExpectedRevision {
				return s.ErrConflict
			}
			previous = old
			v := r.Grant
			if v.BucketID != r.ParentID || v.UserID != r.ID {
				return s.ErrInvalid
			}
			v.Revision = revision
			if r.Deleted {
				v.Read = false
				v.Write = false
			}
			value = v
			if err := write("grants", v, r.ParentID, r.ID); err != nil {
				return err
			}
		case "Credentials":
			var old s.Credential
			if err := read("credentials", &old, r.ID); err != nil {
				return err
			}
			if old.Revision != r.ExpectedRevision {
				return s.ErrConflict
			}
			previous = old
			v := r.Credential
			if v.ID != r.ID || v.UserID == "" || v.DebugAllowed && !v.SystemAdmin || r.ExpectedRevision > 0 && (v.UserID != old.UserID || v.SystemAdmin != old.SystemAdmin || v.DebugAllowed != old.DebugAllowed) {
				return s.ErrInvalid
			}
			v.Revision = revision
			v.CreatedAt = old.CreatedAt
			if r.ExpectedRevision == 0 {
				v.CreatedAt = m.Now()
			}
			if r.Deleted {
				v.Active = false
			}
			if _, err := get[s.User](t, "users", v.UserID); err != nil {
				return err
			}
			if r.ExpectedRevision > 0 && !old.Active && v.Active {
				return s.ErrInvalid
			}
			if r.Security.Version == 0 && r.ExpectedRevision > 0 {
				security, e := get[s.CredentialSecurity](t, "credential-security", r.ID)
				if e == nil {
					r.Security = security
				} else if !errors.Is(e, s.ErrNotFound) {
					return e
				}
			}
			if v.Active && (r.Security.Version != 1 || r.Security.KeyID == "" || len(r.Security.Ciphertext) < 16 || len(r.Security.Ciphertext) > 64<<10) {
				return s.ErrInvalid
			}
			priorSecurity, securityErr := get[s.CredentialSecurity](t, "credential-security", r.ID)
			if securityErr != nil && !errors.Is(securityErr, s.ErrNotFound) {
				return securityErr
			}
			previous = struct {
				Credential s.Credential
				Security   s.CredentialSecurity
			}{old, priorSecurity}

			value = struct {
				Credential s.Credential
				Security   s.CredentialSecurity
			}{v, r.Security}
			if err := write("credentials", v, r.ID); err != nil {
				return err
			}
			if err := write("credential-security", r.Security, r.ID); err != nil {
				return err
			}
		case "AdminTokens":
			var old s.AdminToken
			if err := read("admin-tokens", &old, r.ID); err != nil {
				return err
			}
			if old.Revision != r.ExpectedRevision {
				return s.ErrConflict
			}
			if !admin && old.UserID != actorID {
				return s.ErrAccessDenied
			}
			previous = old
			v := r.Token
			if r.Deleted && v.ID == "" {
				v = old
			}
			if v.ID != r.ID || v.UserID == "" || len(v.Hash) != 32 || v.Scope != s.TokenScopeSystem && v.Scope != s.TokenScopeUser || !r.Deleted && !m.Now().Before(v.ExpiresAt) {
				return s.ErrInvalid
			}
			v.Revision = revision
			if _, err := get[s.User](t, "users", v.UserID); err != nil {
				return err
			}
			if r.Deleted {
				v.Active = false
			}
			if r.ExpectedRevision > 0 && (!old.Active && v.Active || v.UserID != old.UserID || v.Scope != old.Scope || !bytes.Equal(v.Hash, old.Hash) || !v.ExpiresAt.Equal(old.ExpiresAt) || !v.IssuedAt.Equal(old.IssuedAt)) {
				return s.ErrInvalid
			}
			value = v
			if err := write("admin-tokens", v, r.ID); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%w: unsupported control entity %s", s.ErrInvalid, r.Entity)
		}

		if r.ExpectedRevision > 0 && !forceChange && equalRecord(previous, value) {
			if err := NoOp(t, s.WithMutation(ctx, r.Context), r.Entity, identity, copy, previous); err != nil {
				return err
			}
			stored, err := get[s.MutationResult](t, "producer-outcome", r.Context.MutationID)
			result = stored
			return err
		}
		for _, write := range writes {
			if err := write(); err != nil {
				return err
			}
		}
		effect, err := Projection(r.Entity, identity, revision, value, r.Deleted)
		if err != nil {
			return err
		}
		effects = append(effects, effect)
		result, err = Produce(t, r.Context, r.Entity, action, identity, revision, input, effects, value)
		return err
	})
	return result, err
}

func ValidatePayloadKey(key string) bool {
	return strings.HasPrefix(key, "producer/") && len(key) < 2048
}

func Namespace(t Tx, bucket string, objects, uploads int64, required bool) error {
	n, err := get[s.BucketNamespace](t, "bucket-namespace", bucket)
	if errors.Is(err, s.ErrNotFound) && !required {
		return nil
	}
	if err != nil {
		return s.ErrUnavailable
	}
	if n.Version != 1 || n.LiveObjects+objects < 0 || n.Uploads+uploads < 0 {
		return s.ErrUnavailable
	}
	n.LiveObjects += objects
	n.Uploads += uploads
	return put(t, "bucket-namespace", n, bucket)
}

// Legacy metadata ports remain available only in explicit development mode.
// Strict-mode callers must carry trusted admission for canonical/bucket writes.
func Journal(t Tx, ctx context.Context, required bool, writer s.WriterFence, entity, action, identity string, revision uint64, input, value any) (s.MutationResult, error) {
	c, ok := s.MutationFromContext(ctx)
	if !ok {
		if required {
			return s.MutationResult{}, s.ErrFenced
		}
		return s.MutationResult{}, nil
	}
	if c.Writer != writer {
		return s.MutationResult{}, s.ErrFenced
	}
	if entity == "Chunks" {
		c = s.ChildMutation(c, entity, identity, action)
	}
	if c.Entity != "" && (c.Entity != entity || c.Identity != identity) {
		return s.MutationResult{}, s.ErrConflict
	}
	c.Entity = entity
	c.Identity = identity
	b, err := json.Marshal(input)
	if err != nil {
		return s.MutationResult{}, s.ErrInvalid
	}
	effect, err := Projection(entity, identity, revision, value, false)
	if err != nil {
		return s.MutationResult{}, err
	}
	effects := []recovery.Effect{effect}
	if entity == "Chunks" {
		chunk, ok := value.(s.Chunk)
		if !ok {
			return s.MutationResult{}, s.ErrInvalid
		}
		p, err := get[s.PackInfo](t, "packs", chunk.Location.Pack.ID, chunk.Location.Pack.Generation)
		if err != nil {
			return s.MutationResult{}, err
		}
		if p.StateRevision == 0 {
			return s.MutationResult{}, s.ErrUnavailable
		}
		packEffect, err := Projection("Packs", p.Ref.ID+"/"+p.Ref.Generation, p.StateRevision, PackStateProjection(p), false)
		if err != nil {
			return s.MutationResult{}, err
		}
		effects = append(effects, packEffect)
	}
	return Produce(t, c, entity, action, identity, revision, b, effects, value)
}
func Replay(t Tx, ctx context.Context, entity, action, identity string, input ...any) (s.MutationResult, error) {
	c, ok := s.MutationFromContext(ctx)
	if !ok {
		return s.MutationResult{}, s.ErrNotFound
	}
	if entity == "Chunks" {
		c = s.ChildMutation(c, entity, identity, action)
	}
	if err := t.Admit(c); err != nil {
		return s.MutationResult{}, err
	}
	out, err := get[s.MutationResult](t, "producer-outcome", c.MutationID)
	if err != nil {
		return out, err
	}
	if out.HistoryID != c.HistoryID || out.LogicalOpID != c.LogicalOpID || out.Epoch != c.Epoch || out.Scope != scopeKey(entity, identity) {
		return out, s.ErrConflict
	}
	if len(input) > 0 {
		body, err := json.Marshal(input[0])
		if err != nil || out.InputSHA256 != digestBytes(body) {
			return out, s.ErrConflict
		}
	}
	return out, nil
}

func NoOp(t Tx, ctx context.Context, entity, identity string, input, value any) error {
	c, ok := s.MutationFromContext(ctx)
	if !ok {
		return nil
	}
	if c.Entity != "" && (c.Entity != entity || c.Identity != identity) {
		return s.ErrConflict
	}
	c.Entity = entity
	c.Identity = identity
	if err := t.Admit(c); err != nil {
		return err
	}
	body, err := json.Marshal(input)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	out := s.MutationResult{Version: 1, NoOp: true, Scope: scopeKey(entity, identity), InputSHA256: digestBytes(body), Value: encoded, HistoryID: c.HistoryID, LogicalOpID: c.LogicalOpID, Epoch: c.Epoch}
	return put(t, "producer-outcome", out, c.MutationID)
}

func ObjectEvent(t Tx, r s.CommitRequest, h, old s.ObjectHead, q s.LogicalQuota, ledger s.Reservation) (s.OutboxEvent, error) {
	if r.Producer == nil {
		return r.Event, nil
	}
	c := *r.Producer
	if c.Object != h.Object || c.Writer != r.Writer || c.MutationID != h.OpID || c.LogicalOpID != h.OpID || c.HistoryID != r.Event.HistoryID || c.Epoch != r.Event.Epoch {
		return s.OutboxEvent{}, s.ErrInvalid
	}
	identityBytes, _ := json.Marshal(h.Object)
	var effects []recovery.Effect
	var dependencies []recovery.PayloadDependency
	for _, head := range []s.ObjectHead{h, old} {
		if head.ManifestID != "" && head.PageCount > 0 {
			dep, err := t.Attach(1, head.ManifestID, head.Domain, head.PageCount, head.Size, "")
			if err != nil {
				return s.OutboxEvent{}, err
			}
			dependencies = append(dependencies, dep)
		}
	}
	add := func(entity, identity string, revision uint64, value any, deleted bool) error {
		effect, err := Projection(entity, identity, revision, value, deleted)
		effects = append(effects, effect)
		return err
	}
	if err := add("Objects", string(identityBytes), h.Revision, h, false); err != nil {
		return s.OutboxEvent{}, err
	}
	if err := add("QuotaReservations", ledger.ID, ledger.Revision, ledger, false); err != nil {
		return s.OutboxEvent{}, err
	}
	if err := add("UsersUsage", q.OwnerID, h.Revision, struct {
		Version     uint32
		Owner       string
		Limit, Used int64
	}{1, q.OwnerID, q.Limit, q.Used}, false); err != nil {
		return s.OutboxEvent{}, err
	}
	if old.ManifestID != "" {
		if err := add("RetainedManifest", old.ManifestID, old.Revision, old, false); err != nil {
			return s.OutboxEvent{}, err
		}
	}
	if r.Completion != nil {
		receipt, err := get[s.CompletionReceipt](t, "full-receipt", r.Completion.UploadID)
		if err != nil {
			return s.OutboxEvent{}, err
		}
		if err = add("CompletedReceipt", receipt.UploadID, h.Revision, receipt, false); err != nil {
			return s.OutboxEvent{}, err
		}
		u, err := get[s.MultipartUpload](t, "upload-record", r.Completion.UploadID)
		if err != nil {
			return s.OutboxEvent{}, err
		}
		if err = add("CompletedUpload", u.ID, u.Revision, u, false); err != nil {
			return s.OutboxEvent{}, err
		}
		for _, d := range []s.StreamDescriptor{u.Input, u.Snapshot} {
			dep, err := t.Attach(d.Kind, d.Owner, "", d.Pages, d.Length, d.SHA256)
			if err != nil {
				return s.OutboxEvent{}, err
			}
			dependencies = append(dependencies, dep)
		}
	}
	input, _ := json.Marshal(h)
	out, err := Produce(t, c, "Objects", "object_commit", string(identityBytes), h.Revision, input, effects, h, dependencies...)
	return out.Event, err
}

func Attach(t Tx, kind byte, owner, domain string, pages uint32, length int64, sum string, read func(uint32) ([]byte, error)) (recovery.PayloadDependency, error) {
	var zero recovery.PayloadDependency
	if pages == 0 || pages > 1024 {
		return zero, s.ErrInvalid
	}
	a := recovery.Attachment{Version: 1, Kind: kind, Owner: owner, Domain: domain, Length: length, SHA256: sum, Pages: [][]recovery.PageReference{}}
	for i := uint32(0); i < pages; i++ {
		body, err := read(i)
		if err != nil {
			return zero, err
		}
		if i%128 == 0 {
			a.Pages = append(a.Pages, []recovery.PageReference{})
		}
		group := len(a.Pages) - 1
		a.Pages[group] = append(a.Pages[group], recovery.PageReference{Length: len(body), SHA256: digestBytes(body)})
	}
	body, err := recovery.Encode(a)
	if err != nil {
		return zero, err
	}
	key := "producer/attachment/" + digestBytes(body)
	if err := ImmutablePayload(t, key, body); err != nil {
		return zero, err
	}
	return recovery.PayloadDependency{Key: key, Reference: recovery.Reference{Length: len(body), SHA256: digestBytes(body)}}, nil
}

func equalRecord(a, b any) bool {
	decode := func(v any) any {
		body, _ := json.Marshal(v)
		d := json.NewDecoder(bytes.NewReader(body))
		d.UseNumber()
		var x any
		_ = d.Decode(&x)
		return x
	}
	var strip func(any)
	strip = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			delete(x, "Revision")
			for _, child := range x {
				strip(child)
			}
		case []any:
			for _, child := range x {
				strip(child)
			}
		}
	}
	x, y := decode(a), decode(b)
	strip(x)
	strip(y)
	return reflect.DeepEqual(x, y)
}
