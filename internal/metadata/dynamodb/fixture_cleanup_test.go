package dynamodb

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func TestLocalFixtureCreateCleanup(t *testing.T) {
	client := localClient(t)
	for _, scenario := range []string{"lost-response", "not-dispatched", "name-collision"} {
		t.Run(scenario, func(t *testing.T) {
			var nonce [8]byte
			if _, err := rand.Read(nonce[:]); err != nil {
				t.Fatal(err)
			}
			prefix := "miles_contract_cleanup_" + hex.EncodeToString(nonce[:]) + "_"
			var name string
			var calls int
			injected := errors.New("injected CreateTable transport error")
			options := client.Options()
			transport := options.HTTPClient
			options.HTTPClient = wireClient(func(req *http.Request) (*http.Response, error) {
				if req.Header.Get("X-Amz-Target") != "DynamoDB_20120810.CreateTable" {
					return transport.Do(req)
				}
				calls++
				body, err := io.ReadAll(req.Body)
				req.Body.Close()
				if err != nil {
					return nil, err
				}
				var input struct{ TableName string }
				if err := json.Unmarshal(body, &input); err != nil {
					return nil, err
				}
				name = input.TableName
				if scenario == "not-dispatched" {
					return nil, injected
				}
				if scenario == "name-collision" {
					// Create the exact name through a separate, unmodified client.
					_, err := client.CreateTable(req.Context(), &ddb.CreateTableInput{
						TableName: &name, BillingMode: types.BillingModePayPerRequest,
						AttributeDefinitions: []types.AttributeDefinition{{AttributeName: aws.String("collision_pk"), AttributeType: types.ScalarAttributeTypeS}},
						KeySchema:            []types.KeySchemaElement{{AttributeName: aws.String("collision_pk"), KeyType: types.KeyTypeHash}},
					})
					if err != nil {
						return nil, err
					}
				}
				req.Body = io.NopCloser(bytes.NewReader(body))
				response, err := transport.Do(req)
				if err != nil || scenario == "name-collision" {
					return response, err
				}
				if response.StatusCode != http.StatusOK {
					return response, nil
				}
				response.Body.Close()
				return nil, injected
			})
			t.Cleanup(func() {
				if name == "" {
					return
				}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				// Test-owned fallback, even if the regression assertion fails.
				_, err := client.DeleteTable(ctx, &ddb.DeleteTableInput{TableName: &name})
				var missing *types.ResourceNotFoundException
				if err != nil && !errors.As(err, &missing) {
					t.Error(err)
				}
			})
			t.Run("fixture", func(t *testing.T) {
				_, err := provisionFixtureTables(t, t.Context(), ddb.New(options), prefix, true)
				var collision *types.ResourceInUseException
				if scenario == "name-collision" {
					if !errors.As(err, &collision) {
						t.Fatal("expected duplicate-name rejection", err)
					}
				} else if !errors.Is(err, injected) {
					t.Fatal("expected injected transport error", err)
				}
			})
			if calls != 1 || name == "" {
				t.Fatalf("CreateTable dispatches=%d name=%q", calls, name)
			}
			out, err := client.DescribeTable(t.Context(), &ddb.DescribeTableInput{TableName: &name})
			if scenario == "name-collision" {
				if err != nil || aws.ToString(out.Table.KeySchema[0].AttributeName) != "collision_pk" {
					t.Fatal("cleanup modified the pre-existing table", err)
				}
			} else {
				var missing *types.ResourceNotFoundException
				if !errors.As(err, &missing) {
					t.Fatal("expected table to be absent after cleanup", err)
				}
			}
		})
	}
}
