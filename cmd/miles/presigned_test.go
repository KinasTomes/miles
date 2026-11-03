package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/app"
	"github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// Real composition root, Badger, pack SDK and read/write/multipart services.
// Only the physical pack backend is emulated. Neither profile claims AWS proof.
func TestPresignedLocalProfiles(t *testing.T) {
	endpoint := os.Getenv("MILES_RECOVERY_S3_ENDPOINT")
	if endpoint == "" {
		if os.Getenv("MILES_PRESIGNED_LOCAL_REQUIRED") == "1" {
			t.Fatal("MILES_RECOVERY_S3_ENDPOINT required")
		}
		t.Skip("set MILES_RECOVERY_S3_ENDPOINT to a disposable loopback Moto fixture")
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Port() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		t.Fatal("pack fixture must be an explicit loopback HTTP origin")
	}
	for _, profile := range []string{"cloud", "edge"} {
		t.Run(profile, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			transport := http.DefaultTransport.(*http.Transport).Clone()
			transport.Proxy = nil
			defer transport.CloseIdleConnections()
			httpClient := &http.Client{Transport: transport, Timeout: 80 * time.Second,
				CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			provider := func(id, secret string) aws.CredentialsProvider {
				return aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
					return aws.Credentials{AccessKeyID: id, SecretAccessKey: secret}, nil
				})
			}
			backend := sdk.New(sdk.Options{Region: "ap-southeast-1", BaseEndpoint: aws.String(endpoint), UsePathStyle: true,
				Credentials: provider("mileslocal", "mileslocal"), HTTPClient: httpClient, RetryMaxAttempts: 1})
			id, err := storage.NewBucketID()
			if err != nil {
				t.Fatal(err)
			}
			physical := "presign-" + id
			_, err = backend.CreateBucket(ctx, &sdk.CreateBucketInput{Bucket: &physical,
				CreateBucketConfiguration: &types.CreateBucketConfiguration{LocationConstraint: types.BucketLocationConstraintApSoutheast1}})
			if err != nil {
				t.Fatal("create disposable pack bucket", err)
			}
			t.Cleanup(func() {
				cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				pages := sdk.NewListObjectsV2Paginator(backend, &sdk.ListObjectsV2Input{Bucket: &physical})
				for pages.HasMorePages() {
					page, e := pages.NextPage(cleanup)
					if e != nil {
						t.Error(e)
						return
					}
					for _, o := range page.Contents {
						if _, e := backend.DeleteObject(cleanup, &sdk.DeleteObjectInput{Bucket: &physical, Key: o.Key}); e != nil {
							t.Error(e)
						}
					}
				}
				if _, e := backend.DeleteBucket(cleanup, &sdk.DeleteBucketInput{Bucket: &physical}); e != nil {
					t.Error(e)
				}
			})
			c := config.DefaultStartup()
			c.Profile = profile
			c.Mode = "local"
			c.LocalDataPath = t.TempDir()
			c.PackEndpoint = endpoint
			c.PackBucket = physical
			d, err := app.Bootstrap(c)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			server := httptest.NewServer(s3Handler(c.Region, d, new(app.Admission)))
			defer server.Close()
			credential := d.DevCredentials[0]
			client := sdk.New(sdk.Options{Region: c.Region, BaseEndpoint: aws.String(server.URL), UsePathStyle: true,
				Credentials: provider(credential.AccessKeyID, credential.Secret), HTTPClient: httpClient, RetryMaxAttempts: 1})
			bucket := "presigned-logical"
			if _, err := client.CreateBucket(ctx, &sdk.CreateBucketInput{Bucket: &bucket, CreateBucketConfiguration: &types.CreateBucketConfiguration{LocationConstraint: types.BucketLocationConstraint(c.Region)}}); err != nil {
				t.Fatal(err)
			}
			presigner := sdk.NewPresignClient(client)
			payload := []byte("presigned payload\x00with exact bytes\n")
			key := "café +%/a//b/../encoded/"
			put, err := presigner.PresignPutObject(ctx, &sdk.PutObjectInput{Bucket: &bucket, Key: &key, ContentLength: aws.Int64(int64(len(payload))),
				Metadata: map[string]string{"with_underscore": "kept"}}, sdk.WithPresignExpires(7*24*time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			send := func(method, uri string, headers http.Header, body []byte, status int) []byte {
				t.Helper()
				r, err := http.NewRequestWithContext(ctx, method, uri, bytes.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				r.Header = headers.Clone()
				response, err := httpClient.Do(r)
				if err != nil {
					t.Fatal("fixture request failed")
				}
				defer response.Body.Close()
				data, err := io.ReadAll(response.Body)
				if err != nil {
					t.Fatal(err)
				}
				if response.StatusCode != status {
					t.Fatalf("%s: HTTP %d, want %d", method, response.StatusCode, status)
				}
				return data
			}
			send(put.Method, put.URL, put.SignedHeader, payload, 200)
			get, err := presigner.PresignGetObject(ctx, &sdk.GetObjectInput{Bucket: &bucket, Key: &key})
			if err != nil {
				t.Fatal(err)
			}
			if got := send(get.Method, get.URL, get.SignedHeader, nil, 200); !bytes.Equal(got, payload) {
				t.Fatal("presigned GET bytes differ")
			}
			head, err := client.HeadObject(ctx, &sdk.HeadObjectInput{Bucket: &bucket, Key: &key})
			if err != nil || aws.ToInt64(head.ContentLength) != int64(len(payload)) || head.Metadata["with_underscore"] != "kept" {
				t.Fatal("HEAD metadata/length", err)
			}
			rangeResult, err := client.GetObject(ctx, &sdk.GetObjectInput{Bucket: &bucket, Key: &key, Range: aws.String("bytes=2-8")})
			if err != nil {
				t.Fatal(err)
			}
			span, err := io.ReadAll(rangeResult.Body)
			rangeResult.Body.Close()
			if err != nil || !bytes.Equal(span, payload[2:9]) {
				t.Fatal("Range bytes differ", err)
			}
			copyKey := "copy"
			if _, err := client.CopyObject(ctx, &sdk.CopyObjectInput{Bucket: &bucket, Key: &copyKey, CopySource: aws.String(bucket + "/" + url.PathEscape(key))}); err != nil {
				t.Fatal(err)
			}
			multipartKey := "multipart"
			upload, err := client.CreateMultipartUpload(ctx, &sdk.CreateMultipartUploadInput{Bucket: &bucket, Key: &multipartKey})
			if err != nil {
				t.Fatal(err)
			}
			part, err := client.UploadPart(ctx, &sdk.UploadPartInput{Bucket: &bucket, Key: &multipartKey, UploadId: upload.UploadId, PartNumber: aws.Int32(1), Body: bytes.NewReader(payload)})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.CompleteMultipartUpload(ctx, &sdk.CompleteMultipartUploadInput{Bucket: &bucket, Key: &multipartKey, UploadId: upload.UploadId,
				MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{{ETag: part.ETag, PartNumber: aws.Int32(1)}}}}); err != nil {
				t.Fatal(err)
			}
			for _, k := range []string{copyKey, multipartKey} {
				o, err := client.GetObject(ctx, &sdk.GetObjectInput{Bucket: &bucket, Key: &k})
				if err != nil {
					t.Fatal(err)
				}
				got, err := io.ReadAll(o.Body)
				o.Body.Close()
				if err != nil || !bytes.Equal(got, payload) {
					t.Fatal("copy/multipart bytes differ", err)
				}
			}
			// A signed query checksum must be enforced before publication.
			sum := sha256.Sum256(payload)
			checked, err := presigner.PresignPutObject(ctx, &sdk.PutObjectInput{Bucket: &bucket, Key: aws.String("checked"),
				ChecksumSHA256: aws.String(base64.StdEncoding.EncodeToString(sum[:])), ContentLength: aws.Int64(int64(len(payload)))})
			if err != nil {
				t.Fatal(err)
			}
			bad := bytes.Repeat([]byte("x"), len(payload))
			if body := send(checked.Method, checked.URL, checked.SignedHeader, bad, 400); !strings.Contains(string(body), "BadDigest") {
				t.Fatal("checksum error missing")
			}
			_, err = client.HeadObject(ctx, &sdk.HeadObjectInput{Bucket: &bucket, Key: aws.String("checked")})
			var apiError smithy.APIError
			if !errors.As(err, &apiError) || apiError.ErrorCode() != "NotFound" {
				t.Fatal("bad checksum published an object", err)
			}
			for _, k := range []string{key, copyKey, multipartKey} {
				if _, err := client.DeleteObject(ctx, &sdk.DeleteObjectInput{Bucket: &bucket, Key: &k}); err != nil {
					t.Fatal(err)
				}
			}
			page, err := client.ListObjectsV2(ctx, &sdk.ListObjectsV2Input{Bucket: &bucket})
			if err != nil || len(page.Contents) != 0 {
				t.Fatal("DELETE/LIST left live objects", err)
			}
		})
	}
}
