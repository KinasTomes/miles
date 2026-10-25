package admin

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// wireFixture mirrors tests/fixtures/admin/wire.json (API §6.5.3).
type wireFixture struct {
	Cursor struct {
		Kid         string `json:"kid"`
		KeyHex      string `json:"key_hex"`
		Now         int64  `json:"now"`
		Principal   string `json:"principal"`
		Resource    string `json:"resource"`
		BucketID    string `json:"bucket_id"`
		Query       string `json:"query"`
		Filter      string `json:"filter"`
		Pos         string `json:"pos"`
		Exp         int64  `json:"exp"`
		PayloadJSON string `json:"payload_json"`
		Cursor      string `json:"cursor"`
	} `json:"cursor"`
	Token struct {
		TokenIDBytesHex string `json:"token_id_bytes_hex"`
		SecretBytesHex  string `json:"secret_bytes_hex"`
		TokenID         string `json:"token_id"`
		Secret          string `json:"secret"`
		Token           string `json:"token"`
		SecretHash      string `json:"secret_hash"`
	} `json:"token"`
}

func loadWire(t *testing.T) wireFixture {
	t.Helper()
	raw, err := os.ReadFile("../../../tests/fixtures/admin/wire.json")
	if err != nil {
		t.Fatal(err)
	}
	var f wireFixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func fixtureRing(t *testing.T) (wireFixture, Keyring, Binding, time.Time) {
	t.Helper()
	f := loadWire(t)
	c := f.Cursor
	k := Keyring{Current: c.Kid, Keys: map[string][]byte{c.Kid: mustHex(t, c.KeyHex)}}
	b := Binding{Principal: c.Principal, Resource: c.Resource, BucketID: c.BucketID, Filter: c.Filter}
	return f, k, b, time.Unix(c.Now, 0)
}

func TestCursorVector(t *testing.T) {
	f, k, b, now := fixtureRing(t)
	q, err := url.ParseQuery(f.Cursor.Query)
	if err != nil {
		t.Fatal(err)
	}
	if got := CanonicalFilter(q); got != f.Cursor.Filter {
		t.Fatalf("CanonicalFilter = %q, want %q", got, f.Cursor.Filter)
	}
	got, err := k.Seal(b, f.Cursor.Pos, now)
	if err != nil {
		t.Fatal(err)
	}
	if got != f.Cursor.Cursor {
		t.Fatalf("Seal =\n%s\nwant\n%s", got, f.Cursor.Cursor)
	}
	payload, _ := base64.RawURLEncoding.DecodeString(strings.Split(got, ".")[1])
	if string(payload) != f.Cursor.PayloadJSON {
		t.Fatalf("payload = %s, want %s", payload, f.Cursor.PayloadJSON)
	}
	pos, err := k.Open(f.Cursor.Cursor, b, time.Unix(f.Cursor.Exp-1, 999_999_999))
	if err != nil || pos != f.Cursor.Pos {
		t.Fatalf("Open just before exp = %q, %v", pos, err)
	}
}

func TestCanonicalFilterEncoding(t *testing.T) {
	q := url.Values{
		"a-b":    {"1"},
		"a":      {"z", "+", "é"},
		"cursor": {"x"},
		"limit":  {"5"},
	}
	// "a" < "a-b" as names even though "a=" > "a-" as bytes; space/plus/UTF-8 escaped.
	want := "a=%2B&a=%C3%A9&a=z&a-b=1"
	if got := CanonicalFilter(q); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if got := CanonicalFilter(url.Values{"user_id": {"a b~._-"}}); got != "user_id=a%20b~._-" {
		t.Fatalf("got %q", got)
	}
	if got := CanonicalFilter(url.Values{"limit": {"1"}}); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestCursorRejects(t *testing.T) {
	f, k, b, _ := fixtureRing(t)
	c := f.Cursor.Cursor
	parts := strings.Split(c, ".")
	at := time.Unix(f.Cursor.Now, 0)
	flip := func(s string) string { // change the first character, staying in base64url
		r := byte('A')
		if s[0] == 'A' {
			r = 'B'
		}
		return string(r) + s[1:]
	}
	other := Keyring{Current: "other", Keys: map[string][]byte{"other": []byte(strings.Repeat("k", 32))}}
	for kid, key := range k.Keys {
		other.Keys[kid] = key
	}
	// Valid MAC over a payload that breaks a payload rule.
	forge := func(payloadJSON string) string {
		p := base64.RawURLEncoding.EncodeToString([]byte(payloadJSON))
		kid := f.Cursor.Kid
		return kid + "." + p + "." + cursorTag(k.Keys[kid], kid, p)
	}
	good := f.Cursor.PayloadJSON
	cases := []struct {
		name   string
		ring   Keyring
		cursor string
		b      Binding
		now    time.Time
	}{
		{"kid tampered", k, "x" + c, b, at},
		{"payload tampered", k, parts[0] + "." + flip(parts[1]) + "." + parts[2], b, at},
		{"tag tampered", k, parts[0] + "." + parts[1] + "." + flip(parts[2]), b, at},
		{"tag truncated", k, c[:len(c)-1], b, at},
		{"tag padded", k, c + "=", b, at},
		{"unknown kid", Keyring{Current: "k2", Keys: map[string][]byte{"k2": k.Keys[f.Cursor.Kid]}}, c, b, at},
		{"kid of another known key", other, "other." + parts[1] + "." + parts[2], b, at},
		{"two parts", k, parts[0] + "." + parts[1], b, at},
		{"four parts", k, c + ".x", b, at},
		{"empty", k, "", b, at},
		{"expired at exp", k, c, b, time.Unix(f.Cursor.Exp, 0)},
		{"principal", k, c, Binding{"user-x", b.Resource, b.BucketID, b.Filter}, at},
		{"resource", k, c, Binding{b.Principal, "/admin/v1/buckets/demo2/grants", b.BucketID, b.Filter}, at},
		{"bucket_id", k, c, Binding{b.Principal, b.Resource, "bkt-demo-0002", b.Filter}, at},
		{"filter", k, c, Binding{b.Principal, b.Resource, b.BucketID, ""}, at},
		{"version 2", k, forge(strings.Replace(good, `"v":1`, `"v":2`, 1)), b, at},
		{"unknown field", k, forge(strings.Replace(good, `{"v":1`, `{"v":1,"x":1`, 1)), b, at},
		{"trailing data", k, forge(good + "{}"), b, at},
		{"not json", k, forge("null"), b, at},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pos, err := tc.ring.Open(tc.cursor, tc.b, tc.now)
			if !errors.Is(err, ErrInvalidCursor) || pos != "" {
				t.Fatalf("Open = %q, %v; want ErrInvalidCursor", pos, err)
			}
		})
	}
}

