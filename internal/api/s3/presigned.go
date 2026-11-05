package s3

import (
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// presignedHeaders materializes signed query headers on a private request copy.
// Authentication parameters never become operation headers. Conflicting header
// and query values, including differently cased aliases, are ambiguous and fail
// closed instead of choosing one value for verification and another for use.
func presignedHeaders(r *http.Request, query url.Values) (*http.Request, *Error) {
	if !query.Has("X-Amz-Algorithm") {
		return r, nil
	}
	copy := r.Clone(r.Context())
	seen := map[string]bool{}
	for name, values := range query {
		if !strings.HasPrefix(strings.ToLower(name), "x-amz-") || slices.Contains(presignParams, name) {
			continue
		}
		lower := strings.ToLower(name)
		if seen[lower] || slices.ContainsFunc(presignParams, func(p string) bool { return strings.EqualFold(p, name) }) {
			return nil, &Error{Code: ErrInvalidArgument, Message: "ambiguous presigned query header"}
		}
		seen[lower] = true
		if !validQueryHeader(name, values) {
			return nil, &Error{Code: ErrInvalidArgument, Message: "invalid presigned query header"}
		}
		if header := r.Header.Values(name); len(header) > 0 && !slices.Equal(header, values) {
			return nil, &Error{Code: ErrInvalidArgument, Message: "conflicting presigned query and header"}
		}
		copy.Header[http.CanonicalHeaderKey(name)] = slices.Clone(values)
	}
	return copy, nil
}

// Query decoding may produce bytes that an HTTP parser would reject in a
// header. Do not introduce those bytes into metadata or downstream requests.
func validQueryHeader(name string, values []string) bool {
	for _, c := range []byte(name) {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", rune(c))) {
			return false
		}
	}
	for _, value := range values {
		for _, c := range []byte(value) {
			if c < ' ' && c != '\t' || c == 127 {
				return false
			}
		}
	}
	return true
}
