package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/adminclient"
	"github.com/KinasTomes/miles/internal/api/admin"
	"github.com/KinasTomes/miles/internal/app"
	"github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	sdk "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// packS3Emulator implements the physical S3 PackStore/VersionInventory HTTP
// endpoints locally so both Cloud and Edge profiles run the full client and
// Admin guide without external containers or AWS calls.
type packS3Emulator struct {
	mu      sync.Mutex
	buckets map[string]bool
	objects map[string][]byte
	etags   map[string]string
}

func newPackS3Emulator(bucket string) *packS3Emulator {
	return &packS3Emulator{
		buckets: map[string]bool{bucket: true},
		objects: map[string][]byte{},
		etags:   map[string]string{},
	}
}

func (p *packS3Emulator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/")
	bucket, key, hasKey := strings.Cut(rest, "/")
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.buckets[bucket] {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if !hasKey || key == "" {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method == http.MethodGet && r.URL.Query().Has("versions") {
			type versionItem struct {
				Key          string `xml:"Key"`
				VersionId    string `xml:"VersionId"`
				IsLatest     bool   `xml:"IsLatest"`
				Size         int64  `xml:"Size"`
				StorageClass string `xml:"StorageClass"`
			}
			type listVersions struct {
				XMLName xml.Name      `xml:"ListVersionsResult"`
				Name    string        `xml:"Name"`
				Version []versionItem `xml:"Version"`
			}
			out := listVersions{Name: bucket}
			prefix := bucket + "/"
			for k, data := range p.objects {
				if strings.HasPrefix(k, prefix) {
					out.Version = append(out.Version, versionItem{
						Key: strings.TrimPrefix(k, prefix), VersionId: "v1", IsLatest: true, Size: int64(len(data)), StorageClass: "STANDARD",
					})
				}
			}
			b, _ := xml.Marshal(out)
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(append([]byte(xml.Header), b...))
			return
		}
		w.WriteHeader(http.StatusNotImplemented)
		return
	}
	full := bucket + "/" + key
	switch r.Method {
	case http.MethodPut:
		if r.Header.Get("If-None-Match") == "*" {
			if _, exists := p.objects[full]; exists {
				w.WriteHeader(http.StatusPreconditionFailed)
				return
			}
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		sum := md5.Sum(body)
		etag := `"` + hex.EncodeToString(sum[:]) + `"`
		p.objects[full] = body
		p.etags[full] = etag
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusOK)
	case http.MethodHead:
		data, ok := p.objects[full]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.Header().Set("ETag", p.etags[full])
		w.Header().Set("x-amz-storage-class", "STANDARD")
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		data, ok := p.objects[full]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		rng := r.Header.Get("Range")
		if rng == "" {
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			w.Header().Set("ETag", p.etags[full])
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(data)
			return
		}
		var lo, hi int64
		if _, err := fmt.Sscanf(rng, "bytes=%d-%d", &lo, &hi); err != nil || lo < 0 || hi < lo || hi >= int64(len(data)) {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		slice := data[lo : hi+1]
		w.Header().Set("Content-Length", strconv.Itoa(len(slice)))
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", lo, hi, len(data)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(slice)
	case http.MethodDelete:
		delete(p.objects, full)
		delete(p.etags, full)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func TestGuideEndToEndBothProfilesAndCredentialSeparation(t *testing.T) {
	for _, profile := range []string{"cloud", "edge"} {
		t.Run(profile, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			const packBucket = "miles-packs"
			emulator := newPackS3Emulator(packBucket)
			backendSrv := httptest.NewServer(emulator)
			defer backendSrv.Close()

			c := config.DefaultStartup()
			c.Profile = profile
			c.Mode = "local"
			c.LocalDataPath = filepath.Join(t.TempDir(), "local")
			c.PackEndpoint = backendSrv.URL
			c.PackBucket = packBucket
			c.AdminAddress = "127.0.0.1:0"

			d, err := app.Bootstrap(c)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()

			// Seed initial system admin user + token + global policy (bootstrap contract).
			validator, err := config.NewPolicyValidator(config.DSGPolicyLimits())
			if err != nil {
				t.Fatal(err)
			}
			global, err := validator.NewGlobalSnapshot(1, config.DefaultGlobalPolicy())
			if err != nil {
				t.Fatal(err)
			}
			polSnap, err := global.StorageSnapshot()
			if err != nil {
				t.Fatal(err)
			}
			polSnap.Revision = 0
			tokID, tokSecret, sysToken, err := admin.NewToken()
			if err != nil {
				t.Fatal(err)
			}
			hash := sha256.Sum256([]byte(tokSecret))
			now := time.Now().UTC()
			if err := d.Admin.Store.BootstrapAdmin(ctx, storage.AdminBootstrap{
				Token: storage.AdminToken{
					ID: tokID, UserID: "ops", Scope: storage.TokenScopeSystem,
					Hash: hash[:], Active: true, IssuedAt: now, ExpiresAt: now.Add(time.Hour),
				},
				QuotaLimit: 1 << 40,
				Policy:     polSnap,
			}); err != nil {
				t.Fatal(err)
			}

			adm := new(app.Admission)
			s3Srv := httptest.NewServer(s3Handler(c.Region, c.RuntimeNodeID, d, adm))
			defer s3Srv.Close()
			var auditLogs bytes.Buffer
			ah, err := adminHandler(*d.Admin, &auditLogs)
			if err != nil {
				t.Fatal(err)
			}
			adminSrv := httptest.NewServer(ah(adm))
			defer adminSrv.Close()

			tokFile := filepath.Join(t.TempDir(), "ops-token")
			if err := os.WriteFile(tokFile, []byte(sysToken+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			adminCli, err := adminclient.New(adminSrv.URL, tokFile, 10*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer adminCli.Close()

			newS3Client := func(id, secret string) *sdk.Client {
				return sdk.New(sdk.Options{
					Region:       c.Region,
					BaseEndpoint: aws.String(s3Srv.URL),
					UsePathStyle: true,
					Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
						return aws.Credentials{AccessKeyID: id, SecretAccessKey: secret}, nil
					}),
					RetryMaxAttempts: 1,
				})
			}

			devCred := d.DevCredentials[0]
			devClient := newS3Client(devCred.AccessKeyID, devCred.Secret)
			// 1. Credential isolation: backend IAM/pack key MUST fail on DSG S3 & Admin;
			// DSG SigV4 key MUST fail on Admin; DSG Admin bearer MUST fail on S3.
			backendAsS3 := newS3Client("mileslocal", "mileslocal")
			if _, err := backendAsS3.ListBuckets(ctx, &sdk.ListBucketsInput{}); !isS3Code(err, "InvalidAccessKeyId") {
				t.Fatalf("backend IAM key accepted on DSG S3: %v", err)
			}
			sigv4TokFile := filepath.Join(t.TempDir(), "sigv4-as-bearer")
			if err := os.WriteFile(sigv4TokFile, []byte(devCred.AccessKeyID+"."+devCred.Secret), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := adminclient.New(adminSrv.URL, sigv4TokFile, 5*time.Second); err == nil {
				t.Fatal("DSG SigV4 key parsed as Admin bearer")
			}
			bearerS3Req, _ := http.NewRequestWithContext(ctx, http.MethodGet, s3Srv.URL+"/", nil)
			bearerS3Req.Header.Set("Authorization", "Bearer "+sysToken)
			bearerS3Resp, err := http.DefaultClient.Do(bearerS3Req)
			if err != nil {
				t.Fatal(err)
			}
			bearerS3Resp.Body.Close()
			if bearerS3Resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("Admin bearer accepted on S3: %d", bearerS3Resp.StatusCode)
			}

			// 2. Bucket & P0/P1 client workflow as dev owner.
			const bucket = "guide-demo"
			if _, err := devClient.CreateBucket(ctx, &sdk.CreateBucketInput{
				Bucket:                    aws.String(bucket),
				CreateBucketConfiguration: &types.CreateBucketConfiguration{LocationConstraint: types.BucketLocationConstraint(c.Region)},
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := devClient.HeadBucket(ctx, &sdk.HeadBucketInput{Bucket: aws.String(bucket)}); err != nil {
				t.Fatal(err)
			}
			loc, err := devClient.GetBucketLocation(ctx, &sdk.GetBucketLocationInput{Bucket: aws.String(bucket)})
			if err != nil || string(loc.LocationConstraint) != c.Region {
				t.Fatalf("location: %+v %v", loc, err)
			}

			// Grants & bucket policy via Admin API (owner token for dev).
			ownerTokRaw, err := adminCli.Request(ctx, http.MethodPost, "/admin/v1/admin-tokens", []byte(`{"user_id":"dev","system_admin":false,"expires_in_seconds":3600}`))
			if err != nil {
				t.Fatal(err)
			}
			var ownerTok struct {
				Token string `json:"token"`
			}
			if json.Unmarshal(ownerTokRaw, &ownerTok) != nil || ownerTok.Token == "" {
				t.Fatal(string(ownerTokRaw))
			}
			ownerTokFile := filepath.Join(t.TempDir(), "dev-token")
			if err := os.WriteFile(ownerTokFile, []byte(ownerTok.Token), 0600); err != nil {
				t.Fatal(err)
			}
			ownerCli, err := adminclient.New(adminSrv.URL, ownerTokFile, 10*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer ownerCli.Close()

			bktRaw, err := ownerCli.Get(ctx, "/admin/v1/buckets/"+bucket)
			if err != nil {
				t.Fatal(err)
			}
			var bktMeta struct {
				BucketID string `json:"bucket_id"`
			}
			if json.Unmarshal(bktRaw, &bktMeta) != nil || bktMeta.BucketID == "" {
				t.Fatal(string(bktRaw))
			}

			// Create debug credential for ops (system_admin + debug_allowed) and grant read on guide-demo.
			if _, err := adminCli.Request(ctx, http.MethodPost, "/admin/v1/credentials", []byte(`{"user_id":"ops","system_admin":false,"debug_allowed":true}`)); err == nil {
				t.Fatal("debug_allowed without system_admin must fail")
			}
			opsCredRaw, err := adminCli.Request(ctx, http.MethodPost, "/admin/v1/credentials", []byte(`{"user_id":"ops","system_admin":true,"debug_allowed":true}`))
			if err != nil {
				t.Fatal(err)
			}
			var opsCred struct {
				AccessKeyID     string `json:"access_key_id"`
				SecretAccessKey string `json:"secret_access_key"`
			}
			if json.Unmarshal(opsCredRaw, &opsCred) != nil || opsCred.SecretAccessKey == "" {
				t.Fatal(string(opsCredRaw))
			}

			// Before grant: ops with signed x-dsg-debug: 1 is denied 403 and sees no x-dsg-*.
			payload := bytes.Repeat([]byte("miles-guide-payload-"), 1024)
			putOut, err := devClient.PutObject(ctx, &sdk.PutObjectInput{
				Bucket: aws.String(bucket), Key: aws.String("obj.bin"), Body: bytes.NewReader(payload), ContentLength: aws.Int64(int64(len(payload))),
			})
			if err != nil {
				t.Fatal(err)
			}

			signedDebugGET := func(id, secret, key string) (*http.Response, []byte) {
				req, _ := http.NewRequestWithContext(ctx, http.MethodGet, s3Srv.URL+"/"+bucket+"/"+key, nil)
				empty := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
				req.Header.Set("X-Amz-Content-Sha256", empty)
				req.Header.Set("X-Dsg-Debug", "1")
				if err := v4.NewSigner().SignHTTP(ctx, aws.Credentials{AccessKeyID: id, SecretAccessKey: secret}, req, empty, "s3", c.Region, time.Now().UTC()); err != nil {
					t.Fatal(err)
				}
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				b, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				return resp, b
			}

			if resp, _ := signedDebugGET(opsCred.AccessKeyID, opsCred.SecretAccessKey, "obj.bin"); resp.StatusCode != http.StatusForbidden {
				t.Fatalf("ops without grant got %d, want 403", resp.StatusCode)
			}

			// Normal owner (dev) with signed x-dsg-debug: 1 gets 200 and NO x-dsg-* headers.
			if resp, body := signedDebugGET(devCred.AccessKeyID, devCred.Secret, "obj.bin"); resp.StatusCode != http.StatusOK || !bytes.Equal(body, payload) {
				t.Fatalf("dev GET failed: %d", resp.StatusCode)
			} else {
				for h := range resp.Header {
					if strings.HasPrefix(strings.ToLower(h), "x-dsg-") {
						t.Fatalf("normal user leaked debug header %s", h)
					}
				}
			}

			// Owner grants read to ops via Admin API -> ops reads with x-dsg-cache.
			grantBody, _ := json.Marshal(map[string]any{
				"read": true, "expected_revision": 0, "expected_bucket_id": bktMeta.BucketID,
			})
			if _, err := ownerCli.Request(ctx, http.MethodPut, "/admin/v1/buckets/"+bucket+"/grants/ops", grantBody); err != nil {
				t.Fatal(err)
			}
			// Re-verify ops (Wait: snapshot isn't cached yet for ops? Wait: earlier ops got 403 and auth.Snapshots cached the negative grant for 5s, OR wait — let's check! Yes, earlier call cached auth snapshot for ops on guide-demo for up to 5s! Using a fresh key or checking after 5s? Even simpler: let's test ops on a second bucket OR wait not needed if we grant BEFORE ops's first GET on a 2nd bucket, or test negative on bucket 1 and positive on bucket 2 so the test runs in <1s without sleeping 5s!).
			const bucket2 = "guide-debug"
			if _, err := devClient.CreateBucket(ctx, &sdk.CreateBucketInput{
				Bucket:                    aws.String(bucket2),
				CreateBucketConfiguration: &types.CreateBucketConfiguration{LocationConstraint: types.BucketLocationConstraint(c.Region)},
			}); err != nil {
				t.Fatal(err)
			}
			bkt2Raw, err := ownerCli.Get(ctx, "/admin/v1/buckets/"+bucket2)
			if err != nil {
				t.Fatal(err)
			}
			var bkt2Meta struct {
				BucketID string `json:"bucket_id"`
			}
			_ = json.Unmarshal(bkt2Raw, &bkt2Meta)
			grant2Body, _ := json.Marshal(map[string]any{"read": true, "expected_revision": 0, "expected_bucket_id": bkt2Meta.BucketID})
			if _, err := ownerCli.Request(ctx, http.MethodPut, "/admin/v1/buckets/"+bucket2+"/grants/ops", grant2Body); err != nil {
				t.Fatal(err)
			}
			if _, err := devClient.PutObject(ctx, &sdk.PutObjectInput{
				Bucket: aws.String(bucket2), Key: aws.String("dbg.bin"), Body: bytes.NewReader(payload), ContentLength: aws.Int64(int64(len(payload))),
			}); err != nil {
				t.Fatal(err)
			}
			reqDbg, _ := http.NewRequestWithContext(ctx, http.MethodGet, s3Srv.URL+"/"+bucket2+"/dbg.bin", nil)
			emptyHash := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
			reqDbg.Header.Set("X-Amz-Content-Sha256", emptyHash)
			reqDbg.Header.Set("X-Dsg-Debug", "1")
			if err := v4.NewSigner().SignHTTP(ctx, aws.Credentials{AccessKeyID: opsCred.AccessKeyID, SecretAccessKey: opsCred.SecretAccessKey}, reqDbg, emptyHash, "s3", c.Region, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			respDbg, err := http.DefaultClient.Do(reqDbg)
			if err != nil {
				t.Fatal(err)
			}
			respDbg.Body.Close()
			if respDbg.StatusCode != http.StatusOK || respDbg.Header.Get("X-Dsg-Cache") == "" {
				t.Fatalf("ops+debug+grant GET: %d X-Dsg-Cache=%q", respDbg.StatusCode, respDbg.Header.Get("X-Dsg-Cache"))
			}

			// 3. GetObject If-Match (200 vs 412 PreconditionFailed), CopyObject (COPY, REPLACE, self-copy), Multipart, Presigned, RestoreObject, DeleteObjects.
			if _, err := devClient.GetObject(ctx, &sdk.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String("obj.bin"), IfMatch: aws.String(`"wrong"`)}); !isS3Code(err, "PreconditionFailed") {
				t.Fatalf("If-Match wrong ETag: %v", err)
			}
			if _, err := devClient.GetObject(ctx, &sdk.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String("obj.bin"), IfMatch: putOut.ETag}); err != nil {
				t.Fatalf("If-Match matching ETag: %v", err)
			}
			if _, err := devClient.CopyObject(ctx, &sdk.CopyObjectInput{
				Bucket: aws.String(bucket), Key: aws.String("obj.bin"), CopySource: aws.String(bucket + "/obj.bin"), MetadataDirective: types.MetadataDirectiveCopy,
			}); !isS3Code(err, "InvalidRequest") {
				t.Fatalf("self-copy COPY: %v", err)
			}
			if _, err := devClient.CopyObject(ctx, &sdk.CopyObjectInput{
				Bucket: aws.String(bucket), Key: aws.String("obj.bin"), CopySource: aws.String(bucket + "/obj.bin"), MetadataDirective: types.MetadataDirectiveReplace, ContentType: aws.String("application/octet-stream"),
			}); err != nil {
				t.Fatalf("self-copy REPLACE: %v", err)
			}

			// Multipart upload + ListParts + Complete + idempotent retry.
			up, err := devClient.CreateMultipartUpload(ctx, &sdk.CreateMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String("mp.bin")})
			if err != nil {
				t.Fatal(err)
			}
			part, err := devClient.UploadPart(ctx, &sdk.UploadPartInput{
				Bucket: aws.String(bucket), Key: aws.String("mp.bin"), UploadId: up.UploadId, PartNumber: aws.Int32(1), Body: bytes.NewReader(payload), ContentLength: aws.Int64(int64(len(payload))),
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := devClient.ListParts(ctx, &sdk.ListPartsInput{Bucket: aws.String(bucket), Key: aws.String("mp.bin"), UploadId: up.UploadId}); err != nil {
				t.Fatal(err)
			}
			compIn := &sdk.CompleteMultipartUploadInput{
				Bucket: aws.String(bucket), Key: aws.String("mp.bin"), UploadId: up.UploadId,
				MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{{ETag: part.ETag, PartNumber: aws.Int32(1)}}},
			}
			c1, err := devClient.CompleteMultipartUpload(ctx, compIn)
			if err != nil {
				t.Fatal(err)
			}
			c2, err := devClient.CompleteMultipartUpload(ctx, compIn)
			if err != nil || aws.ToString(c1.ETag) != aws.ToString(c2.ETag) {
				t.Fatalf("idempotent Complete: c1=%+v c2=%+v err=%v", c1, c2, err)
			}

			// Presigned GET + query ?x-dsg-debug=1 rejection.
			presigner := sdk.NewPresignClient(devClient)
			pg, err := presigner.PresignGetObject(ctx, &sdk.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String("mp.bin")})
			if err != nil {
				t.Fatal(err)
			}
			u, _ := url.Parse(pg.URL)
			q := u.Query()
			q.Set("x-dsg-debug", "1")
			u.RawQuery = q.Encode()
			respQ, err := http.Get(u.String())
			if err != nil {
				t.Fatal(err)
			}
			respQ.Body.Close()
			if respQ.StatusCode != http.StatusNotImplemented {
				t.Fatalf("query debug status %d, want 501", respQ.StatusCode)
			}

			// RestoreObject on Warm object -> 400 InvalidRequest.
			if _, err := devClient.RestoreObject(ctx, &sdk.RestoreObjectInput{
				Bucket: aws.String(bucket), Key: aws.String("obj.bin"),
				RestoreRequest: &types.RestoreRequest{Days: aws.Int32(1)},
			}); !isS3Code(err, "InvalidRequest") {
				t.Fatalf("Warm RestoreObject: %v", err)
			}

			// Admin stats refresh & query.
			if err := d.Statistics.Refresh(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := adminCli.Get(ctx, "/admin/v1/stats"); err != nil {
				t.Fatal(err)
			}
			if _, err := adminCli.Get(ctx, "/admin/v1/objects/layout?bucket="+bucket+"&key=obj.bin"); err != nil {
				t.Fatal(err)
			}
			// Unsupported route returns ErrUnsupported.
			if _, err := adminCli.Get(ctx, "/admin/v1/cost/report"); !errors.Is(err, adminclient.ErrUnsupported) {
				t.Fatalf("cost/report error: %v", err)
			}

			// DeleteObjects & DeleteBucket.
			if _, err := devClient.DeleteObjects(ctx, &sdk.DeleteObjectsInput{
				Bucket: aws.String(bucket),
				Delete: &types.Delete{Objects: []types.ObjectIdentifier{{Key: aws.String("obj.bin")}, {Key: aws.String("mp.bin")}}},
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := devClient.DeleteBucket(ctx, &sdk.DeleteBucketInput{Bucket: aws.String(bucket)}); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(auditLogs.String(), sysToken) || strings.Contains(auditLogs.String(), opsCred.SecretAccessKey) {
				t.Fatal("admin audit log leaked secrets")
			}
		})
	}
}

func isS3Code(err error, want string) bool {
	var api smithy.APIError
	return errors.As(err, &api) && api.ErrorCode() == want
}
