package app

import (
	"encoding/base64"
	"fmt"
	"io"

	"github.com/KinasTomes/miles/internal/storage"
)

// DevUser is the single user of the dev fixtures: it owns the probe bucket
// and every dev credential below belongs to it.
const DevUser = "dev"

// DevCredential is one SigV4 test credential minted by dev provisioning, in
// the API v2 3.5 format: a 20-character [A-Z0-9] access key and the base64 of
// 30 CSPRNG bytes as the secret. The bytes come from the process entropy
// source at startup, so no secret is ever baked into the repository; the pair
// is bound to DevUser with no system-admin flag. Runtime mode mints nothing.
type DevCredential struct {
	AccessKeyID string
	Secret      string
	UserID      string
}

const (
	devAccessKeyLen = 20
	devSecretBytes  = 30
)

const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// alphabetMax is the largest byte multiple of the alphabet size; bytes above
// it are rejected so every character stays equally likely.
const alphabetMax = 252

func randomAlphabet(r io.Reader, n int) (string, error) {
	out := make([]byte, 0, n)
	buf := make([]byte, n*2)
	for len(out) < n {
		if _, err := io.ReadFull(r, buf); err != nil {
			return "", fmt.Errorf("dev credential: %w", err)
		}
		for _, b := range buf {
			if b > alphabetMax {
				continue
			}
			out = append(out, alphabet[int(b)%len(alphabet)])
			if len(out) == n {
				break
			}
		}
	}
	return string(out), nil
}

// newDevCredential generates one credential for userID from r, which tests
// replace with a deterministic reader.
func newDevCredential(r io.Reader, userID string) (DevCredential, error) {
	key, err := randomAlphabet(r, devAccessKeyLen)
	if err != nil {
		return DevCredential{}, err
	}
	secret := make([]byte, devSecretBytes)
	if _, err := io.ReadFull(r, secret); err != nil {
		return DevCredential{}, fmt.Errorf("dev credential: %w", err)
	}
	return DevCredential{AccessKeyID: key, Secret: base64.StdEncoding.EncodeToString(secret), UserID: userID}, nil
}

// devFixtureIdentity is the authoritative identity the dev store seeds: the
// dev user, the public credential record for the store and the minted pair
// with its secret, returned once here and never logged — the S3 wiring that
// follows hands it to the client side it belongs to.
func devFixtureIdentity(r io.Reader) (storage.User, storage.Credential, DevCredential, error) {
	cred, err := newDevCredential(r, DevUser)
	if err != nil {
		return storage.User{}, storage.Credential{}, DevCredential{}, err
	}
	user := storage.User{ID: DevUser, Active: true, Revision: 1, DefaultDomain: "default"}
	return user, storage.Credential{ID: cred.AccessKeyID, UserID: DevUser, Active: true, Revision: 1}, cred, nil
}
