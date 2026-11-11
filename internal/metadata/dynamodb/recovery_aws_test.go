package dynamodb

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/jobs"
	"github.com/KinasTomes/miles/internal/recovery"
	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/KinasTomes/miles/internal/storage/contracttest"
	"github.com/KinasTomes/miles/internal/storage/fake"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

func liveRecoveryClients(t *testing.T) (*ddb.Client, *s3.Client, string) {
	t.Helper()
	if os.Getenv("MILES_RECOVERY_DEV_ACCEPTANCE") != "1" {
		t.Skip("AWS recovery acceptance not enabled")
	}
	region, account, role := os.Getenv("AWS_REGION"), os.Getenv("MILES_AWS_DEV_ACCOUNT"), os.Getenv("MILES_AWS_DEV_ROLE")
	if region != "ap-southeast-1" || !regexp.MustCompile(`^[0-9]{12}$`).MatchString(account) || !regexp.MustCompile(`^[A-Za-z0-9+=,.@_-]{1,64}$`).MatchString(role) {
		t.Fatal("expected Singapore, account and assumed role")
	}
	for _, key := range []string{"AWS_ENDPOINT_URL", "AWS_ENDPOINT_URL_DYNAMODB", "AWS_ENDPOINT_URL_S3", "AWS_ENDPOINT_URL_STS"} {
		if os.Getenv(key) != "" {
			t.Fatal("custom AWS endpoint not allowed for live acceptance")
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	t.Setenv("AWS_IGNORE_CONFIGURED_ENDPOINT_URLS", "true")
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	t.Cleanup(transport.CloseIdleConnections)
	httpClient := &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region), awsconfig.WithHTTPClient(httpClient))
	if err != nil {
		t.Fatal("load AWS configuration", err)
	}
	creds, err := cfg.Credentials.Retrieve(ctx)
	if err != nil || !creds.CanExpire || creds.SessionToken == "" {
		t.Fatal("temporary AWS credentials required", err)
	}
	identity, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil || aws.ToString(identity.Account) != account || !strings.HasPrefix(aws.ToString(identity.Arn), "arn:aws:sts::"+account+":assumed-role/"+role+"/") {
		t.Fatal("AWS account or assumed role mismatch", err)
	}
	return ddb.NewFromConfig(cfg, func(o *ddb.Options) { o.RetryMaxAttempts = 1 }), s3.NewFromConfig(cfg, func(o *s3.Options) { o.RetryMaxAttempts = 1 }), account
}

// This isolated native-backend fixture uses a modeled clock and fake pack data.
// It proves DynamoDB/S3 fencing and journal semantics, not production timing or GC.
func TestAWSDevR1AdmissionClosureRenewalAndTakeover(t *testing.T) {
	metadata, _, _ := liveRecoveryClients(t)
	seed := contracttest.NewFixture()
	seed.Now = time.Now().UTC()
	seed.Lease.ExpiresAt = seed.Now.Add(time.Minute)
	for i := range seed.Packs {
		seed.Packs[i].StateRevision = 1
	}
	store, client := fixtureStoreWithClient(t, seed, metadata, "miles_r1_acceptance_", true)
	m, peer, fixture, clock := sharedFixtureWith(t, seed, store, client)
	runR1AdmissionClosureRenewalAndTakeover(t, m, peer, fixture, clock)
	t.Log("native DynamoDB: single admission, renewal, fencing, grace, reconciliation, takeover and gate closure; modeled clock")
}

func TestAWSDevRecoveryFencedSealCheckpoint(t *testing.T) {
	metadata, physical, account := liveRecoveryClients(t)
	bucket := os.Getenv("MILES_S3_DEV_BUCKET")
	if bucket == "" {
		t.Fatal("explicit isolated S3 test bucket required")
	}
	versioning, err := physical.GetBucketVersioning(t.Context(), &s3.GetBucketVersioningInput{Bucket: &bucket, ExpectedBucketOwner: &account})
	if err != nil || versioning.Status != s3types.BucketVersioningStatusEnabled {
		t.Fatal("test bucket must be owned and versioned", err)
	}
	id, err := s.NewBucketID()
	if err != nil {
		t.Fatal(err)
	}
	objects, err := recovery.NewS3Objects(trackedDevPacks{Client: physical, t: t, account: account}, bucket, "journal/acceptance/"+id+"/", os.Getenv("MILES_S3_DEV_KMS_KEY"))
	if err != nil {
		t.Fatal(err)
	}
	seed := contracttest.NewFixture()
	seed.Now = time.Now().UTC()
	seed.Lease.ExpiresAt = seed.Now.Add(time.Minute)
	store, client := fixtureStoreWithClient(t, seed, metadata, "miles_recovery_acceptance_", true)
	f := newRecoveryFixtureWith(t, true, seed, store, client, objects)
	runRecoveryFencedSealCheckpoint(t, f)
	t.Log("native DynamoDB and S3: fenced closed producer, export, seal, independent cut verification and idempotent checkpoint; modeled clock and fake packs")
}

func TestLocalRecoveryFencedSealCheckpoint(t *testing.T) {
	runRecoveryFencedSealCheckpoint(t, newRecoveryFixtureBase(t, true))
}

func runRecoveryFencedSealCheckpoint(t *testing.T, f *recoveryFixture) {
	t.Helper()
	request, producer := f.mutation(t, "native-checkpoint")
	request.ID = "owner"
	request.ExpectedRevision = 1
	request.User = s.User{ID: "owner", Revision: 1, Active: true, DefaultDomain: "recovered"}
	request.SetQuota = true
	request.QuotaLimit = 32
	var err error
	if _, err = f.m.MutateControl(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	f.release(t, producer)
	if _, err = f.peer.CheckR1(t.Context(), producer); !errors.Is(err, s.ErrFenced) {
		t.Fatal("closed producer retained authority", err)
	}
	worker := f.service(f.m, f.worker)
	if err = worker.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err = worker.CloseAndSeal(t.Context(), f.request(0)); err != nil {
		t.Fatal(err)
	}
	if _, err = recovery.VerifyCut(t.Context(), f.objects, f.catalog, 0); err != nil {
		t.Fatal(err)
	}
	checkpoint := recovery.CheckpointWorker{Store: f.m, Objects: f.objects, Packs: fake.NewPacks(f.utc.Now), Catalog: f.catalog}
	job := jobs.Job{ID: "checkpoint-native", Worker: f.worker, Params: json.RawMessage(`{"target_epoch":0}`)}
	result, err := checkpoint.Step(t.Context(), job, config.GlobalSnapshot{})
	if err != nil || !result.Done || !result.Settled {
		t.Fatal(result, err)
	}
	again, err := checkpoint.Step(t.Context(), job, config.GlobalSnapshot{})
	if err != nil || !reflect.DeepEqual(result.Result, again.Result) {
		t.Fatal("checkpoint retry differs", again, err)
	}
}
