package dynamodb

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

type Item = map[string]types.AttributeValue

// Observe binds every fact read by an authority provider into the SAME metadata
// transaction. Providers must validate the shared R1 admission/closure protocol;
// a lease timestamp alone is not distributed dispatch permission.
type Observe func(Key) (Item, error)
type WriterAuthority interface {
	Check(context.Context, s.WriterFence, Observe) error
}
type Options struct {
	API              API
	Tables           map[string]string
	Now              func() time.Time
	Authority        WriterAuthority
	MaxAttempts      int
	RequireProducers bool
}
type Store struct {
	api                                    API
	tables                                 map[string]string
	now                                    func() time.Time
	authority                              WriterAuthority
	maxAttempts                            int
	requireProducers                       bool
	beforePublish, afterPublish, reconcile func() error // package-private fault seams
}

func New(options Options) (*Store, error) {
	if options.API == nil || options.Authority == nil {
		return nil, s.ErrInvalid
	}
	tables := map[string]string{}
	seen := map[string]bool{}
	for name := range Tables() {
		physical := options.Tables[name]
		if physical == "" || seen[physical] {
			return nil, s.ErrInvalid
		}
		seen[physical] = true
		tables[name] = physical
	}
	if len(options.Tables) != len(tables) {
		return nil, s.ErrInvalid
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.MaxAttempts == 0 {
		options.MaxAttempts = 8
	}
	if options.MaxAttempts < 1 || options.MaxAttempts > 32 {
		return nil, s.ErrInvalid
	}
	if options.RequireProducers {
		if _, ok := options.Authority.(ProducerAuthority); !ok {
			return nil, s.ErrInvalid
		}
	}
	return &Store{api: options.API, tables: tables, now: options.Now, authority: options.Authority, maxAttempts: options.MaxAttempts, requireProducers: options.RequireProducers}, nil
}

type outcome struct {
	Digest [32]byte
	Head   s.ObjectHead
}
type manifestSeal struct {
	OpID      string
	PageCount uint32
}

// Logical aliases below are implementation addresses, never DynamoDB keys.
// Physical keys are always resolved against the frozen table inventory.
func key(table string, parts ...string) string {
	for _, p := range parts {
		table += "/" + hex.EncodeToString([]byte(p))
	}
	return table
}
func chunkKey(k s.ChunkKey) string { return key("chunks", k.Domain, k.Hash) }
func packKey(k s.PackRef) string   { return key("packs", k.ID, k.Generation) }
func headKey(k s.ObjectKey) string { return key("heads", k.Bucket, k.BucketID, k.Key) }

type address struct {
	key         Key
	scope       string
	incarnation string
}

func resolve(k string) (address, error) {
	parts := strings.Split(k, "/")
	args := make([]string, len(parts)-1)
	for i, p := range parts[1:] {
		b, err := hex.DecodeString(p)
		if err != nil {
			return address{}, s.ErrInvalid
		}
		args[i] = string(b)
	}
	var a address
	var err error
	arg := func(i int) string {
		if i >= len(args) {
			return ""
		}
		return args[i]
	}
	switch parts[0] {
	case "buckets":
		a.key = stringKey("Buckets", "bucket", arg(0))
	case "users", "quotas":
		a.key = stringKey("Users", "user_id", arg(0))
		a.scope = parts[0]
	case "credentials":
		a.key = stringKey("Credentials", "access_key_id", arg(0))
	case "grants":
		a.key = stringKey("BucketGrants", "bucket_id", arg(0))
		a.key.Attributes["user_id"] = text(arg(1))
	case "chunks":
		a.key, err = ChunkKey(s.ChunkKey{Domain: arg(0), Hash: arg(1)})
	case "packs":
		a.key, err = PackKey(s.PackRef{ID: arg(0), Generation: arg(1)})
	case "pack-protection":
		a.key, err = PackKey(s.PackRef{ID: arg(0), Generation: arg(1)})
		a.scope = "protection_revision"
	case "heads":
		a.key, err = ObjectKey(s.ObjectKey{Bucket: arg(0), BucketID: arg(1), Key: arg(2)})
		a.incarnation = arg(1)
	case "reservations":
		a.key = stringKey("QuotaReservations", "reservation_id", arg(0))
	case "upload-record", "full-receipt":
		a.key = stringKey("Uploads", "upload_id", arg(0))
		a.scope = parts[0]
	case "bucket-namespace":
		a.key = stringKey("Buckets", "bucket", arg(0))
		a.scope = "namespace"
	case "credential-security":
		a.key = stringKey("Credentials", "access_key_id", arg(0))
		a.scope = "security"
	case "admin-tokens":
		a.key = stringKey("AdminTokens", "token_id", arg(0))
	case "jobs":
		a.key = stringKey("Jobs", "job_id", arg(0))
	case "nodes":
		a.key = stringKey("Nodes", "node_id", arg(0))
	case "producer-outcome":
		a.key, err = OperationKey(arg(0))
		a.scope = "producer"
	case "payload":
		a.key = stringKey("SystemConfig", "config_id", "storage/payload/"+arg(0))
		a.scope = "body"
	case "part-head":
		n, e := strconv.ParseUint(arg(1), 10, 32)
		if e != nil || n == 0 || n > 10000 {
			return a, s.ErrInvalid
		}
		a.key = stringKey("UploadParts", "upload_id", arg(0))
		a.key.Attributes["part_number"] = number(n)
	case "candidate":
		a.key = stringKey("QuotaReservations", "reservation_id", "multipart/candidate/"+arg(0))
		a.scope = "candidate"
	case "publisher":
		a.key = stringKey("Leases", "resource_id", "upload/"+arg(0))
		a.key.Attributes["lease_id"] = text(arg(1))
	case "events", "exports":
		a.key = stringKey("Outbox", "event_id", arg(0))
		if parts[0] == "exports" {
			a.scope = "export_progress"
		}
	case "outcomes", "event-ops", "reservation-ops":
		a.key, err = OperationKey(arg(0))
		a.scope = map[string]string{"outcomes": "result", "event-ops": "event_id", "reservation-ops": "reservation_id"}[parts[0]]
	case "uploads", "receipts":
		a.key = stringKey("Uploads", "upload_id", arg(0))
		if parts[0] == "receipts" {
			a.scope = "receipt"
		}
	case "manifest-owner":
		a.key, err = ManifestSealKey(1, arg(0))
		a.scope = "preparation"
	case "manifest-seals":
		a.key, err = ManifestSealKey(1, arg(0))
		a.scope = "publication"
	case "pages", "part-pages":
		if len(parts) != 3 {
			return a, s.ErrInvalid
		}
		number, parseErr := strconv.ParseUint(parts[2], 16, 32)
		if parseErr != nil {
			return a, s.ErrInvalid
		}
		kind := byte(1)
		if parts[0] == "part-pages" {
			kind = 2
		}
		owner, parseErr := hex.DecodeString(parts[1])
		if parseErr != nil {
			return a, s.ErrInvalid
		}
		a.key, err = PageKey(kind, string(owner), uint32(number))
	case "policy-current":
		a.key, err = PolicyKey(0)
		a.scope = "revision"
	case "admin-installation":
		a.key = stringKey("SystemConfig", "config_id", "admin/installation")
	case "bucket-cache-current", "bucket-cache-history":
		if arg(0) == "" || parts[0] == "bucket-cache-history" && arg(1) == "" {
			return a, s.ErrInvalid
		}
		id := "bucket-cache/" + hex.EncodeToString([]byte(arg(0))) + "/current"
		if parts[0] == "bucket-cache-history" {
			id = "bucket-cache/" + hex.EncodeToString([]byte(arg(0))) + "/history/" + arg(1)
		}
		a.key = stringKey("SystemConfig", "config_id", id)
		if parts[0] == "bucket-cache-current" {
			a.scope = "revision"
		}
	case "policies":
		n, e := strconv.ParseUint(parts[1], 16, 64)
		if e != nil {
			return a, s.ErrInvalid
		}
		a.key, err = PolicyKey(n)
	default:
		return a, s.ErrInvalid
	}
	return a, err
}
func identity(k Key) string {
	parts := []string{k.Table}
	names := make([]string, 0, len(k.Attributes))
	for n := range k.Attributes {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		switch a := k.Attributes[n].(type) {
		case *types.AttributeValueMemberS:
			parts = append(parts, n+":"+hex.EncodeToString([]byte(a.Value)))
		case *types.AttributeValueMemberN:
			parts = append(parts, n+":"+a.Value)
		}
	}
	return strings.Join(parts, "/")
}
func copyItem(item Item) Item {
	if item == nil {
		return nil
	}
	out := Item{}
	for k, v := range item {
		out[k] = v
	}
	return out
}

type observedRow struct {
	key           Key
	before, after Item
	dirty         bool
}
type transaction struct {
	r1Fences        map[s.WriterFence]bool
	recoveryFences  map[s.WriterFence]bool
	store           *Store
	ctx             context.Context
	rows            map[string]*observedRow
	operationID     string
	operationDigest [32]byte
	dispatchRequest *s.CommitRequest
}

func (m *Store) transaction(ctx context.Context) *transaction {
	return &transaction{store: m, ctx: ctx, rows: map[string]*observedRow{}}
}
func (t *transaction) observe(k Key) (*observedRow, error) {
	id := identity(k)
	if row := t.rows[id]; row != nil {
		return row, nil
	}
	if t.store.tables[k.Table] == "" {
		return nil, s.ErrInvalid
	}
	out, err := t.store.readItem(t.ctx, &ddb.GetItemInput{TableName: aws.String(t.store.tables[k.Table]), Key: k.Attributes, ConsistentRead: aws.Bool(true)})
	if err != nil {
		return nil, backend(err)
	}
	if out == nil {
		return nil, s.ErrUnavailable
	}
	row := &observedRow{key: k, before: out.Item, after: copyItem(out.Item)}
	if len(out.Item) == 0 {
		row.before = nil
		row.after = nil
	}
	t.rows[id] = row
	return row, nil
}
func (t *transaction) load(k string) (*observedRow, error) {
	a, err := resolve(k)
	if err != nil {
		return nil, err
	}
	return t.observe(a.key)
}
func (m *Store) view(ctx context.Context, fn func(*transaction) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn(m.transaction(ctx))
}
func (m *Store) update(ctx context.Context, fn func(*transaction) error) error {
	for attempt := 0; attempt < m.maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		t := m.transaction(ctx)
		if err := fn(t); err != nil {
			return err
		}
		err := t.commit()
		if !errors.Is(err, s.ErrConflict) {
			return err
		}
		if attempt+1 == m.maxAttempts {
			return err
		}
		if err := retryPause(ctx, attempt); err != nil {
			return err
		}
	}
	return s.ErrConflict
}
func backend(err error) error {
	if err == nil {
		return nil
	}
	var cancelled *types.TransactionCanceledException
	if errors.As(err, &cancelled) {
		for _, reason := range cancelled.CancellationReasons {
			if aws.ToString(reason.Code) == "ConditionalCheckFailed" || aws.ToString(reason.Code) == "TransactionConflict" {
				return s.ErrConflict
			}
		}
	}
	// Preserve cancellation identity without treating it as proof of no commit.
	return fmt.Errorf("%w: %w", s.ErrUnavailable, err)
}

