package dynamodb

import (
	"context"
	"errors"
	"testing"
	"time"

	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/KinasTomes/miles/internal/storage/contracttest"
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
}
