package admin

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

// CursorTTL is the lifetime of a cursor from issue (API §6.5.3).
const CursorTTL = time.Hour

const cursorMACPrefix = "dsg-admin-cursor-v1."

// ErrInvalidCursor is the only error of Open: format, unknown kid, MAC,
// expiry and binding failures are not distinguished (400 invalid_cursor).
var ErrInvalidCursor = errors.New("invalid_cursor")

// Keyring holds the cluster-shared cursor MAC keys by kid. Current seals;
// every key verifies. A retired key stays until its rotation time + 1 h.
type Keyring struct {
	Current string
	Keys    map[string][]byte
}

// Validate checks kid syntax, key length >= 32 bytes and that Current exists.
func (k Keyring) Validate() error {
	if _, ok := k.Keys[k.Current]; !ok {
		return fmt.Errorf("admin cursor keyring: current kid %q has no key", k.Current)
	}
	for kid, key := range k.Keys {
		if !validKid(kid) {
			return fmt.Errorf("admin cursor keyring: invalid kid %q", kid)
		}
		if len(key) < 32 {
			return fmt.Errorf("admin cursor keyring: key %q shorter than 32 bytes", kid)
		}
	}
	return nil
}

func validKid(s string) bool {
	if len(s) < 1 || len(s) > 32 {
		return false
	}
	for i := range len(s) {
		if c := s[i]; (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

// Binding is what a cursor is bound to: the caller user_id, the decoded
// request path, the bucket_id of bucket routes ("" otherwise) and
// CanonicalFilter of the query.
type Binding struct {
	Principal, Resource, BucketID, Filter string
}

// cursorPayload field order is the wire order of API §6.5.3.
type cursorPayload struct {
	V         int    `json:"v"`
	Principal string `json:"principal"`
	Resource  string `json:"resource"`
	BucketID  string `json:"bucket_id"`
	Filter    string `json:"filter"`
	Pos       string `json:"pos"`
	Exp       int64  `json:"exp"`
}

// CanonicalFilter encodes every query parameter except cursor and limit,
// RFC 3986 percent-encoded, sorted by encoded name then encoded value,
// joined with "&".
func CanonicalFilter(q url.Values) string {
	type pair struct{ name, value string }
	var pairs []pair
	for name, values := range q {
		if name == "cursor" || name == "limit" {
			continue
		}
		for _, v := range values {
			pairs = append(pairs, pair{rfc3986(name), rfc3986(v)})
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].name != pairs[j].name {
			return pairs[i].name < pairs[j].name
		}
		return pairs[i].value < pairs[j].value
	})
	out := make([]string, len(pairs))
	for i, p := range pairs {
		out[i] = p.name + "=" + p.value
	}
	return strings.Join(out, "&")
}

// rfc3986 percent-encodes every byte except unreserved ALPHA DIGIT - . _ ~,
// with uppercase hex.
func rfc3986(s string) string {
	const hexUpper = "0123456789ABCDEF"
	var b strings.Builder
	for i := range len(s) {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '.' || c == '_' || c == '~' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hexUpper[c>>4])
		b.WriteByte(hexUpper[c&15])
	}
	return b.String()
}

func cursorTag(key []byte, kid, payload string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(cursorMACPrefix + kid + "." + payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// Seal issues "<kid>.<payload>.<tag>" with the current key, expiring at
// now + CursorTTL (unix seconds).
func (k Keyring) Seal(b Binding, pos string, now time.Time) (string, error) {
	if err := k.Validate(); err != nil {
		return "", err
	}
	var raw bytes.Buffer
	enc := json.NewEncoder(&raw)
	enc.SetEscapeHTML(false) // filter holds "&"; keep the payload plain JSON
	if err := enc.Encode(cursorPayload{1, b.Principal, b.Resource, b.BucketID, b.Filter, pos, now.Add(CursorTTL).Unix()}); err != nil {
		return "", err
	}
	payload := base64.RawURLEncoding.EncodeToString(bytes.TrimSuffix(raw.Bytes(), []byte("\n")))
	return k.Current + "." + payload + "." + cursorTag(k.Keys[k.Current], k.Current, payload), nil
}

// Open verifies a cursor against the current request and returns pos.
func (k Keyring) Open(cursor string, b Binding, now time.Time) (string, error) {
	parts := strings.Split(cursor, ".")
	if len(parts) != 3 {
		return "", ErrInvalidCursor
	}
	kid, payload, tag := parts[0], parts[1], parts[2]
	key, ok := k.Keys[kid]
	if !validKid(kid) || !ok {
		return "", ErrInvalidCursor
	}
	got, err := base64.RawURLEncoding.Strict().DecodeString(tag)
	if err != nil || len(got) != sha256.Size {
		return "", ErrInvalidCursor
	}
	want, _ := base64.RawURLEncoding.DecodeString(cursorTag(key, kid, payload))
	if !hmac.Equal(got, want) {
		return "", ErrInvalidCursor
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(payload)
	if err != nil {
		return "", ErrInvalidCursor
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var p cursorPayload
	if dec.Decode(&p) != nil || dec.More() {
		return "", ErrInvalidCursor
	}
	if p.V != 1 || now.Unix() >= p.Exp ||
		p.Principal != b.Principal || p.Resource != b.Resource || p.BucketID != b.BucketID || p.Filter != b.Filter {
		return "", ErrInvalidCursor
	}
	return p.Pos, nil
}