func (t *transaction) commit() error {
	ids := make([]string, 0, len(t.rows))
	dirty := false
	for id, row := range t.rows {
		ids = append(ids, id)
		dirty = dirty || row.dirty
	}
	if !dirty {
		return t.ctx.Err()
	}
	sort.Strings(ids)
	input := &ddb.TransactWriteItemsInput{}
	images := []Item{}
	inventory := map[string]Table{}
	for _, id := range ids {
		row := t.rows[id]
		table := t.store.tables[row.key.Table]
		spec := Tables()[row.key.Table]
		inventory[table] = spec
		names := map[string]string{"#pk": spec.Partition}
		values := Item{}
		condition := "attribute_not_exists(#pk)"
		if row.before != nil {
			delete(names, "#pk")
			if version, ok := row.before["record_revision"]; ok {
				names["#rv"] = "record_revision"
				values[":observed"] = version
				condition = "#rv = :observed"
			} else if body, ok := row.before["body"]; ok {
				names["#body"] = "body"
				values[":observed"] = body
				condition = "#body = :observed"
			} else {
				return fmt.Errorf("%w: missing record revision", s.ErrUnavailable)
			}
		}
		if len(values) == 0 {
			values = nil
		}
		var action types.TransactWriteItem
		if row.dirty {
			item := copyItem(row.after)
			if _, page := item["body"]; !page {
				encoded, err := json.Marshal(struct{ Before, After Item }{row.before, item})
				if err != nil {
					return err
				}
				digest := sha256.Sum256(encoded)
				item["record_revision"] = text(hex.EncodeToString(digest[:]))
			}
			action.Put = &types.Put{TableName: aws.String(table), Item: item, ConditionExpression: aws.String(condition), ExpressionAttributeNames: names, ExpressionAttributeValues: values}
			images = append(images, item)
		} else {
			action.ConditionCheck = &types.ConditionCheck{TableName: aws.String(table), Key: row.key.Attributes, ConditionExpression: aws.String(condition), ExpressionAttributeNames: names, ExpressionAttributeValues: values}
			image := row.before
			if image == nil {
				image = row.key.Attributes
			}
			images = append(images, image)
		}
		input.TransactItems = append(input.TransactItems, action)
	}
	token := make([]byte, 16)
	if _, err := rand.Read(token); err != nil {
		return err
	}
	input.ClientRequestToken = aws.String(hex.EncodeToString(token) + "0000")
	if err := ValidateTransactionPlan(input, images, inventory); err != nil {
		return err
	}
	if err := validateWireBudget(input); err != nil {
		return err
	}
	if t.operationID != "" {
		return t.dispatchOperation(input, images, inventory)
	}
	if err := t.revalidateR1(); err != nil {
		return err
	}
	var err error
	if _, shared := t.store.authority.(*SharedAuthority); shared {
		_, err = t.store.api.TransactWriteItems(t.ctx, input, func(o *ddb.Options) { o.Retryer = aws.NopRetryer{} })
	} else {
		_, err = t.store.api.TransactWriteItems(t.ctx, input)
	}
	return backend(err)
}

