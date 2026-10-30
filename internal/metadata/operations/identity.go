package operations

import (
	"errors"
	"time"

	s "github.com/KinasTomes/miles/internal/storage"
)

// TokenActor resolves an Admin bearer inside the write transaction. Unknown,
// revoked or expired tokens and disabled users are all ErrAccessDenied.
func TokenActor(t Tx, now time.Time, tokenID string) (s.AdminToken, error) {
	token, err := get[s.AdminToken](t, "admin-tokens", tokenID)
	if errors.Is(err, s.ErrNotFound) || err == nil && !token.Usable(now) {
		return s.AdminToken{}, s.ErrAccessDenied
	}
	if err != nil {
		return s.AdminToken{}, err
	}
	user, err := get[s.User](t, "users", token.UserID)
	if errors.Is(err, s.ErrNotFound) || err == nil && !user.Active {
		return s.AdminToken{}, s.ErrAccessDenied
	}
	if err != nil {
		return s.AdminToken{}, err
	}
	return token, nil
}

// SystemActor authorizes a system-wide write (global policy): exactly one of
// an active system-admin credential or a usable system-admin token.
func SystemActor(t Tx, now time.Time, credentialID, tokenID string) error {
	if (credentialID == "") == (tokenID == "") {
		return s.ErrInvalid
	}
	if tokenID != "" {
		token, err := TokenActor(t, now, tokenID)
		if err != nil {
			return err
		}
		if token.Scope != s.TokenScopeSystem {
			return s.ErrAccessDenied
		}
		return nil
	}
	c, err := get[s.Credential](t, "credentials", credentialID)
	if errors.Is(err, s.ErrNotFound) || err == nil && (!c.Active || !c.SystemAdmin) {
		return s.ErrAccessDenied
	}
	if err != nil {
		return err
	}
	u, err := get[s.User](t, "users", c.UserID)
	if errors.Is(err, s.ErrNotFound) || err == nil && !u.Active {
		return s.ErrAccessDenied
	}
	return err
}