func TestCursorRotation(t *testing.T) {
	f, old, b, now := fixtureRing(t)
	rotated := Keyring{Current: "k2026-11", Keys: map[string][]byte{
		f.Cursor.Kid: old.Keys[f.Cursor.Kid],
		"k2026-11":   []byte(strings.Repeat("n", 32)),
	}}
	if pos, err := rotated.Open(f.Cursor.Cursor, b, now); err != nil || pos != f.Cursor.Pos {
		t.Fatalf("old cursor after rotation = %q, %v", pos, err)
	}
	c, err := rotated.Seal(b, "p2", now)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(c, "k2026-11.") {
		t.Fatalf("sealed with %q, want new kid", c)
	}
	if _, err := old.Open(c, b, now); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("keyring without new key opened it: %v", err)
	}
	retired := Keyring{Current: "k2026-11", Keys: map[string][]byte{"k2026-11": rotated.Keys["k2026-11"]}}
	if _, err := retired.Open(f.Cursor.Cursor, b, now); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("retired key still verifies: %v", err)
	}
}

func TestKeyringValidate(t *testing.T) {
	key := []byte(strings.Repeat("k", 32))
	bad := map[string]Keyring{
		"missing current": {Current: "a", Keys: map[string][]byte{"b": key}},
		"short key":       {Current: "a", Keys: map[string][]byte{"a": key[:31]}},
		"uppercase kid":   {Current: "A", Keys: map[string][]byte{"A": key}},
		"dot in kid":      {Current: "a.b", Keys: map[string][]byte{"a.b": key}},
		"kid too long":    {Current: strings.Repeat("a", 33), Keys: map[string][]byte{strings.Repeat("a", 33): key}},
	}
	for name, k := range bad {
		if _, err := k.Seal(Binding{}, "", time.Unix(0, 0)); err == nil {
			t.Errorf("%s: Seal accepted invalid keyring", name)
		}
	}
	if err := (Keyring{Current: strings.Repeat("a", 32), Keys: map[string][]byte{strings.Repeat("a", 32): key}}).Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestTokenVector(t *testing.T) {
	tk := loadWire(t).Token
	if got := hex.EncodeToString(mustHex(t, tk.TokenIDBytesHex)); got != tk.TokenID {
		t.Fatalf("token_id = %q", got)
	}
	if got := base64.RawURLEncoding.EncodeToString(mustHex(t, tk.SecretBytesHex)); got != tk.Secret {
		t.Fatalf("secret = %q", got)
	}
	if got := SecretHash(tk.Secret); got != tk.SecretHash {
		t.Fatalf("SecretHash = %q, want %q", got, tk.SecretHash)
	}
	id, secret, ok := ParseBearer([]string{"Bearer " + tk.Token})
	if !ok || id != tk.TokenID || secret != tk.Secret {
		t.Fatalf("ParseBearer = %q, %q, %v", id, secret, ok)
	}
	if !VerifySecret(tk.SecretHash, tk.Secret) {
		t.Fatal("VerifySecret rejected the vector")
	}
	wrong := "J" + tk.Secret[1:]
	if VerifySecret(tk.SecretHash, wrong) || VerifySecret(strings.ToUpper(tk.SecretHash), tk.Secret) {
		t.Fatal("VerifySecret accepted a wrong secret or non-canonical hash")
	}
}

func TestNewToken(t *testing.T) {
	id, secret, token, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	pid, psecret, ok := ParseBearer([]string{"Bearer " + token})
	if !ok || pid != id || psecret != secret || token != id+"."+secret {
		t.Fatalf("NewToken output does not parse back: %q", token)
	}
	id2, _, _, _ := NewToken()
	if id2 == id {
		t.Fatal("two tokens share token_id")
	}
}

func TestParseBearer(t *testing.T) {
	tk := loadWire(t).Token
	tok := tk.Token
	// 43 chars whose last character carries non-zero padding bits.
	nonCanonical := tk.TokenID + "." + tk.Secret[:42] + "9"
	cases := []struct {
		name   string
		values []string
		ok     bool
	}{
		{"canonical", []string{"Bearer " + tok}, true},
		{"lowercase scheme", []string{"bearer " + tok}, true},
		{"mixed scheme", []string{"BeArEr " + tok}, true},
		{"no header", nil, false},
		{"two headers", []string{"Bearer " + tok, "Bearer " + tok}, false},
		{"two spaces", []string{"Bearer  " + tok}, false},
		{"tab", []string{"Bearer\t" + tok}, false},
		{"trailing space", []string{"Bearer " + tok + " "}, false},
		{"leading space", []string{" Bearer " + tok}, false},
		{"basic scheme", []string{"Basic " + tok}, false},
		{"no scheme", []string{tok}, false},
		{"uppercase token_id", []string{"Bearer " + strings.ToUpper(tk.TokenID) + "." + tk.Secret}, false},
		{"short token_id", []string{"Bearer " + tk.TokenID[1:] + "." + tk.Secret}, false},
		{"non-hex token_id", []string{"Bearer g" + tk.TokenID[1:] + "." + tk.Secret}, false},
		{"no dot", []string{"Bearer " + tk.TokenID + tk.Secret}, false},
		{"short secret", []string{"Bearer " + tk.TokenID + "." + tk.Secret[:42]}, false},
		{"padded secret", []string{"Bearer " + tok + "="}, false},
		{"std base64 secret", []string{"Bearer " + tk.TokenID + "." + tk.Secret[:42] + "+"}, false},
		{"non-canonical secret", []string{"Bearer " + nonCanonical}, false},
		{"extra dot", []string{"Bearer " + tok + ".x"}, false},
		{"comma list", []string{"Bearer " + tok + ", Bearer " + tok}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, secret, ok := ParseBearer(tc.values)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if ok && (id != tk.TokenID || secret != tk.Secret) {
				t.Fatalf("parsed %q, %q", id, secret)
			}
			if !ok && (id != "" || secret != "") {
				t.Fatalf("failure leaked %q, %q", id, secret)
			}
		})
	}
}