func get[T any](t *transaction, k string) (T, error) {
	var out T
	a, err := resolve(k)
	if err != nil {
		return out, err
	}
	row, err := t.observe(a.key)
	if err != nil {
		return out, err
	}
	if row.after == nil {
		return out, s.ErrNotFound
	}
	if a.incarnation != "" {
		actual, ok := row.after["bucket_id"].(*types.AttributeValueMemberS)
		if !ok {
			return out, s.ErrUnavailable
		}
		if actual.Value != a.incarnation {
			return out, s.ErrNotFound
		}
	}
	var attr types.AttributeValue = &types.AttributeValueMemberM{Value: row.after}
	if a.scope != "" && a.scope != "users" && a.scope != "quotas" {
		attr = row.after[a.scope]
		if attr == nil {
			return out, s.ErrNotFound
		}
	}
	if err := unmarshalNative(attr, reflect.ValueOf(&out).Elem()); err != nil {
		return out, err
	}
	if chunk, ok := any(out).(s.Chunk); ok && (chunk.Validate() != nil || chunkKey(chunk.Key) != k) {
		return out, s.ErrUnavailable
	}
	return out, nil
}
func optional[T any](t *transaction, k string) (T, error) {
	v, err := get[T](t, k)
	if errors.Is(err, s.ErrNotFound) {
		err = nil
	}
	return v, err
}
func read[T any](m *Store, ctx context.Context, k string) (T, error) {
	return get[T](m.transaction(ctx), k)
}
func exists(t *transaction, k string) (bool, error) {
	a, err := resolve(k)
	if err != nil {
		return false, err
	}
	row, err := t.observe(a.key)
	if err != nil {
		return false, err
	}
	if row.after == nil {
		return false, nil
	}
	if a.scope != "" {
		_, ok := row.after[a.scope]
		return ok, nil
	}
	return true, nil
}
func put[T any](t *transaction, k string, v T) error {
	a, err := resolve(k)
	if err != nil {
		return err
	}
	row, err := t.observe(a.key)
	if err != nil {
		return err
	}
	attr, err := marshalNative(reflect.ValueOf(v))
	if err != nil {
		return err
	}
	item := Item{}
	if a.scope != "" || strings.HasPrefix(k, "uploads/") || strings.HasPrefix(k, "buckets/") || strings.HasPrefix(k, "credentials/") || strings.HasPrefix(k, "packs/") {
		item = copyItem(row.after)
		if item == nil {
			item = Item{}
		}
	}
	if a.scope != "" && a.scope != "users" && a.scope != "quotas" {
		item[a.scope] = attr
	} else {
		m, ok := attr.(*types.AttributeValueMemberM)
		if !ok {
			return s.ErrInvalid
		}
		for n, v := range m.Value {
			item[n] = v
		}
	}
	for n, v := range a.key.Attributes {
		item[n] = v
	}
	if b, ok := any(v).(s.Bucket); ok {
		item["bucket_id"] = text(b.ID)
	}
	if a.key.Table == "SystemConfig" {
		switch {
		case strings.HasPrefix(a.key.Attributes["config_id"].(*types.AttributeValueMemberS).Value, "storage/op/"):
			item["record_kind"] = text("storage_operation_v1")
		case a.scope == "publication" || a.scope == "preparation":
			item["record_kind"] = text("manifest_owner_v1")
		}
	}
	if err := ValidateItem(item); err != nil {
		return err
	}
	row.after = item
	row.dirty = true
	return nil
}
func (t *transaction) setPage(k string, body []byte) error {
	a, err := resolve(k)
	if err != nil {
		return err
	}
	row, err := t.observe(a.key)
	if err != nil {
		return err
	}
	item := copyItem(a.key.Attributes)
	item["body"] = &types.AttributeValueMemberB{Value: append([]byte(nil), body...)}
	if err := ValidateItem(item); err != nil {
		return err
	}
	row.after = item
	row.dirty = true
	return nil
}

