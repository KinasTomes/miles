package dynamodb

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/KinasTomes/miles/internal/storage/contracttest"
	"github.com/aws/aws-sdk-go-v2/aws"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

func revokeRequest(t *testing.T, m *Store, f contracttest.Fixture, r s.CommitRequest, grant bool) {
	t.Helper()
	if err := m.update(context.Background(), func(tx *transaction) error {
		if grant {
			g := f.Grant
			g.Write = false
			g.Revision++
			return put(tx, key("grants", g.BucketID, g.UserID), g)
		}
		c, err := get[s.Credential](tx, key("credentials", r.CredentialID))
		if err != nil {
			return err
		}
		c.Active = false
		c.Revision++
		return put(tx, key("credentials", c.ID), c)
	}); err != nil {
		t.Fatal(err)
	}
}

type registrationClock struct {
	API
	advance func()
}

func (a *registrationClock) TransactWriteItems(ctx context.Context, in *ddb.TransactWriteItemsInput, opts ...func(*ddb.Options)) (*ddb.TransactWriteItemsOutput, error) {
	out, err := a.API.TransactWriteItems(ctx, in, opts...)
	for _, action := range in.TransactItems {
		if action.Put != nil && action.Put.Item["pending_request"] != nil {
			a.advance()
		}
	}
	return out, err
}

func TestLocalExpiryDuringAttemptRegistrationClosesWithoutPublishing(t *testing.T) {
	ctx := context.Background()
	f := contracttest.NewFixture()
	f.Lease.ExpiresAt = f.Now.Add(2 * time.Hour)
	m, _ := fixtureStore(t, f)
	now := f.Now
	m.now = func() time.Time { return now }
	m.authority = fixtureAuthority{m.now}
	r := f.Request("registration-expiry", 0)
	contracttest.Prepare(t, m, f, r, 4)
	m.api = &registrationClock{API: m.api, advance: func() { now = f.Now.Add(30 * time.Minute) }}
	if _, err := m.CommitObject(ctx, r); !errors.Is(err, s.ErrFenced) {
		t.Fatal(err)
	}
	contracttest.NoPublication(t, m, r)
	if err := m.ReleaseLogicalQuota(ctx, s.ReservationRelease{ID: r.ReservationID, OpID: r.Head.OpID, ExpectedRevision: 1, Writer: r.Writer}); err != nil {
		t.Fatal("expired pending was not safely closed", err)
	}
}

type finalSDK struct {
	API
	client      *ddb.Client
	objectTable string
}

func (a *finalSDK) TransactWriteItems(ctx context.Context, in *ddb.TransactWriteItemsInput, opts ...func(*ddb.Options)) (*ddb.TransactWriteItemsOutput, error) {
	for _, action := range in.TransactItems {
		if action.Put != nil && *action.Put.TableName == a.objectTable {
			return a.client.TransactWriteItems(ctx, in, opts...)
		}
	}
	return a.API.TransactWriteItems(ctx, in, opts...)
}

func TestLocalFinalSDKCannotRetryBehindDeadlineGuard(t *testing.T) {
	ctx := context.Background()
	f := contracttest.NewFixture()
	m, _ := fixtureStore(t, f)
	r := f.Request("no-hidden-retry", 0)
	contracttest.Prepare(t, m, f, r, 4)
	calls := 0
	sdk := ddb.New(ddb.Options{Region: "us-east-1", Credentials: aws.AnonymousCredentials{}, HTTPClient: wireClient(func(request *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 500, Header: http.Header{"Content-Type": []string{"application/x-amz-json-1.0"}}, Body: io.NopCloser(strings.NewReader(`{"__type":"InternalServerError","message":"injected"}`)), Request: request}, nil
	})})
	m.api = &finalSDK{API: m.api, client: sdk, objectTable: m.tables["Objects"]}
	if _, err := m.CommitObject(ctx, r); !errors.Is(err, s.ErrUnavailable) {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("SDK bypassed guard with hidden retries", calls)
	}
	if err := m.ReleaseLogicalQuota(ctx, s.ReservationRelease{ID: r.ReservationID, OpID: r.Head.OpID, ExpectedRevision: 1, Writer: r.Writer}); !errors.Is(err, s.ErrUnavailable) {
		t.Fatal("unknown send released quota", err)
	}
}

