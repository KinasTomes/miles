package main

import (
	"context"
	"net/http"
	"time"

	api "github.com/KinasTomes/miles/internal/api/s3"
	"github.com/KinasTomes/miles/internal/app"
	"github.com/KinasTomes/miles/internal/auth"
	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/storage"
)

// RequestTimeout is the immutable hard budget from Clock/Policy §5, not an idle timeout.
const RequestTimeout = 30 * time.Minute

type devCredentials struct{ d app.Dependencies }

// Credential prefers the dev-minted secret, then the sealed secret of an
// Admin-created credential (local/benchmark). Secrets are never logged.
func (c devCredentials) Credential(ctx context.Context, id string) (api.Credential, error) {
	record, err := c.d.Metadata.GetCredential(ctx, id)
	if err != nil {
		return api.Credential{}, err
	}
	credential := api.Credential{UserID: record.UserID, Active: record.Active, SystemAdmin: record.SystemAdmin, DebugAllowed: record.DebugAllowed}
	for _, v := range c.d.DevCredentials {
		if v.AccessKeyID == id && v.UserID == record.UserID {
			credential.Secret = v.Secret
			return credential, nil
		}
	}
	if c.d.Admin == nil {
		return api.Credential{}, storage.ErrNotFound
	}
	sec, err := c.d.Admin.Store.GetCredentialSecurity(ctx, id)
	if err != nil {
		return api.Credential{}, err
	}
	secret, err := c.d.Admin.Sealer.Open(ctx, id, sec)
	if err != nil {
		return api.Credential{}, err
	}
	credential.Secret = string(secret)
	return credential, nil
}

func s3Handler(region string, d app.Dependencies, a *app.Admission) http.Handler {
	clk := clock.NewReal()
	handlers := api.NewBucketHandlers(d.Metadata)
	if d.Objects != nil {
		for op, handler := range api.NewObjectHandlers(d.Objects, d.Objects, d.Objects) {
			handlers[op] = handler
		}
	}
	rt := &api.Router{Region: region, Now: func() time.Time { return clk.Now().WallUTC }, Credentials: devCredentials{d}, Access: auth.NewSnapshots(d.Metadata, clk), Handlers: handlers}
	return requestGate(a, RequestTimeout, rt, s3Unavailable)
}

func s3Unavailable(w http.ResponseWriter, r *http.Request) {
	_ = api.WriteError(w, r, &api.Error{Code: api.ErrServiceUnavailable}, api.NewRequestID())
}

// Admission does not consult cached readiness. Each handler still owns auth,
// current dependency checks and R1. Release only after the handler is quiescent.
// reject writes the protocol's 503 when admission is closed.
func requestGate(a *app.Admission, budget time.Duration, next http.Handler, reject http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), budget)
		defer cancel()
		deadline, _ := ctx.Deadline()
		rc := http.NewResponseController(w)
		// These bound socket I/O too; WithTimeout alone cannot interrupt a blocked body read.
		_ = rc.SetReadDeadline(deadline)
		_ = rc.SetWriteDeadline(deadline)
		release, ok := a.Acquire()
		if !ok || ctx.Err() != nil {
			if ok {
				release()
			}
			w.Header().Set("Connection", "close")
			reject(w, r)
			return
		}
		defer release()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