func (m *Store) fence(t *transaction, w s.WriterFence) error {
	if _, ok := m.authority.(*SharedAuthority); ok {
		if t.r1Fences == nil {
			t.r1Fences = map[s.WriterFence]bool{}
		}
		t.r1Fences[w] = true
	}
	return m.authority.Check(t.ctx, w, func(k Key) (Item, error) {
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
func (m *Store) authorize(t *transaction, credential, bucket string) (s.Authorization, error) {
	c, err := optional[s.Credential](t, key("credentials", credential))
	if err != nil {
		return s.Authorization{}, err
	}
	if !c.Active || c.UserID == "" {
		return s.Authorization{}, s.ErrAccessDenied
	}
	u, err := optional[s.User](t, key("users", c.UserID))
	if err != nil {
		return s.Authorization{}, err
	}
	if !c.Active || !u.Active {
		return s.Authorization{}, s.ErrAccessDenied
	}
	if bucket == "" {
		return s.Authorization{}, s.ErrBucketNotFound
	}
	b, err := get[s.Bucket](t, key("buckets", bucket))
	if err != nil {
		if errors.Is(err, s.ErrNotFound) {
			return s.Authorization{}, s.ErrBucketNotFound
		}
		return s.Authorization{}, err
	}
	if !b.Active {
		return s.Authorization{}, s.ErrBucketNotFound
	}
	g, err := optional[s.Grant](t, key("grants", b.ID, u.ID))
	if err != nil {
		return s.Authorization{}, err
	}
	return s.Authorization{UserID: u.ID, CredentialID: c.ID, Bucket: b, Read: u.ID == b.OwnerID || g.Read, Write: u.ID == b.OwnerID || g.Write, SystemAdmin: c.SystemAdmin}, nil
}