func TestLocalRevocationClosesPendingBeforeCleanup(t *testing.T) {
	for _, grant := range []bool{false, true} {
		name := "credential"
		if grant {
			name = "grant"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			f := contracttest.NewFixture()
			m, _ := fixtureStore(t, f)
			r := f.Request("revoked-cleanup", 0)
			contracttest.Prepare(t, m, f, r, 4)
			m.api = &dispatchFault{API: m.api, objectTable: m.tables["Objects"], before: true}
			if _, err := m.CommitObject(ctx, r); !errors.Is(err, s.ErrUnavailable) {
				t.Fatal(err)
			}
			revokeRequest(t, m, f, r, grant)
			if _, err := restartFixture(t, m).CommitObject(ctx, r); !errors.Is(err, s.ErrAccessDenied) {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				if err := m.ReleaseLogicalQuota(ctx, s.ReservationRelease{ID: r.ReservationID, OpID: r.Head.OpID, ExpectedRevision: 1, Writer: r.Writer}); err != nil {
					t.Fatal("cleanup still depends on revoked credential", err)
				}
			}
			q, err := m.GetQuota(ctx, f.Quota.OwnerID)
			if err != nil || q.Used != 0 || q.Reserved != 0 || q.Staged != 0 {
				t.Fatal(q, err)
			}
			contracttest.NoPublication(t, m, r)
		})
	}
}

type verificationClock struct {
	API
	table   string
	advance func()
}

func (a *verificationClock) Query(ctx context.Context, in *ddb.QueryInput, opts ...func(*ddb.Options)) (*ddb.QueryOutput, error) {
	out, err := a.API.Query(ctx, in, opts...)
	if in.TableName != nil && *in.TableName == a.table && in.Limit != nil && *in.Limit == 1 {
		a.advance()
	}
	return out, err
}

func TestLocalExpiryBlocksFirstPublication(t *testing.T) {
	for _, replay := range []bool{false, true} {
		name := "verification"
		if replay {
			name = "pending-replay"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			f := contracttest.NewFixture()
			f.Lease.ExpiresAt = f.Now.Add(2 * time.Hour)
			m, _ := fixtureStore(t, f)
			now := f.Now
			m.now = func() time.Time { return now }
			m.authority = fixtureAuthority{m.now}
			r := f.Request("expired-first-publication", 0)
			contracttest.Prepare(t, m, f, r, 4)
			if replay {
				m.api = &dispatchFault{API: m.api, objectTable: m.tables["Objects"], before: true}
				if _, err := m.CommitObject(ctx, r); !errors.Is(err, s.ErrUnavailable) {
					t.Fatal(err)
				}
				now = f.Now.Add(30 * time.Minute)
			} else {
				m.api = &verificationClock{API: m.api, table: m.tables["ManifestPages"], advance: func() { now = f.Now.Add(30 * time.Minute) }}
			}
			if _, err := restartFixture(t, m).CommitObject(ctx, r); !errors.Is(err, s.ErrFenced) {
				t.Fatal("expired operation published", err)
			}
			contracttest.NoPublication(t, m, r)
		})
	}
}

func TestLocalCommittedOutcomeSurvivesRevocationAndExpiry(t *testing.T) {
	ctx := context.Background()
	f := contracttest.NewFixture()
	f.Lease.ExpiresAt = f.Now.Add(2 * time.Hour)
	m, _ := fixtureStore(t, f)
	r := f.Request("committed-before-expiry", 0)
	contracttest.Prepare(t, m, f, r, 4)
	fault := &dispatchFault{API: m.api, objectTable: m.tables["Objects"], after: true}
	m.api = fault
	if _, err := m.CommitObject(ctx, r); !errors.Is(err, s.ErrUnavailable) {
		t.Fatal(err)
	}
	revokeRequest(t, m, f, r, false)
	m.now = func() time.Time { return f.Now.Add(time.Hour) }
	m.authority = fixtureAuthority{m.now}
	before := len(fault.requests)
	result, err := restartFixture(t, m).CommitObject(ctx, r)
	if err != nil || !result.AlreadyCommitted || result.Head.OpID != r.Head.OpID || len(fault.requests) != before {
		t.Fatal("original outcome lost", result, err)
	}
	q, err := m.GetQuota(ctx, f.Quota.OwnerID)
	if err != nil || q.Used != 4 || q.Reserved != 0 || q.Staged != 0 {
		t.Fatal(q, err)
	}
	m.now = func() time.Time { return f.Now.Add(3 * time.Hour) }
	m.authority = fixtureAuthority{m.now}
	if _, err := m.CommitObject(ctx, r); !errors.Is(err, s.ErrFenced) {
		t.Fatal("expired execution writer bypassed shared fence", err)
	}
	retained, err := m.GetCommitOutcome(ctx, r.Head.OpID)
	if err != nil || retained.Head.OpID != r.Head.OpID || !retained.AlreadyCommitted {
		t.Fatal("reconciliation lost result after writer expiry", retained, err)
	}
}
