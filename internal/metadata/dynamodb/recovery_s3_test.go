package dynamodb

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/recovery"
	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

func TestLocalRecoveryS3JournalEndToEnd(t *testing.T) {
	endpoint := os.Getenv("MILES_RECOVERY_S3_ENDPOINT")
	if endpoint == "" {
		if os.Getenv("MILES_RECOVERY_S3_REQUIRED") == "1" {
			t.Fatal("recovery S3 emulator required")
		}
		t.Skip("recovery S3 emulator not configured")
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Port() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		t.Fatal("literal loopback S3 endpoint required")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DisableCompression = true
	t.Cleanup(transport.CloseIdleConnections)
	client := awss3.New(awss3.Options{Region: "ap-southeast-1", BaseEndpoint: aws.String(endpoint), UsePathStyle: true, RetryMaxAttempts: 1,
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "mileslocal", SecretAccessKey: "mileslocal"}, nil
		}),
		HTTPClient: &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	})
	id, err := s.NewBucketID()
	if err != nil {
		t.Fatal(err)
	}
	bucket := "miles-recovery-" + id
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err = client.CreateBucket(ctx, &awss3.CreateBucketInput{Bucket: aws.String(bucket), CreateBucketConfiguration: &types.CreateBucketConfiguration{LocationConstraint: types.BucketLocationConstraintApSoutheast1}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		pager := awss3.NewListObjectsV2Paginator(client, &awss3.ListObjectsV2Input{Bucket: aws.String(bucket)})
		for pager.HasMorePages() {
			page, err := pager.NextPage(ctx)
			if err != nil {
				t.Error(err)
				return
			}
			for _, object := range page.Contents {
				if _, err = client.DeleteObject(ctx, &awss3.DeleteObjectInput{Bucket: aws.String(bucket), Key: object.Key}); err != nil {
					t.Error(err)
				}
			}
		}
		if _, err := client.DeleteBucket(ctx, &awss3.DeleteBucketInput{Bucket: aws.String(bucket)}); err != nil {
			t.Error(err)
		}
	})
	f := newRecoveryFixture(t)
	base, err := f.objects.Read(ctx, "recovery/histories/"+f.catalog.History+"/genesis-"+f.catalog.Genesis.SHA256+".json")
	if err != nil {
		t.Fatal(err)
	}
	objects, err := recovery.NewS3Objects(client, bucket, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err = recovery.ProvisionHistory(ctx, f.m, objects, f.catalog, base); err != nil {
		t.Fatal(err)
	}
	f.objects = objects
	r, w := f.mutation(t, "s3-wire")
	out, err := f.m.MutateControl(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	f.release(t, w)
	if err = f.service(f.peer, f.worker).Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = f.peer.GetOutboxExport(ctx, out.Event.ID); err != nil {
		t.Fatal("readback did not mark export", err)
	}
	if _, err = recovery.VerifyCut(ctx, objects, f.catalog, 0); err == nil {
		t.Fatal("Open epoch became a recovery cut")
	}
	marker, err := f.service(f.peer, f.worker).CloseAndSeal(ctx, f.request(0))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = recovery.VerifyCut(ctx, objects, f.catalog, 0); err != nil {
		t.Fatal(err)
	}
	seal, err := objects.Read(ctx, marker.Key)
	if err != nil {
		t.Fatal(err)
	}
	if err = objects.Create(ctx, marker.Key, seal); err != nil {
		t.Fatal("conditional retry changed bytes", err)
	}
	if err = objects.Create(ctx, marker.Key, []byte(`{"bad":true}`)); !errors.Is(err, s.ErrConflict) {
		t.Fatal("overwrite accepted", err)
	}
	page, err := client.ListObjectsV2(ctx, &awss3.ListObjectsV2Input{Bucket: aws.String(bucket)})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Contents) < 8 {
		t.Fatal("missing journal/catalog/membership/seal artifacts")
	}
	for _, object := range page.Contents {
		if object.StorageClass != types.ObjectStorageClassStandard {
			t.Fatal("non-Standard journal", aws.ToString(object.Key), object.StorageClass)
		}
	}
	if !strings.Contains(marker.Key, "seal-") {
		t.Fatal(marker)
	}
	t.Log("native DynamoDB + signed S3 SDK/Moto: producer, export, exact close, immutable seal, marker, CAS and DB-independent verification")
}
