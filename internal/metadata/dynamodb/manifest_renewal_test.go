package dynamodb

import (
	"context"
	"errors"
	"testing"
	"time"

	s "github.com/KinasTomes/miles/internal/storage"
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
