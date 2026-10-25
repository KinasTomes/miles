// Package s3 is the S3 API of the gateway: the operation table that maps a
// path-style request to exactly one operation, the query parameters, headers
// and checksum combinations DSG rejects instead of ignoring, the XML error
// format (docs/dedup-storage-gateway-api.md sections 2.1, 2.4, 2.5, 2.7 and
// 4.1), and the transport of API v2 2.2.2 and 3: Router checks SigV4 header
// and presigned signatures, and Payload decodes aws-chunked bodies and checks
// their signatures, length and checksums. Request/response fixtures live in
// tests/fixtures/s3.
//
// Resolve checks what can be decided from the request line and headers
// alone. Handlers own every check that needs a parameter's meaning or stored
// state: max-keys, max-parts, part-number-marker, continuation-token,
// encoding-type, fetch-owner, start-after, Range syntax, bucket name rules,
// checksum algorithm/type against the upload, and If-Match against the ETag.
package s3

import (
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Operation is the AWS operation name; it is also the op label of
// s3_api_requests_total.
type Operation string

const (
	ListBuckets             Operation = "ListBuckets"
	CreateBucket            Operation = "CreateBucket"
	HeadBucket              Operation = "HeadBucket"
	DeleteBucket            Operation = "DeleteBucket"
	GetBucketLocation       Operation = "GetBucketLocation"
	ListObjectsV2           Operation = "ListObjectsV2"
	ListObjects             Operation = "ListObjects"
	ListMultipartUploads    Operation = "ListMultipartUploads"
	DeleteObjects           Operation = "DeleteObjects"
	PostObject              Operation = "PostObject"
	PutObject               Operation = "PutObject"
	CopyObject              Operation = "CopyObject"
	GetObject               Operation = "GetObject"
	HeadObject              Operation = "HeadObject"
	DeleteObject            Operation = "DeleteObject"
	GetObjectAttributes     Operation = "GetObjectAttributes"
	RestoreObject           Operation = "RestoreObject"
	CreateMultipartUpload   Operation = "CreateMultipartUpload"
	UploadPart              Operation = "UploadPart"
	UploadPartCopy          Operation = "UploadPartCopy"
	CompleteMultipartUpload Operation = "CompleteMultipartUpload"
	AbortMultipartUpload    Operation = "AbortMultipartUpload"
	ListParts               Operation = "ListParts"
)

// Route is a request resolved to one operation. Bucket is the first path
// segment as sent. Key is the rest of the path decoded exactly once: "+"
// stays "+", and "//", ".." and a trailing "/" are kept. A key that is not
// valid UTF-8 never resolves: Resolve answers InvalidArgument (S3 defines
// keys as UTF-8, and no durable write may see such a key).
type Route struct {
	Op     Operation
	Bucket string
	Key    string
	Query  url.Values
}

type level uint8

const (
	levelService level = iota
	levelBucket
	levelObject
)

// scope is the API v2 priority. Only p0 and p1 rows resolve; p2 and out rows
// exist so those requests get NotImplemented instead of a neighbouring row.
type scope uint8

const (
	p0 scope = iota
	p1
	p2
	out
)

const (
	maxKeyBytes = 1024
	maxParts    = 10000
)

var (
	listObjectsV2Params = []string{"continuation-token", "delimiter", "encoding-type", "fetch-owner", "max-keys", "prefix", "start-after"}
	listPartsParams     = []string{"max-parts", "part-number-marker"}
)

// routes is the operation table. A request matches a row only when method,
// level, discriminator set and presence of x-amz-copy-source are all equal.
// There is no fallback row, so a write never reaches PutObject by accident.
// params lists the other query parameters the operation takes; any other key
// is NotImplemented, on every method.
var routes = []struct {
	method string
	level  level
	query  string // sorted discriminators joined by "&"
	copy   bool   // x-amz-copy-source present
	op     Operation
	scope  scope
	params []string
}{
	{http.MethodGet, levelService, "", false, ListBuckets, p0, nil},

	{http.MethodPut, levelBucket, "", false, CreateBucket, p0, nil},
	{http.MethodHead, levelBucket, "", false, HeadBucket, p0, nil},
	{http.MethodGet, levelBucket, "list-type", false, ListObjectsV2, p0, listObjectsV2Params},
	{http.MethodGet, levelBucket, "location", false, GetBucketLocation, p1, nil},
	{http.MethodPost, levelBucket, "delete", false, DeleteObjects, p1, nil},
	{http.MethodDelete, levelBucket, "", false, DeleteBucket, p1, nil},
	{http.MethodGet, levelBucket, "", false, ListObjects, p2, nil},
	{http.MethodGet, levelBucket, "uploads", false, ListMultipartUploads, p2, nil},
	{http.MethodPost, levelBucket, "", false, PostObject, out, nil},

	{http.MethodPut, levelObject, "", false, PutObject, p0, nil},
	{http.MethodGet, levelObject, "", false, GetObject, p0, nil},
	{http.MethodHead, levelObject, "", false, HeadObject, p0, nil},
	{http.MethodDelete, levelObject, "", false, DeleteObject, p0, nil},
	{http.MethodPut, levelObject, "", true, CopyObject, p1, nil},
	{http.MethodPost, levelObject, "uploads", false, CreateMultipartUpload, p1, nil},
	{http.MethodPut, levelObject, "partNumber&uploadId", false, UploadPart, p1, nil},
	{http.MethodPost, levelObject, "uploadId", false, CompleteMultipartUpload, p1, nil},
	{http.MethodDelete, levelObject, "uploadId", false, AbortMultipartUpload, p1, nil},
	{http.MethodGet, levelObject, "uploadId", false, ListParts, p1, listPartsParams},
	{http.MethodPost, levelObject, "restore", false, RestoreObject, p1, nil},
	{http.MethodPut, levelObject, "partNumber&uploadId", true, UploadPartCopy, p2, nil},
	{http.MethodGet, levelObject, "attributes", false, GetObjectAttributes, p2, nil},
	{http.MethodGet, levelObject, "partNumber", false, GetObject, p2, nil},
	{http.MethodHead, levelObject, "partNumber", false, HeadObject, p2, nil},
}

// Discriminators are the query keys that select a row of routes. Flag
// subresources take no value; the others need a non-empty one.
var (
	flagSubresources  = []string{"attributes", "delete", "location", "restore", "uploads"}
	valueSubresources = []string{"list-type", "partNumber", "uploadId"}
)

// unsupportedSubresources are S3 features outside DSG scope (API v2 4.1). They
// get NotImplemented on every method instead of being served as a plain
// GET/PUT of the bucket or object.
var unsupportedSubresources = []string{
	"accelerate", "acl", "analytics", "cors", "encryption", "intelligent-tiering",
	"inventory", "legal-hold", "lifecycle", "logging", "metadataConfiguration",
	"metadataTable", "metrics", "notification", "object-lock", "ownershipControls",
	"policy", "policyStatus", "publicAccessBlock", "renameObject", "replication",
	"requestPayment", "retention", "select", "session", "tagging", "torrent",
	"versionId", "versioning", "versions", "website",
}

// Resolve maps a path-style request to its operation, or to the S3 error the
// router must return before authentication reads the body.
//
// Every query key must be a discriminator or a parameter of the resolved
// operation, and may appear once. "x-id" (SDK hint) and "x-amz-*" keys
// (presigned SigV4 and hoisted headers) belong to the auth layer and are
// skipped here.
func Resolve(r *http.Request) (Route, *Error) {
	rest, ok := strings.CutPrefix(r.URL.EscapedPath(), "/")
	if !ok {
		return Route{}, &Error{Code: ErrInvalidRequest, Message: "request target must be an absolute path"}
	}
	bucket, rawKey, _ := strings.Cut(rest, "/")
	key, err := url.PathUnescape(rawKey)
	if err != nil {
		return Route{}, &Error{Code: ErrInvalidArgument, Message: "malformed key encoding", Bucket: bucket}
	}
	// S3 defines object keys as UTF-8, and the metadata layer (canonical
	// JSON, Badger) cannot keep a key that is not; reject it here, before
	// any durable write can see it.
	if !utf8.ValidString(key) {
		return Route{}, &Error{Code: ErrInvalidArgument, Message: "object key must be valid UTF-8", Bucket: bucket, Key: key}
	}
	fail := func(code ErrorCode, msg string) (Route, *Error) {
		return Route{}, &Error{Code: code, Message: msg, Bucket: bucket, Key: key}
	}
	if bucket == "" && key != "" {
		return fail(ErrInvalidBucketName, "")
	}
	if len(key) > maxKeyBytes {
		return fail(ErrKeyTooLong, "")
	}

	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return fail(ErrInvalidArgument, "malformed query string")
	}
	var subs, params []string
	for _, k := range slices.Sorted(maps.Keys(query)) {
		v := query[k]
		if len(v) > 1 {
			return fail(ErrInvalidArgument, "query parameter "+k+" is repeated")
		}
		switch {
		case k == "x-id" || len(k) >= 6 && strings.EqualFold(k[:6], "x-amz-"):
		case slices.Contains(unsupportedSubresources, k) || strings.HasPrefix(k, "response-"):
			return fail(ErrNotImplemented, "?"+k+" is not implemented")
		case slices.Contains(flagSubresources, k):
			if v[0] != "" {
				return fail(ErrInvalidArgument, "?"+k+" takes no value")
			}
			subs = append(subs, k)
		case slices.Contains(valueSubresources, k):
			if v[0] == "" {
				return fail(ErrInvalidArgument, k+" must not be empty")
			}
			subs = append(subs, k)
		default:
			params = append(params, k)
		}
	}
	sub := strings.Join(subs, "&")

	lvl := levelObject
	switch {
	case bucket == "":
		lvl = levelService
	case key == "":
		lvl = levelBucket
	}
	copySource := r.Header.Get("X-Amz-Copy-Source") != ""

	methodAllowed := false
	for _, row := range routes {
		if row.method != r.Method || row.level != lvl {
			continue
		}
		methodAllowed = true
		if row.query != sub || row.copy != copySource {
			continue
		}
		if row.scope > p1 {
			name := string(row.op)
			if row.query != "" {
				name += "?" + row.query
			}
			return fail(ErrNotImplemented, name+" is not implemented")
		}
		for _, p := range params {
			if !slices.Contains(row.params, p) {
				return fail(ErrNotImplemented, "?"+p+" is not implemented for "+string(row.op))
			}
		}
		switch row.op {
		case ListObjectsV2:
			if query.Get("list-type") != "2" {
				return fail(ErrInvalidArgument, "list-type must be 2")
			}
		case UploadPart:
			if n, err := strconv.ParseUint(query.Get("partNumber"), 10, 32); err != nil || n < 1 || n > maxParts {
				return fail(ErrInvalidArgument, "Part number must be an integer between 1 and 10000, inclusive")
			}
		}
		if e := checkHeaders(r.Header, row.op); e != nil {
			e.Bucket, e.Key = bucket, key
			return Route{}, e
		}
		return Route{Op: row.op, Bucket: bucket, Key: key, Query: query}, nil
	}
	if !methodAllowed {
		return fail(ErrMethodNotAllowed, "")
	}
	return fail(ErrInvalidRequest, "no operation matches "+r.Method+" with this query and x-amz-copy-source")
}

