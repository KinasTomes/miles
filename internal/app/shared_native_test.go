package app

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/api/admin"
	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/metadata/dynamodb"
	"github.com/KinasTomes/miles/internal/multipart/core"
	"github.com/KinasTomes/miles/internal/object"
	"github.com/KinasTomes/miles/internal/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	s3 "github.com/aws/aws-sdk-go-v2/service/s3"
	stypes "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

type nativeUTC struct{}

func (nativeUTC) Bounds(ctx context.Context) (storage.R1Bounds, error) {
	now := time.Now().UTC()
	return storage.R1Bounds{Lower: now.Add(-time.Millisecond), Upper: now.Add(time.Millisecond), Horizon: now.Add(time.Hour)}, ctx.Err()
}

type nativeBody struct {
	*bytes.Reader
	data []byte
}

func (b nativeBody) MD5() []byte { s := md5.Sum(b.data); return s[:] }
func (b nativeBody) Checksum() (string, []byte) {
	sum := sha256.Sum256(b.data)
	return "SHA256", sum[:]
}
func bodyForNative(data []byte) nativeBody { return nativeBody{bytes.NewReader(data), data} }

func nativeEndpoint(t *testing.T, name string) string {
	t.Helper()
	value := os.Getenv(name)
	if value == "" {
		if os.Getenv(strings.TrimSuffix(name, "_ENDPOINT")+"_REQUIRED") == "1" || os.Getenv("MILES_DDB_LOCAL_REQUIRED") == "1" {
			t.Fatal(name + " required")
		}
		t.Skip(name + " not configured")
	}
	u, e := url.Parse(value)
	if e != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Port() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
		t.Fatal("test endpoints must be explicit loopback origins")
	}
	return value
}

func TestNativeSharedCloudEdgeRuntime(t *testing.T) {
	runNativeSharedRuntime(t, false)
}

func TestNativeWarmGCObjectFlows(t *testing.T) {
	runNativeSharedRuntime(t, true)
}

