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
}

// WrapHTTPClient wraps an aws.HTTPClient to inject ddb_throttle faults.
func WrapHTTPClient(client aws.HTTPClient, reg *Registry, nodeID string) aws.HTTPClient {
	if reg == nil || client == nil {
		return client
	}
	return &faultHTTPClient{client: client, reg: reg, nodeID: nodeID}
}

func (c *faultHTTPClient) Do(r *http.Request) (*http.Response, error) {
	if c.reg != nil && c.reg.ShouldInject(time.Now(), FaultDDBThrottle, c.nodeID) {
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