// headerRules lists request headers DSG does not implement. Ignoring them
// would let the client believe a condition, ACL, tag, lock, website redirect,
// customer key or owner check was applied. A name ending in "*" is a prefix;
// allow holds values that already match DSG behaviour. x-amz-storage-class and
// non-customer x-amz-server-side-encryption* are accepted without controlling
// layout and are never echoed (API v2 4.2 PutObject).
var headerRules = []struct {
	name  string
	allow []string
}{
	{"if-match", nil},
	{"if-none-match", nil},
	{"if-modified-since", nil},
	{"if-unmodified-since", nil},
	{"x-amz-copy-source-if-*", nil},
	{"x-amz-acl", []string{"private", "bucket-owner-full-control"}},
	{"x-amz-grant-*", nil},
	{"x-amz-object-ownership", []string{"BucketOwnerEnforced"}},
	{"x-amz-bucket-object-lock-enabled", nil},
	{"x-amz-object-lock-*", nil},
	{"x-amz-tagging", nil},
	{"x-amz-tagging-directive", []string{"COPY"}},
	{"x-amz-website-redirect-location", nil},
	{"x-amz-server-side-encryption-customer-*", nil},
	{"x-amz-copy-source-server-side-encryption-customer-*", nil},
	{"x-amz-expected-bucket-owner", nil},
	{"x-amz-source-expected-bucket-owner", nil},
	{"x-amz-write-offset-bytes", nil},
}

