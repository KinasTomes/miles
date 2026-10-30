package dynamodb

import (
	"context"
	"reflect"
	"sort"
	"time"

	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

var _ s.IdentityStore = (*Store)(nil)

type adminInstallation struct {
	TokenID, UserID string
	CreatedAt       time.Time
}

func decodeItem[T any](item Item) (T, error) {
	var v T
	err := unmarshalNative(&types.AttributeValueMemberM{Value: item}, reflect.ValueOf(&v).Elem())
	return v, err
}

// Users items merge the user and quota projections; a quota-only item is no user.
func userItem(item Item) bool { return item["id"] != nil }

func (m *Store) GetUser(ctx context.Context, id string) (s.User, error) {
	var out s.User
	err := m.view(ctx, func(t *transaction) error {
		row, err := t.load(key("users", id))
		if err != nil {
			return err
		}
		if row.after == nil || !userItem(row.after) {
			return s.ErrNotFound
		}
		out, err = decodeItem[s.User](row.after)
		if err == nil && out.ID != id {
			err = s.ErrUnavailable
		}
		return err
	})
	return out, err
}

func (m *Store) GetGrant(ctx context.Context, bucketID, userID string) (s.Grant, error) {
	v, err := read[s.Grant](m, ctx, key("grants", bucketID, userID))
	if err == nil && (v.BucketID != bucketID || v.UserID != userID) {
		err = s.ErrUnavailable
	}
	return v, err
}

func (m *Store) GetCredentialSecurity(ctx context.Context, id string) (s.CredentialSecurity, error) {
	return read[s.CredentialSecurity](m, ctx, key("credential-security", id))
}

func (m *Store) GetAdminToken(ctx context.Context, id string) (s.AdminToken, error) {
	v, err := read[s.AdminToken](m, ctx, key("admin-tokens", id))
	if err == nil && v.ID != id {
		err = s.ErrUnavailable
	}
	return v, err
}

func (m *Store) ListUsers(ctx context.Context, r s.IdentityListRequest) (s.IdentityPage[s.User], error) {
	return scanIdentity(m, ctx, "Users", "user_id", r, func(item Item, v s.User) (string, bool) { return v.ID, true })
}

func (m *Store) ListCredentials(ctx context.Context, r s.IdentityListRequest) (s.IdentityPage[s.Credential], error) {
	return scanIdentity(m, ctx, "Credentials", "access_key_id", r, func(_ Item, v s.Credential) (string, bool) {
		return v.ID, r.UserID == "" || v.UserID == r.UserID
	})
}

func (m *Store) ListAdminTokens(ctx context.Context, r s.IdentityListRequest) (s.IdentityPage[s.AdminToken], error) {
	return scanIdentity(m, ctx, "AdminTokens", "token_id", r, func(_ Item, v s.AdminToken) (string, bool) {
		return v.ID, r.UserID == "" || v.UserID == r.UserID
	})
}

// scanIdentity pages a whole-table consistent Scan in key byte order.
// ponytail: full scan, filter and sort in memory per page; admin tables are
// operator-scale. Add a user_id GSI / sorted index when they grow large.
func scanIdentity[T any](m *Store, ctx context.Context, table, pk string, r s.IdentityListRequest, keep func(Item, T) (string, bool)) (s.IdentityPage[T], error) {
	if err := r.Validate(); err != nil {
		return s.IdentityPage[T]{}, err
	}
	type entry struct {
		id string
		v  T
	}
	var entries []entry
	input := &ddb.ScanInput{TableName: aws.String(m.tables[table]), ConsistentRead: aws.Bool(true)}
	for {
		if err := ctx.Err(); err != nil {
			return s.IdentityPage[T]{}, err
		}
		page, err := m.api.Scan(ctx, input)
		if err != nil {
			return s.IdentityPage[T]{}, backend(err)
		}
		if page == nil {
			return s.IdentityPage[T]{}, s.ErrUnavailable
		}
		for _, item := range page.Items {
			if table == "Users" && !userItem(item) {
				continue
			}
			v, err := decodeItem[T](item)
			if err != nil {
				return s.IdentityPage[T]{}, err
			}
			id, listed := keep(item, v)
			if k, ok := item[pk].(*types.AttributeValueMemberS); !ok || k.Value != id {
				return s.IdentityPage[T]{}, s.ErrUnavailable
			}
			if listed && id > r.After {
				entries = append(entries, entry{id, v})
			}
		}
		if len(page.LastEvaluatedKey) == 0 {
			break
		}
		input.ExclusiveStartKey = page.LastEvaluatedKey
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].id < entries[j].id })
	out := s.IdentityPage[T]{Items: []T{}}
	for i, e := range entries {
		if i == r.Limit {
			out.Next = entries[i-1].id
			break
		}
		out.Items = append(out.Items, e.v)
	}
	return out, nil
}

