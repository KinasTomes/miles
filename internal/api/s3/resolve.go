// Package s3 is the S3 API contract of the gateway: the operation table that
// maps a path-style request to exactly one operation, the operations and
// headers DSG rejects instead of ignoring, and the XML error format
// (docs/dedup-storage-gateway-api.md sections 2.1, 2.4, 2.5, 2.7 and 4.1).
// Request/response fixtures for router implementations live in
// tests/fixtures/s3.
package s3

import (
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
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
// stays "+", and "//", ".." and a trailing "/" are kept.
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

const maxKeyBytes = 1024

// routes is the operation table. A request matches a row only when method,
// level, discriminator set and presence of x-amz-copy-source are all equal.
// There is no fallback row, so a write never reaches PutObject by accident.
var routes = []struct {
	method string
	level  level
	query  string // sorted discriminators joined by "&"
	copy   bool   // x-amz-copy-source present
	op     Operation
	scope  scope
}{
	{http.MethodGet, levelService, "", false, ListBuckets, p0},

	{http.MethodPut, levelBucket, "", false, CreateBucket, p0},
	{http.MethodHead, levelBucket, "", false, HeadBucket, p0},
	{http.MethodGet, levelBucket, "list-type", false, ListObjectsV2, p0},
	{http.MethodGet, levelBucket, "location", false, GetBucketLocation, p1},
	{http.MethodPost, levelBucket, "delete", false, DeleteObjects, p1},
	{http.MethodDelete, levelBucket, "", false, DeleteBucket, p1},
	{http.MethodGet, levelBucket, "", false, ListObjects, p2},
	{http.MethodGet, levelBucket, "uploads", false, ListMultipartUploads, p2},
	{http.MethodPost, levelBucket, "", false, PostObject, out},

	{http.MethodPut, levelObject, "", false, PutObject, p0},
	{http.MethodGet, levelObject, "", false, GetObject, p0},
	{http.MethodHead, levelObject, "", false, HeadObject, p0},
	{http.MethodDelete, levelObject, "", false, DeleteObject, p0},
	{http.MethodPut, levelObject, "", true, CopyObject, p1},
	{http.MethodPost, levelObject, "uploads", false, CreateMultipartUpload, p1},
	{http.MethodPut, levelObject, "partNumber&uploadId", false, UploadPart, p1},
	{http.MethodPost, levelObject, "uploadId", false, CompleteMultipartUpload, p1},
	{http.MethodDelete, levelObject, "uploadId", false, AbortMultipartUpload, p1},
	{http.MethodGet, levelObject, "uploadId", false, ListParts, p1},
	{http.MethodPost, levelObject, "restore", false, RestoreObject, p1},
	{http.MethodPut, levelObject, "partNumber&uploadId", true, UploadPartCopy, p2},
	{http.MethodGet, levelObject, "attributes", false, GetObjectAttributes, p2},
	{http.MethodGet, levelObject, "partNumber", false, GetObject, p2},
	{http.MethodHead, levelObject, "partNumber", false, HeadObject, p2},
}

// discriminators are the query keys that select a row of routes.
var discriminators = []string{"attributes", "delete", "list-type", "location", "partNumber", "restore", "uploadId", "uploads"}

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
// Query keys other than discriminators are operation parameters on GET/HEAD
// and are left to the handler. On writes every query key must be a
// discriminator, so an unknown subresource fails instead of turning into a
// PutObject. "x-id" (SDK hint) and "x-amz-*" keys (presigned SigV4 and
// hoisted headers) belong to the auth layer and are skipped here.
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
	read := r.Method == http.MethodGet || r.Method == http.MethodHead
	var subs []string
	for _, k := range slices.Sorted(maps.Keys(query)) {
		switch {
		case k == "x-id" || len(k) >= 6 && strings.EqualFold(k[:6], "x-amz-"):
		case slices.Contains(unsupportedSubresources, k) || strings.HasPrefix(k, "response-"):
			return fail(ErrNotImplemented, "?"+k+" is not implemented")
		case slices.Contains(discriminators, k):
			subs = append(subs, k)
		case !read:
			return fail(ErrNotImplemented, "?"+k+" is not implemented on "+r.Method)
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
		if row.op == ListObjectsV2 && query.Get("list-type") != "2" {
			return fail(ErrInvalidArgument, "list-type must be 2")
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

// checksumAlgorithms are the algorithms of API v2 2.7. Others, including
// SHA512, XXHASH* and MD5 as x-amz-checksum-md5, fail before the body.
var checksumAlgorithms = []string{"CRC32", "CRC32C", "CRC64NVME", "SHA1", "SHA256"}

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
		if e := checkChecksumHeader(lower, value); e != nil {
			return e
		}
	}
	return nil
}

func headerMatches(rule, name string) bool {
	if prefix, ok := strings.CutSuffix(rule, "*"); ok {
		return strings.HasPrefix(name, prefix)
	}
	return name == rule
}

// checkChecksumHeader rejects checksum algorithms outside API v2 2.7 by name.
// Which algorithm/type pairs each operation accepts is the handler's matrix.
func checkChecksumHeader(name, value string) *Error {
	var alg string
	switch name {
	case "x-amz-checksum-mode", "x-amz-checksum-type":
		return nil
	case "x-amz-checksum-algorithm", "x-amz-sdk-checksum-algorithm":
		alg = value
	case "x-amz-trailer":
		var ok bool
		if alg, ok = strings.CutPrefix(strings.ToLower(value), "x-amz-checksum-"); !ok {
			return &Error{Code: ErrInvalidRequest, Message: "trailer " + value + " is not supported"}
		}
	default:
		var ok bool
		if alg, ok = strings.CutPrefix(name, "x-amz-checksum-"); !ok {
			return nil
		}
	}
	if !slices.ContainsFunc(checksumAlgorithms, func(a string) bool { return strings.EqualFold(a, alg) }) {
		return &Error{Code: ErrInvalidRequest, Message: "checksum algorithm " + alg + " is not supported"}
	}
	return nil
}