func TestPrecondition(t *testing.T) {
	u := func(v uint64) *uint64 { return &v }
	const maxS = "9007199254740991"
	cases := []struct {
		name     string
		expected *uint64
		ifMatch  []string
		create   bool
		want     uint64
		ok       bool
	}{
		{"body only", u(3), nil, false, 3, true},
		{"body zero creates a grant", u(0), nil, true, 0, true},
		{"body zero on a resource that always exists", u(0), nil, false, 0, false},
		{"body max", u(MaxRevision), nil, false, MaxRevision, true},
		{"body above max", u(MaxRevision + 1), nil, true, 0, false},
		{"if-match only", nil, []string{`"3"`}, false, 3, true},
		{"if-match max", nil, []string{`"` + maxS + `"`}, false, MaxRevision, true},
		{"both equal", u(7), []string{`"7"`}, false, 7, true},
		{"both different", u(7), []string{`"8"`}, false, 0, false},
		{"body zero with if-match", u(0), []string{`"1"`}, true, 0, false},
		{"neither", nil, nil, true, 0, false},
		{"empty header list", nil, []string{}, false, 0, false},
		{"weak", nil, []string{`W/"3"`}, false, 0, false},
		{"star", nil, []string{"*"}, false, 0, false},
		{"quoted star", nil, []string{`"*"`}, false, 0, false},
		{"list in one header", nil, []string{`"3", "4"`}, false, 0, false},
		{"two headers", nil, []string{`"3"`, `"3"`}, false, 0, false},
		{"leading zero", nil, []string{`"03"`}, false, 0, false},
		{"zero", nil, []string{`"0"`}, true, 0, false},
		{"plus sign", nil, []string{`"+3"`}, false, 0, false},
		{"unquoted", nil, []string{"3"}, false, 0, false},
		{"empty quotes", nil, []string{`""`}, false, 0, false},
		{"empty value", nil, []string{""}, false, 0, false},
		{"above max", nil, []string{`"9007199254740992"`}, false, 0, false},
		{"uint64 overflow", nil, []string{`"18446744073709551616"`}, false, 0, false},
		{"space inside", nil, []string{`" 3"`}, false, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Precondition(tc.expected, tc.ifMatch, tc.create)
			if tc.ok {
				if err != nil || got != tc.want {
					t.Fatalf("= %d, %v; want %d", got, err, tc.want)
				}
				return
			}
			if !errors.Is(err, ErrPrecondition) {
				t.Fatalf("= %d, %v; want ErrPrecondition", got, err)
			}
		})
	}
}

func TestETagRoundTrip(t *testing.T) {
	for _, rev := range []uint64{1, 42, MaxRevision} {
		if got, err := Precondition(nil, []string{ETag(rev)}, false); err != nil || got != rev {
			t.Fatalf("ETag(%d) = %s does not round-trip: %d, %v", rev, ETag(rev), got, err)
		}
	}
}
