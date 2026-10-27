package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/KinasTomes/miles/internal/config"
)

// TestDevCredentialFormat pins the API v2 3.5 shape of a minted credential:
// 20 characters of [A-Z0-9] and a base64 secret of exactly 30 CSPRNG bytes,
// bound to the dev user. A deterministic reader proves both halves come from
// the entropy source and nowhere else.
func TestDevCredentialFormat(t *testing.T) {
	ones := bytes.NewReader(bytes.Repeat([]byte{1}, 128))
	cred, err := newDevCredential(ones, DevUser)
	if err != nil {
		t.Fatal(err)
	}
	if len(cred.AccessKeyID) != 20 {
		t.Fatalf("access key %q is not 20 characters", cred.AccessKeyID)
	}
	for _, c := range cred.AccessKeyID {
		if !(c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			t.Fatalf("access key %q has %q outside [A-Z0-9]", cred.AccessKeyID, c)
		}
	}
	secret, err := base64.StdEncoding.DecodeString(cred.Secret)
	if err != nil || len(secret) != 30 {
		t.Fatalf("secret is not the base64 of 30 bytes: %d %v", len(secret), err)
	}
	if bytes.Contains(secret, []byte{0xff}) {
		t.Fatal("secret bytes were not taken from the reader")
	}
	if cred.UserID != DevUser {
		t.Fatalf("credential bound to %q, want %q", cred.UserID, DevUser)
	}
}

// TestRandomAlphabetRejectsBoundary pins rejection sampling: 252 = 7*36 is the
// first byte that would bias the alphabet, so it is dropped like 255.
func TestRandomAlphabetRejectsBoundary(t *testing.T) {
	src := append(bytes.Repeat([]byte{252}, 40), bytes.Repeat([]byte{1}, 40)...)
	got, err := randomAlphabet(bytes.NewReader(src), 20)
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.Repeat("B", 20); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestDevProvisioningIsPerProcessAndAuthoritative runs Bootstrap twice: each
// process mints a different credential (nothing static in the repo), the
// store holds the public record bound to the dev user, and the pair really
// owns the probe bucket. Runtime mode mints nothing.
func TestDevProvisioningIsPerProcessAndAuthoritative(t *testing.T) {
	c := config.DefaultStartup()
	c.Mode = "dev"
	first, err := Bootstrap(c)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.DevCredentials) != 1 {
		t.Fatalf("dev mints %d credentials, want 1", len(first.DevCredentials))
	}
	cred := first.DevCredentials[0]
	second, err := Bootstrap(c)
	if err != nil {
		t.Fatal(err)
	}
	if second.DevCredentials[0].AccessKeyID == cred.AccessKeyID || second.DevCredentials[0].Secret == cred.Secret {
		t.Fatal("two processes minted the same credential")
	}
	ctx := context.Background()
	stored, err := first.Metadata.GetCredential(ctx, cred.AccessKeyID)
	if err != nil {
		t.Fatal(err)
	}
	if !stored.Active || stored.UserID != DevUser || stored.SystemAdmin {
		t.Fatalf("stored credential=%+v is not an active, non-admin credential of %s", stored, DevUser)
	}
	a, err := first.Metadata.Authorize(ctx, cred.AccessKeyID, "miles-dev-probe")
	if err != nil || !a.Read || !a.Write || a.SystemAdmin {
		t.Fatalf("dev credential does not own the probe bucket: %+v %v", a, err)
	}
	if a.Bucket.Domain != "default" || a.Bucket.OwnerID != DevUser {
		t.Fatalf("probe bucket=%+v", a.Bucket)
	}

	runtime, err := Bootstrap(config.DefaultStartup())
	if err != nil {
		t.Fatal(err)
	}
	if len(runtime.DevCredentials) != 0 {
		t.Fatal("runtime mode minted dev credentials")
	}
}
