package dynamodb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

type wireClient func(*http.Request) (*http.Response, error)

func (f wireClient) Do(r *http.Request) (*http.Response, error) { return f(r) }

// Exercise the SDK serializer with an in-process HTTP seam, not a second JSON
// marshaller. No network or AWS credentials are used by this test.
func TestNativeBinaryUsesSDKWireEncoding(t *testing.T) {
	in, images := transactionFixture(1, 3)
	if err := ValidateTransactionPlan(in, images, Tables()); err != nil {
		t.Fatal(err)
	}
	called := false
	client := ddb.New(ddb.Options{Region: "us-east-1", Credentials: aws.AnonymousCredentials{}, HTTPClient: wireClient(func(r *http.Request) (*http.Response, error) {
		called = true
		var body struct {
			TransactItems []struct {
				Put struct{ Item map[string]map[string]string }
			}
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if len(body.TransactItems) != 1 || body.TransactItems[0].Put.Item["body"]["B"] != "AAAA" {
			t.Fatal("SDK Binary encoding", body)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/x-amz-json-1.0"}}, Body: io.NopCloser(strings.NewReader("{}")), Request: r}, nil
	})})
	if _, err := client.TransactWriteItems(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("SDK did not dispatch to test seam")
	}
}

func TestTransportBudgetBoundsSDKSerialization(t *testing.T) {
	in, _ := transactionFixture(2, 64<<10)
	in.TransactItems[0].Put.Item["nested"] = &types.AttributeValueMemberM{Value: Item{
		"list": &types.AttributeValueMemberL{Value: []types.AttributeValue{
			&types.AttributeValueMemberN{Value: "9007199254740993"},
			&types.AttributeValueMemberBOOL{Value: true},
			&types.AttributeValueMemberS{Value: "<escaped>\n\""},
		}},
	}}
	upper, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if err = validateWireBudget(in); err != nil {
		t.Fatal(err)
	}
	called := false
	client := ddb.New(ddb.Options{Region: "us-east-1", Credentials: aws.AnonymousCredentials{}, HTTPClient: wireClient(func(r *http.Request) (*http.Response, error) {
		called = true
		actual, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if len(actual) > len(upper) {
			t.Fatalf("SDK wire=%d exceeds guard=%d", len(actual), len(upper))
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("{}")), Request: r}, nil
	})})
	if _, err = client.TransactWriteItems(context.Background(), in); err != nil || !called {
		t.Fatal(err)
	}
	in.TransactItems[0].Put.Item["body"] = &types.AttributeValueMemberB{Value: bytes.Repeat([]byte{1}, MaxWireBytes)}
	if err = validateWireBudget(in); !errors.Is(err, s.ErrInvalid) {
		t.Fatal("transport budget accepted", err)
	}
}
