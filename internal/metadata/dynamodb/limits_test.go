package dynamodb

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	s "github.com/KinasTomes/miles/internal/storage"

	"github.com/aws/aws-sdk-go-v2/aws"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func TestNativeItemSizeBounds(t *testing.T) {
	// Binary is raw bytes, and UTF-8 names/strings count bytes, not runes.
	item := map[string]types.AttributeValue{"b": &types.AttributeValueMemberB{Value: make([]byte, 6)}, "ñ": text("đ")}
	if n, err := ItemSizeUpperBound(item); err != nil || n != 11 {
		t.Fatal(n, err)
	}
	nested := map[string]types.AttributeValue{
		"m": &types.AttributeValueMemberM{Value: map[string]types.AttributeValue{"ñ": text("đ")}},
		"l": &types.AttributeValueMemberL{Value: []types.AttributeValue{&types.AttributeValueMemberBOOL{Value: true}}},
	}
	if n, err := ItemSizeUpperBound(nested); err != nil || n != 15 {
		t.Fatal(n, err)
	}
	item = map[string]types.AttributeValue{"b": &types.AttributeValueMemberB{Value: make([]byte, MaxItemBytes-1)}}
	if err := ValidateItem(item); err != nil {
		t.Fatal("boundary", err)
	}
	item["b"] = &types.AttributeValueMemberB{Value: make([]byte, MaxItemBytes)}
	if err := ValidateItem(item); err == nil {
		t.Fatal("oversized item accepted")
	}
	for _, v := range []types.AttributeValue{nil, (*types.AttributeValueMemberS)(nil), &types.AttributeValueMemberN{Value: "1e5"}, &types.AttributeValueMemberN{Value: "-0"}, &types.AttributeValueMemberN{Value: strings.Repeat("9", 39)}} {
		if _, err := ItemSizeUpperBound(map[string]types.AttributeValue{"bad": v}); err == nil {
			t.Fatal("unsupported native value", v)
		}
	}
	cycle := &types.AttributeValueMemberM{Value: map[string]types.AttributeValue{}}
	cycle.Value["cycle"] = cycle
	if _, err := ItemSizeUpperBound(map[string]types.AttributeValue{"root": cycle}); err == nil {
		t.Fatal("unbounded nesting")
	}
}

func TestTransactionExactAggregateAndActionBoundaries(t *testing.T) {
	for _, delta := range []int{-1, 0, 1} {
		t.Run(fmt.Sprintf("bytes_%+d", delta), func(t *testing.T) {
			in, images := transactionFixture(11, 0)
			remaining := MaxTransactionBytes + delta
			for i, image := range images {
				overhead, err := ItemSizeUpperBound(image)
				if err != nil {
					t.Fatal(err)
				}
				target := remaining / (len(images) - i)
				image["body"] = &types.AttributeValueMemberB{Value: make([]byte, target-overhead)}
				remaining -= target
			}
			err := ValidateTransactionPlan(in, images, Tables())
			if remaining != 0 || (delta <= 0 && err != nil) || (delta > 0 && !errors.Is(err, s.ErrInvalid)) {
				t.Fatal("aggregate boundary", remaining, err)
			}
		})
	}
	for _, n := range []int{99, 100, 101} {
		t.Run(fmt.Sprintf("actions_%d", n), func(t *testing.T) {
			in, images := transactionFixture(n, 0)
			err := ValidateTransactionPlan(in, images, Tables())
			if (n <= MaxTransactionActions && err != nil) || (n > MaxTransactionActions && !errors.Is(err, s.ErrInvalid)) {
				t.Fatal("action boundary", n, err)
			}
		})
	}
}

func transactionFixture(n, bodyBytes int) (*ddb.TransactWriteItemsInput, []map[string]types.AttributeValue) {
	in := &ddb.TransactWriteItemsInput{ClientRequestToken: aws.String(strings.Repeat("a", 36))}
	images := []map[string]types.AttributeValue{}
	for i := 0; i < n; i++ {
		item := map[string]types.AttributeValue{"event_id": text(fmt.Sprint(i)), "body": &types.AttributeValueMemberB{Value: make([]byte, bodyBytes)}}
		in.TransactItems = append(in.TransactItems, types.TransactWriteItem{Put: &types.Put{TableName: aws.String("Outbox"), Item: item, ConditionExpression: aws.String("attribute_not_exists(event_id)")}})
		images = append(images, item)
	}
	return in, images
}
func TestTransactionGuardCountsAllTargetsAndImages(t *testing.T) {
	in, images := transactionFixture(10, 400000)
	if err := ValidateTransactionPlan(in, images, Tables()); err != nil {
		t.Fatal("bounded request", err)
	}
	large, largeImages := transactionFixture(11, 400000)
	if err := ValidateTransactionPlan(large, largeImages, Tables()); err == nil {
		t.Fatal("aggregate item size ignored")
	}
	tooMany, tooManyImages := transactionFixture(101, 0)
	if err := ValidateTransactionPlan(tooMany, tooManyImages, Tables()); err == nil {
		t.Fatal("action cap ignored")
	}
	in, images = transactionFixture(1, 0)
	key := map[string]types.AttributeValue{"event_id": text("0")}
	in.TransactItems = append(in.TransactItems, types.TransactWriteItem{ConditionCheck: &types.ConditionCheck{TableName: aws.String("Outbox"), Key: key, ConditionExpression: aws.String("attribute_exists(event_id)")}})
	images = append(images, images[0])
	if err := ValidateTransactionPlan(in, images, Tables()); err == nil {
		t.Fatal("Put and ConditionCheck repeated the same target")
	}
	in, images = transactionFixture(1, 0)
	in.TransactItems[0].Put = nil
	in.TransactItems[0].Update = &types.Update{TableName: aws.String("Outbox"), Key: map[string]types.AttributeValue{"event_id": text("different")}}
	if err := ValidateTransactionPlan(in, images, Tables()); err == nil {
		t.Fatal("image does not match target")
	}
	in, images = transactionFixture(1, 0)
	in.ClientRequestToken = aws.String("different-token")
	if err := ValidateTransactionPlan(in, images, Tables()); err == nil {
		t.Fatal("wrong attempt token")
	}
}
