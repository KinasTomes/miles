package faults

import (
	"bytes"
	"io"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
)

type faultHTTPClient struct {
	client aws.HTTPClient
	reg    *Registry
	nodeID string
	kind   FaultType
}

// WrapHTTPClient wraps an aws.HTTPClient to inject ddb_throttle faults.
func WrapHTTPClient(client aws.HTTPClient, reg *Registry, nodeID string) aws.HTTPClient {
	if reg == nil || client == nil {
		return client
	}
	return &faultHTTPClient{client: client, reg: reg, nodeID: nodeID, kind: FaultDDBThrottle}
}

// WrapS3HTTPClient injects a local S3 error before a request leaves the node.
func WrapS3HTTPClient(client aws.HTTPClient, reg *Registry, nodeID string) aws.HTTPClient {
	if reg == nil || client == nil {
		return client
	}
	return &faultHTTPClient{client: client, reg: reg, nodeID: nodeID, kind: FaultS35xx}
}

func (c *faultHTTPClient) Do(r *http.Request) (*http.Response, error) {
	if c.reg != nil && c.reg.ShouldInject(time.Now(), c.kind, c.nodeID) {
		if c.kind == FaultS35xx {
			body := `<Error><Code>InternalError</Code><Message>Injected S3 failure</Message></Error>`
			return &http.Response{StatusCode: http.StatusInternalServerError, Header: http.Header{"Content-Type": []string{"application/xml"}}, Body: io.NopCloser(bytes.NewReader([]byte(body))), ContentLength: int64(len(body)), Request: r}, nil
		}
		body := `{"__type":"com.amazonaws.dynamodb.v20120810#ProvisionedThroughputExceededException","message":"The level of configured provisioned throughput for the table was exceeded"}`
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Header: http.Header{
				"X-Amzn-Errortype": []string{"ProvisionedThroughputExceededException"},
				"Content-Type":     []string{"application/x-amz-json-1.0"},
			},
			Body:          io.NopCloser(bytes.NewReader([]byte(body))),
			ContentLength: int64(len(body)),
			Request:       r,
		}, nil
	}
	return c.client.Do(r)
}