// ListGrants queries one bucket partition; user_id sort order is byte order.
func (m *Store) ListGrants(ctx context.Context, bucketID string, r s.IdentityListRequest) (s.IdentityPage[s.Grant], error) {
	out := s.IdentityPage[s.Grant]{Items: []s.Grant{}}
	if err := r.Validate(); err != nil {
		return out, err
	}
	if bucketID == "" {
		return out, s.ErrInvalid
	}
	input := &ddb.QueryInput{TableName: aws.String(m.tables["BucketGrants"]), ConsistentRead: aws.Bool(true),
		KeyConditionExpression: aws.String("bucket_id = :b"), ExpressionAttributeValues: Item{":b": text(bucketID)}}
	if r.After != "" {
		input.KeyConditionExpression = aws.String("bucket_id = :b AND user_id > :a")
		input.ExpressionAttributeValues[":a"] = text(r.After)
	}
	for {
		if err := ctx.Err(); err != nil {
			return s.IdentityPage[s.Grant]{}, err
		}
		page, err := m.api.Query(ctx, input)
		if err != nil {
			return s.IdentityPage[s.Grant]{}, backend(err)
		}
		if page == nil {
			return s.IdentityPage[s.Grant]{}, s.ErrUnavailable
		}
		for _, item := range page.Items {
			g, err := decodeItem[s.Grant](item)
			if err != nil {
				return s.IdentityPage[s.Grant]{}, err
			}
			if k, ok := item["user_id"].(*types.AttributeValueMemberS); !ok || k.Value != g.UserID || g.BucketID != bucketID {
				return s.IdentityPage[s.Grant]{}, s.ErrUnavailable
			}
			if !g.Read && !g.Write {
				continue
			}
			if len(out.Items) == r.Limit {
				out.Next = out.Items[len(out.Items)-1].UserID
				return out, nil
			}
			out.Items = append(out.Items, g)
		}
		if len(page.LastEvaluatedKey) == 0 {
			return out, nil
		}
		input.ExclusiveStartKey = page.LastEvaluatedKey
	}
}

// BootstrapAdmin commits marker, user, token and missing policy in one native
// transaction. It emits no outbox event: there is no mutation context offline;
// the R2 epoch-0 export (W04-PHONG-04) owns publishing this installation state.
func (m *Store) BootstrapAdmin(ctx context.Context, b s.AdminBootstrap) error {
	now := m.now()
	if err := b.Validate(now); err != nil {
		return err
	}
	return m.update(ctx, func(t *transaction) error {
		for _, k := range []string{"admin-installation", key("admin-tokens", b.Token.ID)} {
			found, err := exists(t, k)
			if err != nil {
				return err
			}
			if found {
				return s.ErrConflict
			}
		}
		row, err := t.load(key("users", b.Token.UserID))
		if err != nil {
			return err
		}
		var user s.User
		if row.after == nil || !userItem(row.after) {
			user = s.User{ID: b.Token.UserID, Active: true, Revision: 1, DefaultDomain: "default"}
			if row.after == nil || row.after["owner_id"] == nil {
				if err := put(t, key("quotas", user.ID), s.LogicalQuota{OwnerID: user.ID, Limit: b.QuotaLimit}); err != nil {
					return err
				}
			}
		} else {
			if user, err = decodeItem[s.User](row.after); err != nil {
				return err
			}
			if user.ID != b.Token.UserID {
				return s.ErrUnavailable
			}
			if !user.Active {
				user.Active = true
				user.Revision++
			}
		}
		if err := put(t, key("users", user.ID), user); err != nil {
			return err
		}
		token := b.Token
		token.Revision = 1
		if err := put(t, key("admin-tokens", token.ID), token); err != nil {
			return err
		}
		current, err := optional[uint64](t, "policy-current")
		if err != nil {
			return err
		}
		if current == 0 {
			policy := b.Policy
			policy.Revision = 1
			if err := put(t, policyKey(1), policy); err != nil {
				return err
			}
			if err := put(t, "policy-current", uint64(1)); err != nil {
				return err
			}
		}
		return put(t, "admin-installation", adminInstallation{TokenID: token.ID, UserID: user.ID, CreatedAt: now})
	})
}