func runNativeSharedRuntime(t *testing.T, gcFlows bool) {
	dEndpoint, sEndpoint := nativeEndpoint(t, "MILES_DDB_LOCAL_ENDPOINT"), nativeEndpoint(t, "MILES_RECOVERY_S3_ENDPOINT")
	timeout := 90 * time.Second
	if gcFlows {
		timeout = 4 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	h := &http.Client{Transport: transport}
	t.Cleanup(transport.CloseIdleConnections)
	cred := aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		return aws.Credentials{AccessKeyID: "mileslocal", SecretAccessKey: "mileslocal"}, nil
	})
	clients := runtimeClients{metadata: ddb.New(ddb.Options{Region: "ap-southeast-1", BaseEndpoint: aws.String(dEndpoint), Credentials: cred, HTTPClient: h, RetryMaxAttempts: 1}), packs: s3.New(s3.Options{Region: "ap-southeast-1", BaseEndpoint: aws.String(sEndpoint), UsePathStyle: true, Credentials: cred, HTTPClient: h, RetryMaxAttempts: 1}), transport: transport}
	for {
		_, e := clients.metadata.ListTables(ctx, &ddb.ListTablesInput{})
		if e == nil {
			break
		}
		if ctx.Err() != nil {
			t.Fatal(e)
		}
		time.Sleep(100 * time.Millisecond)
	}
	id, e := storage.NewBucketID()
	if e != nil {
		t.Fatal(e)
	}
	c := config.DefaultStartup()
	c.RuntimeNodeID = "cloud-a"
	c.Profile = "cloud"
	c.LocalDataPath = t.TempDir()
	c.DDBTablePrefix = "runtime_" + id + "_"
	c.PackBucket = "runtime-" + id
	c.AdminAddress = "127.0.0.1:9000"
	for name, spec := range dynamodb.Tables() {
		name := c.DDBTablePrefix + name
		attributes := []dtypes.AttributeDefinition{{AttributeName: aws.String(spec.Partition), AttributeType: dtypes.ScalarAttributeTypeS}}
		keys := []dtypes.KeySchemaElement{{AttributeName: aws.String(spec.Partition), KeyType: dtypes.KeyTypeHash}}
		if spec.Sort != "" {
			attributes = append(attributes, dtypes.AttributeDefinition{AttributeName: aws.String(spec.Sort), AttributeType: dtypes.ScalarAttributeType(spec.SortType)})
			keys = append(keys, dtypes.KeySchemaElement{AttributeName: aws.String(spec.Sort), KeyType: dtypes.KeyTypeRange})
		}
		if _, e = clients.metadata.CreateTable(ctx, &ddb.CreateTableInput{TableName: aws.String(name), AttributeDefinitions: attributes, KeySchema: keys, BillingMode: dtypes.BillingModePayPerRequest}); e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() {
			_, e := clients.metadata.DeleteTable(context.Background(), &ddb.DeleteTableInput{TableName: aws.String(name)})
			if e != nil {
				t.Error(e)
			}
		})
	}
	if _, e = clients.packs.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(c.PackBucket), CreateBucketConfiguration: &stypes.CreateBucketConfiguration{LocationConstraint: stypes.BucketLocationConstraintApSoutheast1}}); e != nil {
		t.Fatal(e)
	}
	if gcFlows {
		_, e = clients.packs.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{Bucket: aws.String(c.PackBucket), VersioningConfiguration: &stypes.VersioningConfiguration{Status: stypes.BucketVersioningStatusEnabled}})
		if e != nil {
			t.Fatal(e)
		}
	}
	t.Cleanup(func() { cleanupNativePackBucket(t, clients.packs, c.PackBucket, gcFlows) })
	testClock := &gcFlowClock{Clock: clock.NewReal()}
	var utc storage.R1TimeSource = nativeUTC{}
	if gcFlows {
		utc = testClock
	}
	m, e := sharedStore(c, clients, utc)
	if e != nil {
		t.Fatal(e)
	}
	v, _ := config.NewPolicyValidator(config.DSGPolicyLimits())
	global, _ := v.NewGlobalSnapshot(1, config.DefaultGlobalPolicy())
	policy, _ := global.StorageSnapshot()
	policy.Revision = 0
	tokenID, tokenSecret, _, e := admin.NewToken()
	if e != nil {
		t.Fatal(e)
	}
	sum := sha256.Sum256([]byte(tokenSecret))
	bootstrap := storage.AdminBootstrap{Token: storage.AdminToken{ID: tokenID, Hash: sum[:], UserID: "owner", Scope: storage.TokenScopeSystem, Active: true, IssuedAt: time.Now().UTC(), ExpiresAt: time.Now().Add(time.Hour).UTC()}, QuotaLimit: 1 << 30, Policy: policy}
	if e = m.BootstrapAdmin(ctx, bootstrap); e != nil {
		t.Fatal(e)
	}
	if e = provisionSharedHistory(ctx, m, clients, c, bootstrap, id); e != nil {
		t.Fatal(e)
	}
	var nodes []Dependencies
	for _, profile := range []string{"cloud", "cloud", "edge"} {
		c.Profile = profile
		c.RuntimeNodeID = profile
		c.LocalDataPath = t.TempDir()
		if e = os.Chmod(c.LocalDataPath, 0700); e != nil {
			t.Fatal(e)
		}
		// Real gateways own independent transports. Sharing one here lets one
		// node's CloseIdleConnections interfere with another node's cleanup.
		nodeTransport := transport.Clone()
		nodeHTTP := &http.Client{Transport: nodeTransport}
		if gcFlows {
			nodeHTTP.Transport = gcFlowTransport{nodeTransport}
		}
		nodeClients := runtimeClients{metadata: ddb.New(ddb.Options{Region: "ap-southeast-1", BaseEndpoint: aws.String(dEndpoint), Credentials: cred, HTTPClient: nodeHTTP, RetryMaxAttempts: 1}), packs: s3.New(s3.Options{Region: "ap-southeast-1", BaseEndpoint: aws.String(sEndpoint), UsePathStyle: true, Credentials: cred, HTTPClient: nodeHTTP, RetryMaxAttempts: 1}), transport: nodeTransport}
		d, e := composeShared(ctx, c, nodeClients, testClock, utc, "cluster-v1", bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32))
		if e != nil {
			t.Fatal(e)
		}
		nodes = append(nodes, d)
		t.Cleanup(func() {
			if e := d.Close(); e != nil {
				t.Error(e)
			}
		})
	}
	if !gcFlows {
		nativeShutdownDuringPeerRecovery(t, ctx, nodes)
		nativeProvisionedCheckpoint(t, ctx, nodes[0])
	}
	mutation, e := nodes[0].Admin.Admit(ctx, tokenID)
	if e != nil {
		t.Fatal(e)
	}
	security, e := nodes[0].Admin.Sealer.Seal(ctx, "RUNTIMEKEY00000000001", []byte(strings.Repeat("s", 40)))
	if e != nil {
		t.Fatal(e)
	}
	_, e = m.MutateControl(ctx, storage.ControlMutation{AdminTokenID: tokenID, Context: mutation, Entity: "Credentials", ID: "RUNTIMEKEY00000000001", Credential: storage.Credential{ID: "RUNTIMEKEY00000000001", UserID: "owner", Active: true}, Security: security})
	nodes[0].Admin.Finish(ctx, mutation)
	if e != nil {
		t.Fatal(e)
	}
	stored, e := nodes[2].Admin.Store.GetCredentialSecurity(ctx, "RUNTIMEKEY00000000001")
	if e != nil {
		t.Fatal(e)
	}
	if clear, e := nodes[2].Admin.Sealer.Open(ctx, "RUNTIMEKEY00000000001", stored); e != nil || string(clear) != strings.Repeat("s", 40) {
		t.Fatal("shared credential cannot open on Edge", e)
	}
	bucket, _, e := nodes[0].Metadata.CreateBucket(ctx, storage.BucketCreate{CredentialID: "RUNTIMEKEY00000000001", UserID: "owner", Name: "shared-runtime"})
	if e != nil {
		t.Fatal(e)
	}

	cacheDoc, _ := json.Marshal(map[string]any{"cache": config.DefaultCachePolicy()})
	mutation, e = nodes[1].Admin.Admit(ctx, tokenID)
	if e != nil {
		t.Fatal(e)
	}
	_, e = m.PutBucketPolicy(storage.WithMutation(ctx, mutation), storage.BucketPolicyUpdate{Bucket: bucket.Name, BucketID: bucket.ID, AdminTokenID: tokenID, Writer: mutation.Writer, Candidate: storage.PolicySnapshot{Version: 1, Document: cacheDoc}})
	nodes[1].Admin.Finish(ctx, mutation)
	if e != nil {
		t.Fatal(e)
	}
	if gcFlows {
		nativeWarmGCFlows(t, ctx, nodes, m, clients.packs, c.PackBucket, bucket, testClock, global)
		return
	}
	key := "Unicode/tiếng Việt//plus+percent%2F"
	for i := 0; i < 3; i++ {
		data := bytes.Repeat([]byte{byte(61 + i)}, 128<<10)
		svc, e := nodes[i].ObjectsForRequest(ctx, bucket.Name)
		if e != nil {
			t.Fatal(e)
		}
		_, e = svc.PutObject(ctx, object.PutRequest{Bucket: bucket, Key: key, UserID: "owner", CredentialID: "RUNTIMEKEY00000000001", ContentLength: int64(len(data)), Body: bodyForNative(data)})
		if e != nil {
			t.Fatal("cross-profile PUT", i, e)
		}
		for j, d := range nodes {
			reader, e := d.ObjectsForRequest(ctx, bucket.Name)
			if e != nil {
				t.Fatal(e)
			}
			var got bytes.Buffer
			_, e = reader.GetObject(ctx, object.GetRequest{Bucket: bucket.Name, BucketID: bucket.ID, Key: key, CredentialID: "RUNTIMEKEY00000000001", Emit: func(_ context.Context, p []byte) error { _, e := got.Write(p); return e }})
			if e != nil || !bytes.Equal(got.Bytes(), data) {
				t.Fatal("cross-profile GET", i, j, e)
			}
		}
	}
	edge, e := nodes[2].ObjectsForRequest(ctx, bucket.Name)
	if e != nil {
		t.Fatal(e)
	}
	u, e := edge.CreateUpload(ctx, object.CreateUploadRequest{Bucket: bucket, Key: "multipart", CredentialID: "RUNTIMEKEY00000000001", ChecksumAlgorithm: "CRC64NVME", ChecksumType: "FULL_OBJECT"})
	if e != nil {
		t.Fatal("initiate", e)
	}
	if e = edge.AbortUpload(ctx, object.AbortRequest{Upload: u, CredentialID: "RUNTIMEKEY00000000001"}); e != nil {
		t.Fatal("abort", e)
	}
	u, e = edge.CreateUpload(ctx, object.CreateUploadRequest{Bucket: bucket, Key: "completed", CredentialID: "RUNTIMEKEY00000000001", ChecksumAlgorithm: "SHA256", ChecksumType: "COMPOSITE"})
	if e != nil {
		t.Fatal(e)
	}
	partData := bytes.Repeat([]byte("multipart across profiles"), 4096)
	cloud, e := nodes[0].ObjectsForRequest(ctx, bucket.Name)
	if e != nil {
		t.Fatal(e)
	}
	part, e := cloud.UploadPart(ctx, object.PartRequest{Upload: u, Number: 1, Body: bodyForNative(partData), ContentLength: int64(len(partData)), CredentialID: "RUNTIMEKEY00000000001"})
	if e != nil {
		t.Fatal("shared UploadPart", e)
	}
	u, e = edge.GetUpload(ctx, u.ID)
	if e != nil {
		t.Fatal(e)
	}
	in := core.Input{V: 1, BucketID: bucket.ID, Key: u.Object.Key, ChecksumAlgorithm: u.ChecksumAlgorithm, ChecksumType: u.ChecksumType, Parts: []core.Part{{PartNumber: 1, ETag: part.ETag, Checksum: &part.ChecksumValue}}}
	quota, e := m.GetQuota(ctx, bucket.OwnerID)
	if e != nil {
		t.Fatal(e)
	}
	invalid := in
	invalid.Parts = append([]core.Part(nil), in.Parts...)
	invalid.Parts[0].ETag = strings.Repeat("0", 32)
	if _, e = edge.CompleteUpload(ctx, object.CompleteRequest{Upload: u, Input: invalid, UserID: "owner", CredentialID: "RUNTIMEKEY00000000001"}); e == nil {
		t.Fatal("invalid ETag completed")
	} else {
		var validation *core.ValidationError
		if !errors.As(e, &validation) || validation.Code != "InvalidPart" {
			t.Fatal("invalid ETag returned wrong error", e)
		}
	}
	u, e = m.GetUpload(ctx, u.ID)
	if e != nil || u.State != "Active" || u.CompletionOp == "" {
		t.Fatal("validation did not reopen shared upload", u.State, e)
	}
	after, e := m.GetQuota(ctx, bucket.OwnerID)
	if e != nil || after != quota {
		t.Fatal("validation changed staging quota", after, quota, e)
	}
	if _, e = edge.CompleteUpload(ctx, object.CompleteRequest{Upload: u, Input: in, UserID: "owner", CredentialID: "RUNTIMEKEY00000000001"}); e != nil {
		t.Fatal("shared CompleteMultipartUpload", e)
	}
	var got bytes.Buffer
	_, e = cloud.GetObject(ctx, object.GetRequest{Bucket: bucket.Name, BucketID: bucket.ID, Key: u.Object.Key, CredentialID: "RUNTIMEKEY00000000001", Range: &storage.ByteRange{Offset: 7, Length: 51}, Emit: func(_ context.Context, p []byte) error { _, e := got.Write(p); return e }})
	if e != nil || !bytes.Equal(got.Bytes(), partData[7:58]) {
		t.Fatal("completed cross-profile Range", e)
	}
	g, e := m.GetR1Gate(ctx)
	if e != nil || g.Active != 0 || g.Resolving != 0 {
		t.Fatal("request admission leaked", g, e)
	}
	q, e := m.ListOutbox(ctx, storage.OutboxQuery{HistoryID: g.HistoryID, Epoch: g.Epoch, Limit: 128})
	if e != nil || len(q.Events) < 5 {
		t.Fatal("native producer events missing", q, e)
	}
	nativeUploadJobs(t, ctx, nodes, clients, c.DDBTablePrefix, bucket)
	nativeSweepDuringCut(t, ctx, nodes, clients, c.DDBTablePrefix, bucket)
	beforePurgeQuota, e := m.GetQuota(ctx, bucket.OwnerID)
	if e != nil {
		t.Fatal(e)
	}
	nativeClusterOperations(t, ctx, nodes, tokenID+"."+tokenSecret)
	afterPurgeQuota, e := m.GetQuota(ctx, bucket.OwnerID)
	if e != nil || afterPurgeQuota != beforePurgeQuota {
		t.Fatal("cache purge changed durable quota", e)
	}
	got.Reset()
	_, e = cloud.GetObject(ctx, object.GetRequest{Bucket: bucket.Name, BucketID: bucket.ID, Key: u.Object.Key, CredentialID: "RUNTIMEKEY00000000001", Range: &storage.ByteRange{Offset: 7, Length: 51}, Emit: func(_ context.Context, p []byte) error { _, e := got.Write(p); return e }})
	if e != nil || !bytes.Equal(got.Bytes(), partData[7:58]) {
		t.Fatal("cache purge changed durable object or prevented refill", e)
	}

}

func cleanupNativePackBucket(t *testing.T, client *s3.Client, bucket string, versioned bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	remove := func(key, version *string) {
		if _, e := client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &bucket, Key: key, VersionId: version}); e != nil {
			t.Error(e)
		}
	}
	if versioned {
		pages := s3.NewListObjectVersionsPaginator(client, &s3.ListObjectVersionsInput{Bucket: &bucket})
		var versions []stypes.ObjectVersion
		var markers []stypes.DeleteMarkerEntry
		for pages.HasMorePages() {
			page, e := pages.NextPage(ctx)
			if e != nil {
				t.Error(e)
				return
			}
			versions = append(versions, page.Versions...)
			markers = append(markers, page.DeleteMarkers...)
		}
		for _, version := range versions {
			remove(version.Key, version.VersionId)
		}
		for _, marker := range markers {
			remove(marker.Key, marker.VersionId)
		}
	} else {
		page, e := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: &bucket})
		if e != nil {
			t.Error(e)
			return
		}
		for _, object := range page.Contents {
			remove(object.Key, nil)
		}
	}
	if _, e := client.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: &bucket}); e != nil {
		t.Error(e)
	}
}
