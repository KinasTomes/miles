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
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func TestLocalFixtureCreateCleanup(t *testing.T) {
	client := localClient(t)
	for _, scenario := range []string{"lost-response", "delayed-visibility", "not-dispatched", "name-collision"} {
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
				target := req.Header.Get("X-Amz-Target")
				if scenario == "delayed-visibility" && target == "DynamoDB_20120810.DescribeTable" {
					return &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{"Content-Type": {"application/x-amz-json-1.0"}},
						Body: io.NopCloser(strings.NewReader(`{"__type":"ResourceNotFoundException","message":"injected delayed visibility"}`)), Request: req}, nil
				}
				if target != "DynamoDB_20120810.CreateTable" {
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
			if scenario == "not-dispatched" || scenario == "delayed-visibility" {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if scenario == "not-dispatched" {
					cancel()
				}
				faultClient := ddb.New(options)
				_, attempts, err := createFixtureTables(ctx, faultClient, prefix, true)
				if scenario == "not-dispatched" {
					if !errors.Is(err, context.Canceled) || calls != 0 || len(attempts) != 0 {
						t.Fatalf("pre-cancelled create reached dispatch: calls=%d attempts=%d err=%v", calls, len(attempts), err)
					}
					verified, err := reconcileFixtureTables(t.Context(), faultClient, attempts, true, time.Now())
					if verified != 0 || err != nil {
						t.Fatal(verified, err)
					}
					return
				}
				if !errors.Is(err, injected) || calls != 1 || len(attempts) != 1 || name == "" {
					t.Fatalf("expected lost create response: calls=%d attempts=%d name=%q err=%v", calls, len(attempts), name, err)
				}
				// Keep the synthetic missing responses active through the whole
				// observation budget, then check with the independent client.
				verified, err := reconcileFixtureTables(t.Context(), faultClient, attempts, true, time.Now().Add(100*time.Millisecond))
				if verified != 0 || err == nil || !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), "outcome still unknown") {
					t.Fatalf("hidden table must remain unresolved: verified=%d err=%v", verified, err)
				}
				if _, err := client.DescribeTable(t.Context(), &ddb.DescribeTableInput{TableName: &name}); err != nil {
					t.Fatal("real table must still exist under delayed visibility", err)
				}
				return
			}
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
