// Package admin holds the wire rules of the Admin API (API §6.5) that the
// W04 routes share: bearer tokens (§6.4), pagination cursors (§6.5.3) and
// revision preconditions (§6.5.2). Routes, handlers, JSON body decoding and
// metadata access are W04 and live elsewhere.
package admin

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"strings"
)

const (
	tokenIDLen = 32 // lowercase hex of 16 CSPRNG bytes
	secretLen  = 43 // base64url without padding of 32 CSPRNG bytes
)

// NewToken mints a bearer token "<token_id>.<secret>" (API §6.4).
// Callers store SecretHash(secret) and return token to the client once.
func NewToken() (tokenID, secret, token string, err error) {
	var b [16 + 32]byte
	if _, err = rand.Read(b[:]); err != nil {
		return "", "", "", err
	}
	tokenID = hex.EncodeToString(b[:16])
	secret = base64.RawURLEncoding.EncodeToString(b[16:])
	return tokenID, secret, tokenID + "." + secret, nil
}

// ParseBearer parses the values of the Authorization header (API §6.4):
// exactly one header, scheme "Bearer" in any case, one space, then a token
// whose token_id is 32 lowercase hex and whose secret strict-decodes
// (RawURLEncoding) to 32 bytes. Any failure is ok=false, which the caller
// maps to 401 unauthorized without saying why.
func ParseBearer(values []string) (tokenID, secret string, ok bool) {
	if len(values) != 1 {
		return "", "", false
	}
	scheme, rest, found := strings.Cut(values[0], " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", "", false
	}
	tokenID, secret, found = strings.Cut(rest, ".")
	if !found || !validTokenID(tokenID) || len(secret) != secretLen {
		return "", "", false
	}
	if b, err := base64.RawURLEncoding.Strict().DecodeString(secret); err != nil || len(b) != 32 {
		return "", "", false
	}
	return tokenID, secret, true
}

func validTokenID(s string) bool {
	if len(s) != tokenIDLen {
		return false
	}
	for i := range len(s) {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// SecretHash is the AdminTokens.secret_hash of a secret: lowercase hex
// SHA-256 of its 43 ASCII characters.
func SecretHash(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// VerifySecret compares SecretHash(secret) with the stored hash in
// constant time. Status, expiry and user checks are the caller's.
func VerifySecret(storedHash, secret string) bool {
	return subtle.ConstantTimeCompare([]byte(storedHash), []byte(SecretHash(secret))) == 1
}
