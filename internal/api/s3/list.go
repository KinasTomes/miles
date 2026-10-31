package s3

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/KinasTomes/miles/internal/storage"
)

// maxListKeys is the default and cap of max-keys (API v2 2.5, 4.2).
const maxListKeys = 1000

// listTokenTTL bounds how long a continuation token is accepted.
const listTokenTTL = time.Hour

// listTokenKey MACs continuation tokens.
// ponytail: per-process key, so tokens die with the process and are not shared
// between nodes; move to a configured shared keyring when the cluster profile
// (W05) serves LIST from more than one node.
var listTokenKey = func() []byte {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		panic(err)
	}
	return key
}()

// listBucketResult is the ListObjectsV2 response of API v2 4.2.
type listBucketResult struct {
	XMLName               xml.Name `xml:"ListBucketResult"`
	Name                  string
	Prefix                string `xml:",omitempty"`
	Delimiter             string `xml:",omitempty"`
	MaxKeys               int
	KeyCount              int
	IsTruncated           bool
	EncodingType          string `xml:",omitempty"`
	ContinuationToken     string `xml:",omitempty"`
	NextContinuationToken string `xml:",omitempty"`
	StartAfter            string `xml:",omitempty"`
	Contents              []listContent
	CommonPrefixes        []listPrefix
}

type listContent struct {
	Key          string
	LastModified string
	ETag         string
	Size         int64
	StorageClass string
	Owner        *listOwner `xml:",omitempty"`
}

// listOwner is the DSG owner_id of the bucket, not an AWS canonical ID; S3
// DisplayName is not returned (API v2 4.2 fetch-owner).
type listOwner struct{ ID string }

type listPrefix struct{ Prefix string }

// ListObjectsV2 returns one consistent logical page of the bucket (API v2
// 4.2). The router already authorized read on the bucket incarnation; the
// store checks the read right and incarnation again inside its query.
func (o *Objects) ListObjectsV2(w http.ResponseWriter, r *Request) error {
	if o.Lister == nil {
		return &Error{Code: ErrNotImplemented, Message: "ListObjectsV2 is not implemented"}
	}
	q := r.Query
	invalid := func(msg string) error { return &Error{Code: ErrInvalidArgument, Message: msg, Bucket: r.Bucket} }
	maxKeys := maxListKeys
	if v, ok := q["max-keys"]; ok {
		n, err := strconv.Atoi(v[0])
		if err != nil || n < 0 {
			return invalid("Provided max-keys not an integer or within integer range")
		}
		maxKeys = min(n, maxListKeys)
	}
	encoding := q.Get("encoding-type")
	if _, ok := q["encoding-type"]; ok && encoding != "url" {
		return invalid("Invalid Encoding Method specified in Request")
	}
	fetchOwner := false
	if v, ok := q["fetch-owner"]; ok {
		b, err := strconv.ParseBool(v[0])
		if err != nil {
			return invalid("Invalid fetch-owner value")
		}
		fetchOwner = b
	}
	request := storage.ListObjectsRequest{
		PrincipalID: r.Caller.UserID, Bucket: r.Bucket, BucketID: r.Access.Bucket.ID,
		Prefix: q.Get("prefix"), Delimiter: q.Get("delimiter"), StartAfter: q.Get("start-after"), MaxKeys: maxKeys,
	}
	token, hasToken := q["continuation-token"]
	if hasToken {
		cursor, ok := openListToken(token[0], request, time.Now())
		if !ok {
			return invalid("The continuation token provided is incorrect")
		}
		request.Cursor = &cursor
	}
	page, err := o.Lister.ListObjects(r.HTTP.Context(), request)
	if err != nil {
		return listError(err, r.Bucket)
	}

	enc := func(s string) string { return s }
	if encoding == "url" {
		enc = url.QueryEscape
	}
	out := listBucketResult{
		Name: r.Bucket, Prefix: enc(request.Prefix), Delimiter: enc(request.Delimiter), MaxKeys: maxKeys,
		KeyCount:     len(page.Objects) + len(page.CommonPrefixes),
		IsTruncated:  page.Next != nil,
		EncodingType: encoding, StartAfter: enc(request.StartAfter),
		Contents:       make([]listContent, len(page.Objects)),
		CommonPrefixes: make([]listPrefix, len(page.CommonPrefixes)),
	}
	if hasToken {
		out.ContinuationToken = token[0]
	}
	if page.Next != nil {
		out.NextContinuationToken = sealListToken(*page.Next, time.Now())
	}
	var owner *listOwner
	if fetchOwner {
		owner = &listOwner{ID: r.Access.Bucket.OwnerID}
	}
	for i, e := range page.Objects {
		out.Contents[i] = listContent{
			Key: enc(e.Key), LastModified: e.LastModified.UTC().Format(isoTimeLayout), ETag: e.ETag,
			Size: e.Size, StorageClass: "STANDARD", Owner: owner,
		}
	}
	for i, p := range page.CommonPrefixes {
		out.CommonPrefixes[i] = listPrefix{Prefix: enc(p)}
	}
	return WriteXML(w, out)
}

