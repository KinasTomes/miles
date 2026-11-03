package dynamodb

import (
	"context"
	"reflect"

	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// ScanUploadCandidate advances one native discovery page. An empty filtered
// page can still have a continuation. Eligibility is rechecked by the worker
// and the operation's transaction before changing any upload state.
func (m *Store) ScanUploadCandidate(ctx context.Context, after string) (*s.MultipartUpload, string, error) {
	if after != "" && !mutationID.MatchString(after) {
		return nil, "", s.ErrInvalid
	}
	input := &ddb.ScanInput{
		TableName: aws.String(m.tables["Uploads"]), ConsistentRead: aws.Bool(true), Limit: aws.Int32(1),
		FilterExpression:          aws.String("#u.#state IN (:active,:completing,:aborting)"),
		ExpressionAttributeNames:  map[string]string{"#u": "upload-record", "#state": "state"},
		ExpressionAttributeValues: Item{":active": text("Active"), ":completing": text("Completing"), ":aborting": text("Aborting")},
	}
	if after != "" {
		input.ExclusiveStartKey = Item{"upload_id": text(after)}
	}
	page, err := m.api.Scan(ctx, input)
	if err != nil {
		return nil, "", backend(err)
	}
	if page == nil || len(page.Items) > 1 {
		return nil, "", s.ErrUnavailable
	}
	next := ""
	if len(page.LastEvaluatedKey) != 0 {
		value, ok := page.LastEvaluatedKey["upload_id"].(*types.AttributeValueMemberS)
		if !ok || !mutationID.MatchString(value.Value) || value.Value == after {
			return nil, "", s.ErrUnavailable
		}
		next = value.Value
	}
	if len(page.Items) == 0 {
		return nil, next, nil
	}
	item := page.Items[0]
	var upload s.MultipartUpload
	if err = unmarshalNative(item["upload-record"], reflect.ValueOf(&upload).Elem()); err != nil {
		return nil, "", err
	}
	id, ok := item["upload_id"].(*types.AttributeValueMemberS)
	if !ok || !mutationID.MatchString(id.Value) || upload.ID != id.Value || upload.Object.Validate() != nil || upload.Revision == 0 || upload.ExpiresAt.IsZero() {
		return nil, "", s.ErrUnavailable
	}
	return &upload, next, nil
}
