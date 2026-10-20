package s3

import (
	"cmp"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/KinasTomes/miles/internal/storage"
)

// Credential is a SigV4 credential of the Credentials table (API v2 2.2.3)
// with its secret decrypted.
type Credential struct {
	UserID, Secret                    string
	Active, SystemAdmin, DebugAllowed bool
}

// Credentials looks up the credential of an access key ID. A key that does
// not exist returns storage.ErrNotFound; a revoked key returns its credential
// with Active false. Any other error is a backend failure.
type Credentials interface {
	Credential(ctx context.Context, accessKeyID string) (Credential, error)
}

// Caller is the principal a request is authenticated as. Debug is the gate of
// every x-dsg-* response header (API v2 5): x-dsg-debug: 1 is signed and the
// credential has both system_admin and debug_allowed.
type Caller struct {
	AccessKeyID, UserID string
	SystemAdmin, Debug  bool
}

const (
	sigV4Algorithm    = "AWS4-HMAC-SHA256"
	amzDateLayout     = "20060102T150405Z"
	maxClockSkew      = 15 * time.Minute
	maxPresignExpires = 7 * 24 * 60 * 60 // seconds

	unsignedPayload          = "UNSIGNED-PAYLOAD"
	streamingSigned          = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD"
	streamingUnsignedTrailer = "STREAMING-UNSIGNED-PAYLOAD-TRAILER"
	streamingSignedTrailer   = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER"
	emptySHA256              = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

// presignParams are the query parameters of a presigned URL; all are required.
var presignParams = []string{"X-Amz-Algorithm", "X-Amz-Credential", "X-Amz-Date", "X-Amz-Expires", "X-Amz-SignedHeaders", "X-Amz-Signature"}

// sigV4 is the signature a request carries in its Authorization header or in
// the query of a presigned URL.
type sigV4 struct {
	accessKeyID   string
	scope         string // <yyyymmdd>/<region>/s3/aws4_request
	amzDate       string
	date          time.Time
	signedHeaders []string
	signature     string
	payloadHash   string
	presigned     bool
	expires       time.Duration
}

// signer holds what the payload decoder needs to check chunk and trailer
// signatures: the signing key of the request scope and the request signature,
// which the first chunk signature chains from.
type signer struct {
	key            []byte
	amzDate, scope string
	seed           string
}

func (s *signer) sign(stringToSign string) string {
	return hex.EncodeToString(hmacSHA256(s.key, stringToSign))
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

// authenticate checks the SigV4 signature of r in the order of API v2 2.2.2:
// format, access key (InvalidAccessKeyId), clock or presigned expiry,
// signature (SignatureDoesNotMatch), then host and x-amz-* headers left out of
// SignedHeaders (AccessDenied). An error that is not an *Error is a failure of
// the credential backend.
func (rt *Router) authenticate(r *http.Request, query url.Values) (Caller, *signer, error) {
	s, e := parseSigV4(r, query, rt.Region)
	if e != nil {
		return Caller{}, nil, e
	}
	cred, err := rt.Credentials.Credential(r.Context(), s.accessKeyID)
	switch {
	case errors.Is(err, storage.ErrNotFound) || err == nil && !cred.Active:
		return Caller{}, nil, &Error{Code: ErrInvalidAccessKeyID}
	case err != nil:
		return Caller{}, nil, fmt.Errorf("s3: look up credential %s: %w", s.accessKeyID, err)
	}

	switch age := rt.Now().Sub(s.date); {
	case s.presigned && age > s.expires:
		return Caller{}, nil, &Error{Code: ErrAccessDenied, Message: "Request has expired"}
	case age < -maxClockSkew || !s.presigned && age > maxClockSkew:
		return Caller{}, nil, &Error{Code: ErrRequestTimeTooSkewed}
	}

	sg := &signer{key: signingKey(cred.Secret, s.scope), amzDate: s.amzDate, scope: s.scope}
	sum := sha256.Sum256([]byte(canonicalRequest(r, query, &s)))
	sg.seed = sg.sign(sigV4Algorithm + "\n" + s.amzDate + "\n" + s.scope + "\n" + hex.EncodeToString(sum[:]))
	if !hmac.Equal([]byte(sg.seed), []byte(s.signature)) {
		return Caller{}, nil, &Error{Code: ErrSignatureDoesNotMatch}
	}

	if !slices.Contains(s.signedHeaders, "host") {
		return Caller{}, nil, &Error{Code: ErrAccessDenied, Message: "host must be signed"}
	}
	for _, name := range slices.Sorted(maps.Keys(r.Header)) {
		if lower := strings.ToLower(name); strings.HasPrefix(lower, "x-amz-") && !slices.Contains(s.signedHeaders, lower) {
			return Caller{}, nil, &Error{Code: ErrAccessDenied, Message: "header " + lower + " must be signed"}
		}
	}
	return Caller{
		AccessKeyID: s.accessKeyID,
		UserID:      cred.UserID,
		SystemAdmin: cred.SystemAdmin,
		Debug:       r.Header.Get("X-Dsg-Debug") == "1" && slices.Contains(s.signedHeaders, "x-dsg-debug") && cred.SystemAdmin && cred.DebugAllowed,
	}, sg, nil
}

// parseSigV4 reads the signature of r without looking anything up. A
// malformed Authorization header or presigned query is InvalidArgument; a
// request without signature is AccessDenied (DSG has no anonymous access) and
// a SigV2 header NotImplemented, like SigV2 presigned URLs in Resolve.
func parseSigV4(r *http.Request, query url.Values, region string) (sigV4, *Error) {
	invalid := func(msg string) (sigV4, *Error) {
		return sigV4{}, &Error{Code: ErrInvalidArgument, Message: msg}
	}
	header := r.Header.Get("Authorization")
	var s sigV4
	var credential, signedHeaders string
	switch presigned := slices.ContainsFunc(presignParams, query.Has); {
	case presigned && header != "":
		return invalid("use either the Authorization header or a presigned query, not both")
	case presigned:
		for _, p := range presignParams {
			if query.Get(p) == "" {
				return invalid("presigned URL requires " + p)
			}
		}
		if query.Get("X-Amz-Algorithm") != sigV4Algorithm {
			return invalid("X-Amz-Algorithm must be " + sigV4Algorithm)
		}
		n, err := strconv.Atoi(query.Get("X-Amz-Expires"))
		if err != nil || n < 1 || n > maxPresignExpires {
			return invalid("X-Amz-Expires must be an integer between 1 and 604800")
		}
		s = sigV4{
			amzDate:     query.Get("X-Amz-Date"),
			signature:   query.Get("X-Amz-Signature"),
			payloadHash: cmp.Or(r.Header.Get("X-Amz-Content-Sha256"), unsignedPayload),
			presigned:   true,
			expires:     time.Duration(n) * time.Second,
		}
		credential, signedHeaders = query.Get("X-Amz-Credential"), query.Get("X-Amz-SignedHeaders")
	case header == "":
		return sigV4{}, &Error{Code: ErrAccessDenied, Message: "anonymous requests are not supported; sign with SigV4"}
	default:
		rest, ok := strings.CutPrefix(header, sigV4Algorithm+" ")
		if !ok {
			if strings.HasPrefix(header, "AWS ") {
				return sigV4{}, &Error{Code: ErrNotImplemented, Message: "SigV2 is not implemented; sign with SigV4"}
			}
			return invalid("Authorization must use " + sigV4Algorithm)
		}
		fields := map[string]string{}
		for part := range strings.SplitSeq(rest, ",") {
			k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
			if _, dup := fields[k]; !ok || dup {
				return invalid("malformed Authorization component " + strings.TrimSpace(part))
			}
			fields[k] = v
		}
		credential, signedHeaders = fields["Credential"], fields["SignedHeaders"]
		s = sigV4{amzDate: r.Header.Get("X-Amz-Date"), signature: fields["Signature"], payloadHash: r.Header.Get("X-Amz-Content-Sha256")}
		if len(fields) != 3 || credential == "" || signedHeaders == "" || s.signature == "" {
			return invalid("Authorization requires Credential, SignedHeaders and Signature")
		}
	}

	var err error
	if s.date, err = time.Parse(amzDateLayout, s.amzDate); err != nil {
		return invalid("X-Amz-Date must be in the form yyyyMMddTHHmmssZ")
	}
	parts := strings.Split(credential, "/")
	if len(parts) != 5 || parts[0] == "" {
		return invalid("credential must be <access key>/<date>/<region>/s3/aws4_request")
	}
	if scope := s.amzDate[:8] + "/" + region + "/s3/aws4_request"; strings.Join(parts[1:], "/") != scope {
		return invalid("credential scope must be " + scope)
	}
	s.accessKeyID, s.scope = parts[0], strings.Join(parts[1:], "/")
	if _, ok := hexDigest(s.signature); !ok {
		return invalid("signature must be 64 hex digits")
	}
	if s.signedHeaders = strings.Split(signedHeaders, ";"); slices.Contains(s.signedHeaders, "") {
		return invalid("malformed SignedHeaders")
	}
	switch _, hexHash := hexDigest(s.payloadHash); {
	case hexHash, s.payloadHash == unsignedPayload, s.payloadHash == streamingSigned,
		s.payloadHash == streamingUnsignedTrailer, s.payloadHash == streamingSignedTrailer:
	default:
		return invalid("x-amz-content-sha256 must be a hex SHA-256, " + unsignedPayload + " or a STREAMING- value of API v2 2.2")
	}
	return s, nil
}

// signingKey derives the SigV4 key of a scope: HMAC-SHA256 chained from
// "AWS4"+secret through date, region, "s3" and "aws4_request".
func signingKey(secret, scope string) []byte {
	key := []byte("AWS4" + secret)
	for part := range strings.SplitSeq(scope, "/") {
		key = hmacSHA256(key, part)
	}
	return key
}

// canonicalRequest is the SigV4 canonical request of r (API v2 2.2.2): the
// path exactly as received, the query decoded once and re-encoded in sorted
// order, the signed headers with trimmed values, then the payload hash.
func canonicalRequest(r *http.Request, query url.Values, s *sigV4) string {
	path, _, _ := strings.Cut(r.RequestURI, "?")
	if !strings.HasPrefix(path, "/") {
		path = r.URL.EscapedPath() // absolute-form request target
	}
	type param struct{ name, value string }
	var params []param
	for name, values := range query {
		if s.presigned && name == "X-Amz-Signature" {
			continue
		}
		for _, v := range values {
			params = append(params, param{uriEncode(name), uriEncode(v)})
		}
	}
	slices.SortFunc(params, func(a, b param) int {
		return cmp.Or(strings.Compare(a.name, b.name), strings.Compare(a.value, b.value))
	})

	var b strings.Builder
	b.WriteString(r.Method + "\n" + path + "\n")
	for i, p := range params {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(p.name + "=" + p.value)
	}
	b.WriteByte('\n')
	for _, name := range s.signedHeaders {
		value := r.Host
		if name != "host" {
			var values []string
			for _, v := range r.Header.Values(name) {
				values = append(values, strings.Join(strings.Fields(v), " "))
			}
			value = strings.Join(values, ",")
		}
		b.WriteString(name + ":" + value + "\n")
	}
	b.WriteString("\n" + strings.Join(s.signedHeaders, ";") + "\n" + s.payloadHash)
	return b.String()
}

// uriEncode is the RFC 3986 encoding of SigV4: A-Z a-z 0-9 - . _ ~ stay,
// every other byte becomes %XX in upper case.
func uriEncode(s string) string { return strings.ReplaceAll(url.QueryEscape(s), "+", "%20") }

// hexDigest decodes 64 hex digits: a SHA-256 digest or a SigV4 signature.
func hexDigest(s string) ([]byte, bool) {
	b, err := hex.DecodeString(s)
	return b, err == nil && len(b) == sha256.Size
}
