package benchmark

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	smithyhttp "github.com/aws/smithy-go/transport/http"
)

func TestResponseDiagnosticsOnlyPersistsBoundedHTTPMetadata(t *testing.T) {
	for _, tc := range []struct {
		name, id, wantID   string
		status, wantStatus int
	}{
		{"valid S3 ID", "AB12CDEF567890AB", "AB12CDEF567890AB", 500, 500},
		{"reject URL and credentials", "secret=https://private-host/secret", "", 503, 503},
		{"reject long ID", strings.Repeat("A", 129), "", 500, 500},
		{"reject invalid HTTP code", "FA11AB1E1234ABCD", "FA11AB1E1234ABCD", 900, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			httpErr := &smithyhttp.ResponseError{
				Response: &smithyhttp.Response{Response: &http.Response{StatusCode: tc.status,
					Header: http.Header{"X-Amz-Request-Id": []string{tc.id}}}},
				Err: errors.New("raw private URI and credential must not be persisted"),
			}
			status, id := responseDiagnostics(httpErr)
			if status != tc.wantStatus || id != tc.wantID {
				t.Fatalf("unsafe or missing HTTP diagnostics: status=%d id=%q", status, id)
			}
		})
	}
	if status, id := responseDiagnostics(nil); status != 0 || id != "" {
		t.Fatal(status, id)
	}
	broken := &smithyhttp.ResponseError{Response: &smithyhttp.Response{}, Err: errors.New("private")}
	if status, id := responseDiagnostics(broken); status != 0 || id != "" {
		t.Fatal(status, id)
	}
}
