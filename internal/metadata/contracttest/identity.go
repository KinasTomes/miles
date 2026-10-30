package contracttest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/config"
	s "github.com/KinasTomes/miles/internal/storage"
)

// RunIdentity covers IdentityStore reads, offline bootstrap and the Admin
// token actor rules of MutateControl/PutPolicy/PutBucketPolicy.
func RunIdentity(t *testing.T, factory MultipartFactory) {
	t.Run("bootstrap-lists-and-token-actors", func(t *testing.T) {
		ctx := context.Background()
		f := NewFixture()
		h := factory(t, f)
		h.SetStrict(true)
		ids := h.Metadata.(s.IdentityStore)
		p := h.Metadata.(s.ProducerStore)
		mutation := func(label string) s.MutationContext {
			op := fixtureOpID("identity-" + label)
			return s.MutationContext{HistoryID: fixtureHistoryID, LogicalOpID: op, MutationID: op, Writer: f.Lease.Fence}
		}
		token := func(id, user, scope string) s.AdminToken {
			return s.AdminToken{ID: id, UserID: user, Scope: scope, Hash: bytes.Repeat([]byte{7}, 32), Active: true, IssuedAt: f.Now, ExpiresAt: f.Now.Add(time.Hour)}
		}
		mutate := func(label string, r s.ControlMutation) error {
			r.Context = mutation(label)
			_, err := p.MutateControl(ctx, r)
			return err
		}

		// Offline bootstrap: once, atomically, without a writer fence.
		root := token(strings.Repeat("a", 32), "root", s.TokenScopeSystem)
		boot := s.AdminBootstrap{Token: root, QuotaLimit: 1 << 30, Policy: s.PolicySnapshot{Version: s.ModelVersion, Document: []byte(`{"enabled":true}`)}}
		invalid := boot
		invalid.Token.Scope = s.TokenScopeUser
		if err := ids.BootstrapAdmin(ctx, invalid); !errors.Is(err, s.ErrInvalid) {
			t.Fatal("user-scope bootstrap token", err)
		}
		if err := ids.BootstrapAdmin(ctx, boot); err != nil {
			t.Fatal("bootstrap", err)
		}
		again := boot
		again.Token.ID = strings.Repeat("d", 32)
		if err := ids.BootstrapAdmin(ctx, again); !errors.Is(err, s.ErrConflict) {
			t.Fatal("second bootstrap", err)
		}
		if _, err := ids.GetAdminToken(ctx, again.Token.ID); !errors.Is(err, s.ErrNotFound) {
			t.Fatal("conflicting bootstrap wrote token", err)
		}
		if u, err := ids.GetUser(ctx, "root"); err != nil || !u.Active || u.Revision != 1 || u.DefaultDomain != "default" {
			t.Fatal("bootstrap user", u, err)
		}
		if q, err := h.Metadata.GetQuota(ctx, "root"); err != nil || q.Limit != 1<<30 {
			t.Fatal("bootstrap quota", q, err)
		}
		if v, err := ids.GetAdminToken(ctx, root.ID); err != nil || v.Revision != 1 || !v.IssuedAt.Equal(f.Now) || v.Scope != s.TokenScopeSystem {
			t.Fatal("bootstrap token", v, err)
		}
		if v, err := h.Metadata.GetPolicy(ctx, 0); err != nil || v.Revision != 1 || string(v.Document) != `{"enabled":true}` {
			t.Fatal("bootstrap policy", v, err)
		}
		if _, err := ids.GetUser(ctx, "missing"); !errors.Is(err, s.ErrNotFound) {
			t.Fatal("missing user", err)
		}

		// Tokens for the owner and a non-owner; the system token is the actor.
		owner := token(strings.Repeat("b", 32), "owner", s.TokenScopeUser)
		writer := token(strings.Repeat("c", 32), "writer", s.TokenScopeUser)
		spare := token(strings.Repeat("e", 32), "owner", s.TokenScopeUser)
		for _, v := range []s.AdminToken{owner, writer, spare} {
			if err := mutate("token-"+v.ID, s.ControlMutation{AdminTokenID: root.ID, Entity: "AdminTokens", ID: v.ID, Token: v}); err != nil {
				t.Fatal("system token creates token", err)
			}
		}
		bad := token(strings.Repeat("f", 32), "owner", "owner")
		if err := mutate("token-bad-scope", s.ControlMutation{AdminTokenID: root.ID, Entity: "AdminTokens", ID: bad.ID, Token: bad}); !errors.Is(err, s.ErrInvalid) {
			t.Fatal("unknown token scope", err)
		}
		if err := mutate("both-actors", s.ControlMutation{AdminTokenID: root.ID, CredentialID: "admin-key", Entity: "AdminTokens", ID: bad.ID, Token: bad}); !errors.Is(err, s.ErrInvalid) {
			t.Fatal("two actors", err)
		}
		if err := mutate("user-token-users", s.ControlMutation{AdminTokenID: owner.ID, Entity: "Users", ID: "owner", User: s.User{ID: "owner", Active: true}, ExpectedRevision: 1}); !errors.Is(err, s.ErrAccessDenied) {
			t.Fatal("non-admin token edits users", err)
		}
		if err := mutate("user-token-self-elevate", s.ControlMutation{AdminTokenID: owner.ID, Entity: "AdminTokens", ID: bad.ID, Token: token(bad.ID, "owner", s.TokenScopeSystem)}); !errors.Is(err, s.ErrAccessDenied) {
			t.Fatal("non-admin token mints token", err)
		}

		// Grants: owner token allowed on its bucket, non-owner denied, owner row invalid.
		grant := func(user string, read, write bool) s.ControlMutation {
			return s.ControlMutation{Entity: "BucketGrants", ParentID: f.Bucket.ID, ID: user, Bucket: s.Bucket{Name: f.Bucket.Name}, Grant: s.Grant{BucketID: f.Bucket.ID, UserID: user, Read: read, Write: write}}
		}
		r := grant("admin", true, false)
		r.AdminTokenID = owner.ID
		if err := mutate("owner-grant", r); err != nil {
			t.Fatal("owner token grant", err)
		}
		r = grant("root", true, false)
		r.AdminTokenID = writer.ID
		if err := mutate("writer-grant", r); !errors.Is(err, s.ErrAccessDenied) {
			t.Fatal("non-owner token grant", err)
		}
		r = grant("owner", true, true)
		r.AdminTokenID = root.ID
		if err := mutate("grant-owner", r); !errors.Is(err, s.ErrInvalid) {
			t.Fatal("grant to owner", err)
		}
		r = grant("writer", false, false)
		r.AdminTokenID, r.Deleted, r.ExpectedRevision = owner.ID, true, 1
		if err := mutate("owner-revoke-grant", r); err != nil {
			t.Fatal("owner token revokes grant", err)
		}
		if g, err := ids.GetGrant(ctx, f.Bucket.ID, "writer"); err != nil || g.Read || g.Write || g.Revision != 2 {
			t.Fatal("tombstone", g, err)
		}
		page, err := ids.ListGrants(ctx, f.Bucket.ID, s.IdentityListRequest{Limit: 10})
		if err != nil || len(page.Items) != 1 || page.Items[0].UserID != "admin" || page.Next != "" {
			t.Fatal("grant list skips tombstone", page, err)
		}

		// Credentials: debug requires system admin; flags and CreatedAt are immutable.
		security := s.CredentialSecurity{Version: 1, KeyID: "kms-key", Ciphertext: bytes.Repeat([]byte{1}, 32)}
		credential := func(id string, admin, debug bool) s.ControlMutation {
			return s.ControlMutation{AdminTokenID: root.ID, Entity: "Credentials", ID: id, Credential: s.Credential{ID: id, UserID: "owner", Active: true, SystemAdmin: admin, DebugAllowed: debug}, Security: security}
		}
		if err := mutate("debug-no-admin", credential("DEBUGKEY000000000000", false, true)); !errors.Is(err, s.ErrInvalid) {
			t.Fatal("debug_allowed without system_admin", err)
		}
		if err := mutate("debug-admin", credential("DEBUGKEY000000000001", true, true)); err != nil {
			t.Fatal(err)
		}
		changed := credential("DEBUGKEY000000000001", false, false)
		changed.ExpectedRevision = 1
		if err := mutate("credential-flags", changed); !errors.Is(err, s.ErrInvalid) {
			t.Fatal("credential flags changed", err)
		}
		if err := mutate("owner-token-credential", s.ControlMutation{AdminTokenID: owner.ID, Entity: "Credentials", ID: "OWNERKEY000000000000", Credential: s.Credential{ID: "OWNERKEY000000000000", UserID: "owner", Active: true}, Security: security}); !errors.Is(err, s.ErrAccessDenied) {
			t.Fatal("non-admin token creates credential", err)
		}
		revoke := credential("DEBUGKEY000000000001", true, true)
		revoke.ExpectedRevision, revoke.Deleted = 1, true
		if err := mutate("credential-revoke", revoke); err != nil {
			t.Fatal(err)
		}
		creds, err := ids.ListCredentials(ctx, s.IdentityListRequest{UserID: "owner", Limit: 10})
		if err != nil || len(creds.Items) != 2 || creds.Items[0].ID != "DEBUGKEY000000000001" || creds.Items[1].ID != "owner-key" {
			t.Fatal("credential filter/order", creds, err)
		}
		if c := creds.Items[0]; c.Active || !c.DebugAllowed || !c.CreatedAt.Equal(f.Now) || c.Revision != 2 {
			t.Fatal("revoked credential", c)
		}
		if sec, err := ids.GetCredentialSecurity(ctx, "DEBUGKEY000000000001"); err != nil || !bytes.Equal(sec.Ciphertext, security.Ciphertext) {
			t.Fatal("credential security", sec, err)
		}

		// Token revoke: only the token's own user (or an admin).
		del := func(id, actor string) s.ControlMutation {
			return s.ControlMutation{AdminTokenID: actor, Entity: "AdminTokens", ID: id, ExpectedRevision: 1, Deleted: true}
		}
		if err := mutate("writer-revokes-owner", del(owner.ID, writer.ID)); !errors.Is(err, s.ErrAccessDenied) {
			t.Fatal("foreign token revoke", err)
		}
		if err := mutate("owner-self-revoke", del(owner.ID, owner.ID)); err != nil {
			t.Fatal("self revoke", err)
		}
		r = grant("root", true, false)
		r.AdminTokenID = owner.ID
		if err := mutate("revoked-token-grant", r); !errors.Is(err, s.ErrAccessDenied) {
			t.Fatal("revoked token still acts", err)
		}
		tokens, err := ids.ListAdminTokens(ctx, s.IdentityListRequest{UserID: "owner", Limit: 1})
		if err != nil || len(tokens.Items) != 1 || tokens.Items[0].ID != owner.ID || tokens.Items[0].Active || tokens.Next != owner.ID {
			t.Fatal("token page", tokens, err)
		}
		tokens, err = ids.ListAdminTokens(ctx, s.IdentityListRequest{UserID: "owner", After: tokens.Next, Limit: 1})
		if err != nil || len(tokens.Items) != 1 || tokens.Items[0].ID != spare.ID || tokens.Next != "" {
			t.Fatal("token last page", tokens, err)
		}

		// Users paging is byte ordered and Next appears only before more items.
		var all []string
		for after := ""; ; {
			users, err := ids.ListUsers(ctx, s.IdentityListRequest{After: after, Limit: 2})
			if err != nil {
				t.Fatal(err)
			}
			for _, u := range users.Items {
				all = append(all, u.ID)
			}
			if users.Next == "" {
				break
			}
			if users.Next != all[len(all)-1] {
				t.Fatal("next is not the last key", users)
			}
			after = users.Next
		}
		if fmt.Sprint(all) != "[admin owner root writer]" {
			t.Fatal("user order", all)
		}
		if _, err := ids.ListUsers(ctx, s.IdentityListRequest{Limit: 1001}); !errors.Is(err, s.ErrInvalid) {
			t.Fatal("limit bound", err)
		}

		// Token actor for global and bucket policy writes.
		policy := func(label, actor string, expected uint64, doc string) (s.PolicySnapshot, error) {
			c := mutation(label)
			id, _ := s.EventID(c.HistoryID, c.MutationID, "SystemConfig", "entity_put")
			e := s.OutboxEvent{Version: 1, ID: id, HistoryID: c.HistoryID, Entity: "SystemConfig", EntityID: "global-policy", Action: "entity_put", Revision: expected + 1, OpID: c.MutationID, PayloadPointer: "backend", PayloadSHA256: fmt.Sprintf("%064x", 1)}
			return h.Metadata.PutPolicy(s.WithMutation(ctx, c), s.PolicyUpdate{Candidate: s.PolicySnapshot{Version: 1, Document: []byte(doc)}, ExpectedRevision: expected, AdminTokenID: actor, Writer: c.Writer, Event: &e})
		}
		if _, err := policy("policy-user-token", spare.ID, 1, `{"enabled":false}`); !errors.Is(err, s.ErrAccessDenied) {
			t.Fatal("user token global policy", err)
		}
		if v, err := policy("policy-root-token", root.ID, 1, `{"enabled":false}`); err != nil || v.Revision != 2 {
			t.Fatal("system token global policy", v, err)
		}
		if _, err := h.Metadata.PutPolicy(ctx, s.PolicyUpdate{Candidate: s.PolicySnapshot{Version: 1, Document: []byte(`{}`)}, ExpectedRevision: 2, Writer: f.Lease.Fence}); !errors.Is(err, s.ErrInvalid) {
			t.Fatal("policy without actor", err)
		}
		bucketPolicies := h.Metadata.(s.BucketPolicyStore)
		doc, _ := json.Marshal(struct {
			Cache config.CachePolicy `json:"cache"`
		}{config.DefaultCachePolicy()})
		bucketPolicy := func(label, actor, bucketID string) (s.PolicySnapshot, error) {
			c := mutation(label)
			return bucketPolicies.PutBucketPolicy(s.WithMutation(ctx, c), s.BucketPolicyUpdate{Bucket: f.Bucket.Name, BucketID: bucketID, AdminTokenID: actor, Writer: c.Writer, Candidate: s.PolicySnapshot{Version: 1, Document: doc}})
		}
		if _, err := bucketPolicy("bucket-writer-token", writer.ID, f.Bucket.ID); !errors.Is(err, s.ErrAccessDenied) {
			t.Fatal("non-owner token bucket policy", err)
		}
		if _, err := bucketPolicy("bucket-wrong-id", spare.ID, "bucket-other"); !errors.Is(err, s.ErrConflict) {
			t.Fatal("bucket id mismatch", err)
		}
		if v, err := bucketPolicy("bucket-owner-token", spare.ID, f.Bucket.ID); err != nil || v.Revision != 1 {
			t.Fatal("owner token bucket policy", v, err)
		}
	})
	// A limit below Used+Reserved would make every later reservation, even the
	// zero-byte one of DELETE, fail; the transaction must refuse it whole.
	t.Run("quota-limit-keeps-ledger-valid", func(t *testing.T) {
		ctx := context.Background()
		f := NewFixture()
		f.Quota.Used, f.Quota.Reserved = 6, 2 // populated ledger plus an active reservation
		h := factory(t, f)
		h.SetStrict(true)
		p := h.Metadata.(s.ProducerStore)
		set := func(label string, revision uint64, limit int64) (s.MutationContext, error) {
			op := fixtureOpID("quota-" + label)
			c := s.MutationContext{HistoryID: fixtureHistoryID, LogicalOpID: op, MutationID: op, Writer: f.Lease.Fence}
			_, err := p.MutateControl(ctx, s.ControlMutation{Context: c, CredentialID: "admin-key", Entity: "Users", ID: "owner", ExpectedRevision: revision,
				User: s.User{ID: "owner", Active: true, DefaultDomain: "default"}, SetQuota: true, QuotaLimit: limit})
			return c, err
		}
		for _, tc := range []struct {
			label string
			limit int64
		}{{"below-reserved", 7}, {"below-used", 5}, {"zero", 0}} {
			c, err := set(tc.label, 1, tc.limit)
			if !errors.Is(err, s.ErrQuotaExceeded) {
				t.Fatal(tc.label, err)
			}
			if _, err := p.GetMutation(ctx, c.MutationID); !errors.Is(err, s.ErrNotFound) {
				t.Fatal("refused limit left an outcome/event", tc.label, err)
			}
		}
		if q, err := h.Metadata.GetQuota(ctx, "owner"); err != nil || q.Limit != 8 || q.Used != 6 || q.Reserved != 2 {
			t.Fatal("refused limit changed the ledger", q, err)
		}
		if u, err := h.Metadata.(s.IdentityStore).GetUser(ctx, "owner"); err != nil || u.Revision != 1 {
			t.Fatal("refused limit changed the user", u, err)
		}
		if _, err := set("raise", 1, 9); err != nil {
			t.Fatal("raise", err)
		}
		if _, err := set("exact", 2, 8); err != nil {
			t.Fatal("limit equal to used+reserved", err)
		}
		if q, err := h.Metadata.GetQuota(ctx, "owner"); err != nil || q.Limit != 8 || q.Used != 6 || q.Reserved != 2 {
			t.Fatal("accepted limit", q, err)
		}
	})
}
