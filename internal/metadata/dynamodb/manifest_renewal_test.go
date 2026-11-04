package dynamodb

import (
	"context"
	"errors"
	"testing"
	"time"

	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/KinasTomes/miles/internal/storage/contracttest"
	"github.com/aws/aws-sdk-go-v2/aws"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

type manifestVerificationInterleave struct {
	API
	table      string
	afterQuery func()
}

func (a *manifestVerificationInterleave) Query(ctx context.Context, in *ddb.QueryInput, options ...func(*ddb.Options)) (*ddb.QueryOutput, error) {
	out, err := a.API.Query(ctx, in, options...)
	if err == nil && aws.ToString(in.TableName) == a.table && a.afterQuery != nil {
		action := a.afterQuery
		a.afterQuery = nil
		action()
	}
	return out, err
}

func TestLocalManifestVerificationRechecksLiveAuthorityAndFrozenOwner(t *testing.T) {
	for _, mode := range []string{"renew", "gate_membership", "closed_writer", "changed_owner"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			m, peer, f, utc := sharedFixture(t)
			r := f.Request("verified-cas", 0)
			r.Head.Size = 0
			admission := r1Request(f, "verified-cas", 1)
			admission.Object = r.Head.Object
			r.Writer = admission.Fence
			if _, err := m.AcquireR1(ctx, admission); err != nil {
				t.Fatal(err)
			}
			if err := m.BindR1(ctx, r.Writer, r.Head); err != nil {
				t.Fatal(err)
			}
			called := false
			seam := &manifestVerificationInterleave{API: m.api, table: m.tables["ManifestPages"]}
			seam.afterQuery = func() {
				called = true
				var err error
				switch mode {
				case "renew":
					utc.Advance(15 * time.Second)
					_, err = peer.RenewR1(ctx, r.Writer)
				case "gate_membership":
					other := admission
					other.Fence.LeaseID = "other-writer"
					_, err = peer.AcquireR1(ctx, other)
				case "closed_writer":
					err = peer.CloseR1(ctx, r.Writer)
				case "changed_owner":
					err = peer.update(ctx, func(tx *transaction) error {
						owner, e := get[manifestOwner](tx, key("manifest-owner", r.Head.ManifestID))
						if e != nil {
							return e
						}
						owner.State = "Building"
						if e = advanceOwner(&owner); e != nil {
							return e
						}
						return put(tx, key("manifest-owner", r.Head.ManifestID), owner)
					})
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			m.api = seam
			err := m.prepareManifest(ctx, r.Writer, &r.Head)
			if !called {
				t.Fatal("verification interleave did not execute")
			}
			switch mode {
			case "renew", "gate_membership":
				if err != nil {
					t.Fatal("live authority change rejected verification", err)
				}
			case "closed_writer":
				if !errors.Is(err, s.ErrFenced) {
					t.Fatal("closed writer verified manifest", err)
				}
			case "changed_owner":
				if !errors.Is(err, s.ErrConflict) {
					t.Fatal("changed frozen owner accepted", err)
				}
			}
			owner, e := read[manifestOwner](peer, ctx, key("manifest-owner", r.Head.ManifestID))
			if e != nil {
				t.Fatal(e)
			}
			if (owner.State == "Verified") != (mode == "renew" || mode == "gate_membership") {
				t.Fatal("unexpected owner state", owner.State)
			}
		})
	}
}

func TestLocalCommitPublishesAfterManifestScanOutlastsInitialLease(t *testing.T) {
	for _, mode := range []string{"renewed", "revoked", "expired"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			m, peer, f, utc := sharedFixture(t)
			m.authority.(*SharedAuthority).Lifetime = time.Minute
			peer.authority.(*SharedAuthority).Lifetime = time.Minute
			r := f.Request("slow-commit", 0)
			admission := r1Request(f, "slow-commit", 1)
			admission.Object, admission.OpID = r.Head.Object, r.Head.OpID
			r.Writer, f.Lease.Fence = admission.Fence, admission.Fence
			if _, err := m.AcquireR1(ctx, admission); err != nil {
				t.Fatal(err)
			}
			if err := m.BindR1(ctx, r.Writer, r.Head); err != nil {
				t.Fatal(err)
			}
			contracttest.Prepare(t, m, f, r, r.Head.Size)
			scanned := false
			m.api = &manifestVerificationInterleave{API: m.api, table: m.tables["ManifestPages"], afterQuery: func() {
				scanned = true
				for range 5 {
					utc.Advance(15 * time.Second)
					if mode != "expired" {
						if _, err := peer.RenewR1(ctx, r.Writer); err != nil {
							t.Fatal(err)
						}
					}
				}
				if mode == "revoked" {
					if err := peer.update(ctx, func(tx *transaction) error {
						credential, err := get[s.Credential](tx, key("credentials", r.CredentialID))
						if err != nil {
							return err
						}
						credential.Active = false
						credential.Revision++
						return put(tx, key("credentials", r.CredentialID), credential)
					}); err != nil {
						t.Fatal(err)
					}
				}
			}}
			result, err := m.CommitObject(ctx, r)
			if !scanned {
				t.Fatal("manifest verification did not run")
			}
			if mode != "renewed" {
				want := s.ErrAccessDenied
				if mode == "expired" {
					want = s.ErrFenced
				}
				if !errors.Is(err, want) {
					t.Fatal("commit did not recheck current authority", result, err)
				}
				contracttest.NoPublication(t, m, r)
				return
			}
			if err != nil || result.Head.Revision != 1 || result.AlreadyCommitted {
				t.Fatal("first commit after renewed long scan", result, err)
			}
			head, err := peer.GetObject(ctx, r.Head.Object)
			if err != nil || head.Revision != 1 || head.OpID != r.Head.OpID {
				t.Fatal("head publication", head, err)
			}
			if _, err := peer.GetOutbox(ctx, r.Event.ID); err != nil {
				t.Fatal("outbox publication", err)
			}
			if retry, err := m.CommitObject(ctx, r); err != nil || !retry.AlreadyCommitted || retry.Head.Revision != 1 {
				t.Fatal("idempotent retry", retry, err)
			}
		})
	}
}