func listError(err error, bucket string) error {
	code := ErrInternalError
	switch {
	case errors.Is(err, storage.ErrAccessDenied):
		code = ErrAccessDenied
	case errors.Is(err, storage.ErrNotFound):
		// LIST has no key: any absence is the bucket incarnation.
		code = ErrNoSuchBucket
	case errors.Is(err, storage.ErrInvalid):
		code = ErrInvalidArgument
	case errors.Is(err, storage.ErrUnavailable):
		code = ErrServiceUnavailable
	default:
		return err
	}
	return &Error{Code: code, Bucket: bucket}
}

// listTokenPosition is the scan state a continuation token carries. The
// binding (principal, bucket incarnation, prefix, delimiter) is not carried:
// it is taken from the request and covered by the MAC, so a token replayed
// against other options fails before the store sees it.
type listTokenPosition struct {
	Key     string `json:"k"`
	Prefix  string `json:"p,omitempty"`
	Expires int64  `json:"e"`
}

func listTokenTag(c storage.ObjectListCursor, payload string) []byte {
	binding, _ := json.Marshal([]string{"dsg-s3-list-v1", c.PrincipalID, c.Bucket, c.BucketID, c.Prefix, c.Delimiter, payload})
	m := hmac.New(sha256.New, listTokenKey)
	m.Write(binding)
	return m.Sum(nil)
}

// sealListToken issues "<payload>.<tag>", both unpadded base64url, for the
// cursor of the next page, expiring listTokenTTL after now.
func sealListToken(c storage.ObjectListCursor, now time.Time) string {
	raw, _ := json.Marshal(listTokenPosition{Key: c.LastScannedKey, Prefix: c.LastCommonPrefix, Expires: now.Add(listTokenTTL).Unix()})
	payload := base64.RawURLEncoding.EncodeToString(raw)
	return payload + "." + base64.RawURLEncoding.EncodeToString(listTokenTag(c, payload))
}

// openListToken verifies token against the request it continues and returns
// the store cursor. Format, MAC, expiry and binding failures are not told
// apart: each is InvalidArgument (API v2 4.2).
func openListToken(token string, r storage.ListObjectsRequest, now time.Time) (storage.ObjectListCursor, bool) {
	c := storage.ObjectListCursor{PrincipalID: r.PrincipalID, Bucket: r.Bucket, BucketID: r.BucketID, Prefix: r.Prefix, Delimiter: r.Delimiter}
	payload, tag, ok := strings.Cut(token, ".")
	if !ok {
		return c, false
	}
	got, err := base64.RawURLEncoding.Strict().DecodeString(tag)
	if err != nil || !hmac.Equal(got, listTokenTag(c, payload)) {
		return c, false
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(payload)
	if err != nil {
		return c, false
	}
	var p listTokenPosition
	if json.Unmarshal(raw, &p) != nil || now.Unix() >= p.Expires {
		return c, false
	}
	c.LastScannedKey, c.LastCommonPrefix = p.Key, p.Prefix
	return c, true
}
