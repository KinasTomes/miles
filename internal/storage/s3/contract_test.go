package s3

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/pack"
	"github.com/KinasTomes/miles/internal/storage"
	"github.com/KinasTomes/miles/internal/storage/contracttest"
	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
)

func warmHarness(t *testing.T, api API, bucket, kms string, ref storage.PackRef) contracttest.PackHarness {
	t.Helper()
	s, err := New(api, bucket, kms)
	if err != nil {
		t.Fatal(err)
	}
	return contracttest.PackHarness{Store: s, Ref: ref, SidecarInfo: func(r storage.PackRef) (storage.PackInfo, bool) {
		_, key, err := keys(r)
		if err != nil {
			return storage.PackInfo{}, false
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		h, err := api.HeadObject(ctx, &awss3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
		if err != nil {
			t.Error("sidecar HEAD", err)
			return storage.PackInfo{}, false
		}
		class := storage.StorageClass(h.StorageClass)
		if class == "" {
			class = storage.Standard
		}
		return storage.PackInfo{Ref: r, Size: aws.ToInt64(h.ContentLength), Class: class}, true
	}}
}

func TestWarmPackContract(t *testing.T) {
	contracttest.RunWarmPacks(t, func(t *testing.T, _ func() time.Time) contracttest.PackHarness {
		return warmHarness(t, newFakeAPI(), "dev-bucket", "kms-key", storage.PackRef{})
	})
}

// TestS3DevWarmContract is opt-in. It creates only random STANDARD test objects;
// it does not provision infrastructure, restore archives, or delete versions.
func TestS3DevWarmContract(t *testing.T) {
	if os.Getenv("MILES_S3_DEV_ACCEPTANCE") != "1" {
		t.Skip("S3 dev acceptance not enabled")
	}
	bucket, region := os.Getenv("MILES_S3_DEV_BUCKET"), os.Getenv("AWS_REGION")
	key, secret, token := os.Getenv("AWS_ACCESS_KEY_ID"), os.Getenv("AWS_SECRET_ACCESS_KEY"), os.Getenv("AWS_SESSION_TOKEN")
	if bucket == "" || region == "" || key == "" || secret == "" || token == "" {
		t.Fatal("explicit dev bucket, region and temporary AWS session credentials required")
	}
	client := awss3.New(awss3.Options{Region: region, HTTPClient: &http.Client{Timeout: 30 * time.Second},
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: key, SecretAccessKey: secret, SessionToken: token, Source: "dev acceptance environment"}, nil
		})})
	contracttest.RunWarmPacks(t, func(t *testing.T, _ func() time.Time) contracttest.PackHarness {
		ref, err := pack.NewRef()
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("retained acceptance pack_id=%s generation=%s; additional generation=other", ref.ID, ref.Generation)
		return warmHarness(t, client, bucket, os.Getenv("MILES_S3_DEV_KMS_KEY"), ref)
	})
}