func checkHeaders(h http.Header, op Operation) *Error {
	for _, name := range slices.Sorted(maps.Keys(h)) {
		lower := strings.ToLower(name)
		value := h.Get(name)
		if lower == "if-match" && op == GetObject {
			// aws s3 cp sends If-Match on every ranged GET of a multipart
			// download (s3transfer >= 0.13, AWS CLI v2); the GetObject
			// handler must answer 412 PreconditionFailed on mismatch.
			continue
		}
		for _, rule := range headerRules {
			if headerMatches(rule.name, lower) && !slices.Contains(rule.allow, value) {
				return &Error{Code: ErrNotImplemented, Message: "header " + lower + " is not implemented"}
			}
		}
	}
	return checkChecksum(h, op)
}

func headerMatches(rule, name string) bool {
	if prefix, ok := strings.CutSuffix(rule, "*"); ok {
		return strings.HasPrefix(name, prefix)
	}
	return name == rule
}

// checksumTypes maps each algorithm of API v2 2.7 to the multipart checksum
// types it supports. Other algorithms, including SHA512, XXHASH* and MD5 as
// x-amz-checksum-md5, fail before the body.
var checksumTypes = map[string][]string{
	"CRC64NVME": {"FULL_OBJECT"},
	"CRC32":     {"FULL_OBJECT", "COMPOSITE"},
	"CRC32C":    {"FULL_OBJECT", "COMPOSITE"},
	"SHA1":      {"COMPOSITE"},
	"SHA256":    {"COMPOSITE"},
}

