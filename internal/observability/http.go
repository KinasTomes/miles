package observability

import (
	"io"
	"net/http"

	"github.com/aws/aws-sdk-go-v2/aws"
)

type bodyCounter struct {
	io.ReadCloser
	add func(int)
}

func (b bodyCounter) Read(p []byte) (int, error) { n, e := b.ReadCloser.Read(p); b.add(n); return n, e }

// CountHTTPClient counts bytes consumed by the transport/caller, including partial
// errors. It preserves the client's TLS, redirect and proxy policies. These are
// HTTP body bytes, excluding headers, TLS and TCP overhead, not wire/billed bytes.
func CountHTTPClient(client aws.HTTPClient, sent, received func(int)) aws.HTTPClient {
	return countingClient{client, sent, received}
}

type countingClient struct {
	client         aws.HTTPClient
	sent, received func(int)
}

func (c countingClient) Do(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if r.Body != nil && r.Body != http.NoBody {
		r.Body = bodyCounter{r.Body, c.sent}
	}
	out, e := c.client.Do(r)
	if out != nil && out.Body != nil {
		out.Body = bodyCounter{out.Body, c.received}
	}
	return out, e
}
func (m *Runtime) HTTPClient(client aws.HTTPClient, metadata bool) aws.HTTPClient {
	request, response := "miles_gateway_s3_request_body_bytes_total", "miles_gateway_s3_response_body_bytes_total"
	if metadata {
		request, response = "miles_metadata_request_body_bytes_total", "miles_metadata_response_body_bytes_total"
	}
	return CountHTTPClient(client, func(n int) { m.add(request, float64(n)) }, func(n int) { m.add(response, float64(n)) })
}
func (m *Runtime) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil && r.Body != http.NoBody {
			r = r.Clone(r.Context())
			r.Body = bodyCounter{r.Body, func(n int) { m.add("miles_client_gateway_body_bytes_total", float64(n)) }}
		}
		next.ServeHTTP(&countingResponse{ResponseWriter: w, add: func(n int) { m.add("miles_gateway_client_body_bytes_total", float64(n)) }}, r)
	})
}

type countingResponse struct {
	http.ResponseWriter
	add func(int)
}

func (w *countingResponse) Write(p []byte) (int, error) {
	n, e := w.ResponseWriter.Write(p)
	w.add(n)
	return n, e
}
func (w *countingResponse) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *countingResponse) Flush()                      { _ = http.NewResponseController(w.ResponseWriter).Flush() }
