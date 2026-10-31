package s3

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/KinasTomes/miles/internal/object"
)

func TestObjectMetadataRemovesTransportEncoding(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input []string
		want  string
	}{
		{"transport only", []string{"aws-chunked"}, ""},
		{"other encoding", []string{"gzip"}, "gzip"},
		{"combined", []string{"gzip, aws-chunked"}, "gzip"},
		{"case and whitespace", []string{" AWS-Chunked , gzip "}, "gzip"},
		{"multiple header values", []string{"gzip", "aws-chunked, br"}, "gzip, br"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPut, "/demo/file", nil)
			for _, v := range tc.input {
				r.Header.Add("Content-Encoding", v)
			}
			metadata := RequestMetadata(r)
			if got := metadata["content-encoding"]; got != tc.want {
				t.Fatalf("stored encoding %q, want %q", got, tc.want)
			}
			response := http.Header{}
			writeObjectMetadata(response, metadata)
			if got := response.Get("Content-Encoding"); got != tc.want {
				t.Fatalf("returned encoding %q", got)
			}
		})
	}
}
func TestObjectPutLimitsBeforeWriter(t *testing.T) {
	for _, tc := range []struct {
		name     string
		length   int64
		metadata string
	}{
		{"single PUT over 5 GiB", object.MaxPutBytes + 1, ""},
		{"user metadata over 2 KiB", 0, strings.Repeat("x", 2049)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writer := &fakeObjectWriter{}
			r := objectRequest(http.MethodPut, "demo", "file", nil, nil)
			r.HTTP.ContentLength = tc.length
			if tc.metadata != "" {
				r.HTTP.Header.Set("X-Amz-Meta-Large", tc.metadata)
			}
			r.Body = newPayload(r.HTTP, PutObject, nil)
			err := (&Objects{Writer: writer}).PutObject(httptest.NewRecorder(), r)
			if code(t, err) != ErrEntityTooLarge || writer.got.Key != "" {
				t.Fatal("oversize PUT reached writer", err, writer.got)
			}
		})
	}
}

func TestDecodedLengthMatchesPayloadMode(t *testing.T) {
	for _, tc := range []struct {
		name, mode, decoded string
		encoded, want       int64
	}{
		{"plain ignores transport length", "UNSIGNED-PAYLOAD", "0", object.MaxPutBytes + 1, object.MaxPutBytes + 1},
		{"aws-chunked uses decoded length", streamingUnsignedTrailer, "7", 100, 7},
		{"aws-chunked requires decoded length", streamingUnsignedTrailer, "", 100, -1},
		{"aws-chunked rejects invalid length", streamingUnsignedTrailer, "-1", 100, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPut, "/demo/file", nil)
			r.ContentLength = tc.encoded
			r.Header.Set("X-Amz-Content-Sha256", tc.mode)
			r.Header.Set("X-Amz-Decoded-Content-Length", tc.decoded)
			if got := DecodedLength(r); got != tc.want {
				t.Fatal("length does not match decoder", got, tc.want)
			}
		})
	}
}