// trailerPayloads are the x-amz-content-sha256 values whose aws-chunked body
// carries the trailer named by x-amz-trailer.
var trailerPayloads = []string{streamingUnsignedTrailer, streamingSignedTrailer}

// checkChecksum rejects checksum headers DSG cannot honour, before the body
// is read: an algorithm outside checksumTypes; more than one checksum value
// (x-amz-checksum-<alg> headers and x-amz-trailer entries together); names
// that disagree with x-amz-sdk-checksum-algorithm or x-amz-checksum-algorithm;
// x-amz-trailer without a trailer payload or the reverse; and
// x-amz-checksum-type or x-amz-checksum-mode outside the operations and values
// S3 defines for them. All are InvalidRequest (API v2 2.7).
func checkChecksum(h http.Header, op Operation) *Error {
	invalid := func(msg string) *Error { return &Error{Code: ErrInvalidRequest, Message: msg} }
	var algs []string // every algorithm named, in header order
	values := 0       // checksum values sent in headers or trailers
	for _, name := range slices.Sorted(maps.Keys(h)) {
		lower := strings.ToLower(name)
		alg, isChecksum := strings.CutPrefix(lower, "x-amz-checksum-")
		switch {
		case lower == "x-amz-sdk-checksum-algorithm" || isChecksum && alg == "algorithm":
			algs = append(algs, h[name]...)
		case !isChecksum || alg == "mode" || alg == "type":
		default:
			for range h[name] {
				algs = append(algs, alg)
			}
			values += len(h[name])
		}
	}

	payload := h.Get("X-Amz-Content-Sha256")
	if trailers := h.Values("X-Amz-Trailer"); len(trailers) > 0 {
		if !slices.Contains(trailerPayloads, payload) {
			return invalid("x-amz-trailer requires x-amz-content-sha256 " + strings.Join(trailerPayloads, " or "))
		}
		for _, t := range strings.Split(strings.Join(trailers, ","), ",") {
			alg, ok := strings.CutPrefix(strings.ToLower(strings.TrimSpace(t)), "x-amz-checksum-")
			if !ok {
				return invalid("trailer " + t + " is not supported")
			}
			algs = append(algs, alg)
			values++
		}
	} else if slices.Contains(trailerPayloads, payload) {
		return invalid("x-amz-content-sha256 " + payload + " requires x-amz-trailer")
	}

	for _, a := range algs {
		if _, ok := checksumTypes[strings.ToUpper(a)]; !ok {
			return invalid("checksum algorithm " + a + " is not supported")
		}
	}
	if values > 1 {
		return invalid("expecting a single x-amz-checksum- header or trailer")
	}
	for _, a := range algs {
		if !strings.EqualFold(a, algs[0]) {
			return invalid("checksum headers name different algorithms: " + algs[0] + ", " + a)
		}
	}

	if types := h.Values("X-Amz-Checksum-Type"); len(types) > 0 {
		if op != CreateMultipartUpload && op != CompleteMultipartUpload {
			return invalid("x-amz-checksum-type is only valid on CreateMultipartUpload and CompleteMultipartUpload")
		}
		t := types[0]
		if len(types) > 1 || t != "FULL_OBJECT" && t != "COMPOSITE" {
			return invalid("x-amz-checksum-type must be FULL_OBJECT or COMPOSITE")
		}
		if op == CreateMultipartUpload {
			alg := strings.ToUpper(h.Get("X-Amz-Checksum-Algorithm"))
			if alg == "" {
				return invalid("x-amz-checksum-type requires x-amz-checksum-algorithm")
			}
			if !slices.Contains(checksumTypes[alg], t) {
				return invalid(alg + " does not support checksum type " + t)
			}
		}
	}
	if modes := h.Values("X-Amz-Checksum-Mode"); len(modes) > 0 {
		if op != GetObject && op != HeadObject {
			return invalid("x-amz-checksum-mode is only valid on GetObject and HeadObject")
		}
		if len(modes) > 1 || modes[0] != "ENABLED" {
			return invalid("x-amz-checksum-mode must be ENABLED")
		}
	}
	return nil
}
