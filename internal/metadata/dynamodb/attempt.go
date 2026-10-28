package dynamodb

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/gob"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

var registerAttemptCodec sync.Once

func attemptCodec() {
	registerAttemptCodec.Do(func() {
		gob.Register(&types.AttributeValueMemberS{})
		gob.Register(&types.AttributeValueMemberN{})
		gob.Register(&types.AttributeValueMemberB{})
		gob.Register(&types.AttributeValueMemberBOOL{})
		gob.Register(&types.AttributeValueMemberNULL{})
		gob.Register(&types.AttributeValueMemberM{})
		gob.Register(&types.AttributeValueMemberL{})
	})
}
func commitDigest(r s.CommitRequest) ([32]byte, error) {
	r.Writer = s.WriterFence{}
	r.Completion = nil
	r.ReservationRevision = 0
	body, err := json.Marshal(r)
	return sha256.Sum256(body), err
}

// A pending request is a bounded internal attempt record, NOT an R2 event body.
// The original op row is CASed to Dispatching before dispatch. Final publication
// consumes that exact revision, so retiring an attempt fences all late retries.
func (t *transaction) dispatchOperation(input *ddb.TransactWriteItemsInput, images []Item, inventory map[string]Table) error {
	a, err := resolve(key("outcomes", t.operationID))
	if err != nil {
		return err
	}
	row := t.rows[identity(a.key)]
	if row == nil {
		return s.ErrInvalid
	}
	if row.before != nil && row.before["pending_request"] != nil {
		return s.ErrUnavailable
	}
	var final *types.Put
	for _, action := range input.TransactItems {
		if action.Put != nil && aws.ToString(action.Put.TableName) == t.store.tables[a.key.Table] {
			candidate := Item{}
			for n := range a.key.Attributes {
				candidate[n] = action.Put.Item[n]
			}
			if physicalIdentity(a.key.Table, candidate) == physicalIdentity(a.key.Table, a.key.Attributes) {
				final = action.Put
				break
			}
		}
	}
	if final == nil {
		return s.ErrInvalid
	}
	// Preserve the original op-row predicate for attempt registration.
	oldCondition, oldNames, oldValues := final.ConditionExpression, final.ExpressionAttributeNames, final.ExpressionAttributeValues
	pendingRevision := aws.ToString(input.ClientRequestToken)[:32]
	final.ConditionExpression = aws.String("#rv = :attempt")
	final.ExpressionAttributeNames = map[string]string{"#rv": "record_revision"}
	final.ExpressionAttributeValues = Item{":attempt": text(pendingRevision)}
	final.Item["input_sha256"] = text(hex.EncodeToString(t.operationDigest[:]))
	final.Item["state"] = text("Committed")
	if err := ValidateTransactionPlan(input, images, inventory); err != nil {
		return err
	}
	if err := validateWireBudget(input); err != nil {
		return err
	}
	attemptCodec()
	var buffer bytes.Buffer
	if err := gob.NewEncoder(&buffer).Encode(input); err != nil {
		return fmt.Errorf("%w: attempt encoding", s.ErrInvalid)
	}
	pending := copyItem(row.before)
	if pending == nil {
		pending = copyItem(a.key.Attributes)
	}
	request := buffer.Bytes()
	sum := sha256.Sum256(request)
	pending["record_revision"] = text(pendingRevision)
	pending["record_kind"] = text("storage_operation_v1")
	pending["state"] = text("Dispatching")
	pending["input_sha256"] = text(hex.EncodeToString(t.operationDigest[:]))
	pending["pending_sha256"] = text(hex.EncodeToString(sum[:]))
	pending["pending_request"] = &types.AttributeValueMemberB{Value: request}
	if err := ValidateItem(pending); err != nil {
		return err
	}
	prepareToken := sha256.Sum256([]byte("miles-ddb-attempt-v1/" + aws.ToString(input.ClientRequestToken)))
	registration := &ddb.TransactWriteItemsInput{ClientRequestToken: aws.String(hex.EncodeToString(prepareToken[:16]) + "0000"), TransactItems: []types.TransactWriteItem{{Put: &types.Put{TableName: final.TableName, Item: pending, ConditionExpression: oldCondition, ExpressionAttributeNames: oldNames, ExpressionAttributeValues: oldValues}}}}
	if _, err := t.store.api.TransactWriteItems(t.ctx, registration); err != nil {
		return backend(err)
	}
	_, err = t.store.api.TransactWriteItems(t.ctx, input)
	if isKnownCancellation(err) {
		if clearErr := t.store.retireAttempt(t.ctx, a.key, pending); clearErr != nil {
			return clearErr
		}
		return s.ErrConflict
	}
	return backend(err)
}

func isKnownCancellation(err error) bool {
	var cancelled *types.TransactionCanceledException
	return errors.As(err, &cancelled)
}

func (m *Store) retireAttempt(ctx context.Context, k Key, pending Item) error {
	current, err := m.readItem(ctx, &ddb.GetItemInput{TableName: aws.String(m.tables[k.Table]), Key: k.Attributes, ConsistentRead: aws.Bool(true)})
	if err != nil {
		return backend(err)
	}
	if current == nil || current.Item == nil {
		return s.ErrUnavailable
	}
	if current.Item["result"] != nil {
		return s.ErrConflict
	}
	old, ok := pending["record_revision"].(*types.AttributeValueMemberS)
	actual, valid := current.Item["record_revision"].(*types.AttributeValueMemberS)
	if !ok || !valid || actual.Value != old.Value {
		return s.ErrUnavailable
	}
	item := copyItem(current.Item)
	delete(item, "pending_request")
	delete(item, "pending_sha256")
	item["state"] = text("Prepared")
	revision := sha256.Sum256([]byte("retired/" + old.Value))
	item["record_revision"] = text(hex.EncodeToString(revision[:]))
	_, err = m.api.TransactWriteItems(ctx, &ddb.TransactWriteItemsInput{ClientRequestToken: aws.String(hex.EncodeToString(revision[:16]) + "0000"), TransactItems: []types.TransactWriteItem{{Put: &types.Put{TableName: aws.String(m.tables[k.Table]), Item: item, ConditionExpression: aws.String("#rv = :old"), ExpressionAttributeNames: map[string]string{"#rv": "record_revision"}, ExpressionAttributeValues: Item{":old": old}}}}})
	return backend(err)
}

func (m *Store) resumeCommit(ctx context.Context, r s.CommitRequest) (s.CommitResult, bool, error) {
	var zero s.CommitResult
	t := m.transaction(ctx)
	row, err := t.load(key("outcomes", r.Head.OpID))
	if err != nil {
		return zero, true, err
	}
	if row.after == nil || row.after["pending_request"] == nil {
		return zero, false, nil
	}
	if err := m.fence(t, r.Writer); err != nil {
		return zero, true, err
	}
	auth, err := m.authorize(t, r.CredentialID, r.Head.Object.Bucket)
	if err != nil {
		return zero, true, err
	}
	if auth.UserID != r.UserID || !auth.Write {
		return zero, true, s.ErrAccessDenied
	}
	if auth.Bucket.ID != r.Head.Object.BucketID {
		return zero, true, s.ErrBucketNotFound
	}
	if !r.Head.Deleted {
		if err := m.protect(t, r.Writer, r.Head.ManifestID); err != nil {
			return zero, true, err
		}
	}
	digest, err := commitDigest(r)
	if err != nil {
		return zero, true, s.ErrInvalid
	}
	binding, ok := row.after["input_sha256"].(*types.AttributeValueMemberS)
	if !ok || binding.Value != hex.EncodeToString(digest[:]) {
		return zero, true, s.ErrConflict
	}
	body, err := CloneBinary(row.after["pending_request"])
	if err != nil {
		return zero, true, s.ErrUnavailable
	}
	sum := sha256.Sum256(body)
	expected, ok := row.after["pending_sha256"].(*types.AttributeValueMemberS)
	if !ok || expected.Value != hex.EncodeToString(sum[:]) || len(body) > MaxItemBytes {
		return zero, true, s.ErrUnavailable
	}
	attemptCodec()
	var input ddb.TransactWriteItemsInput
	if gob.NewDecoder(bytes.NewReader(body)).Decode(&input) != nil || !attemptToken.MatchString(aws.ToString(input.ClientRequestToken)) || len(input.TransactItems) > MaxTransactionActions {
		return zero, true, s.ErrUnavailable
	}
	// Saved requests may only address this store's explicitly configured tables.
	allowed := map[string]bool{}
	for _, table := range m.tables {
		allowed[table] = true
	}
	for _, action := range input.TransactItems {
		var table string
		switch {
		case action.Put != nil:
			table = aws.ToString(action.Put.TableName)
		case action.ConditionCheck != nil:
			table = aws.ToString(action.ConditionCheck.TableName)
		default:
			return zero, true, s.ErrUnavailable
		}
		if !allowed[table] {
			return zero, true, s.ErrUnavailable
		}
	}
	_, err = m.api.TransactWriteItems(ctx, &input)
	if err != nil {
		if isKnownCancellation(err) {
			if clearErr := m.retireAttempt(ctx, row.key, row.after); clearErr != nil {
				return zero, true, clearErr
			}
			return zero, false, nil
		}
		return zero, true, backend(err)
	}
	result, err := m.GetCommitOutcome(ctx, r.Head.OpID)
	return result, true, err
}
