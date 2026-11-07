package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/app"
	"github.com/KinasTomes/miles/internal/cache"
	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/coordination"
	"github.com/KinasTomes/miles/internal/engine"
	"github.com/KinasTomes/miles/internal/faults"
	"github.com/KinasTomes/miles/internal/metadata/badger"
	"github.com/KinasTomes/miles/internal/metadata/contracttest"
	"github.com/KinasTomes/miles/internal/object"
	"github.com/KinasTomes/miles/internal/readpath"
	"github.com/KinasTomes/miles/internal/storage"
	packs3 "github.com/KinasTomes/miles/internal/storage/s3"
	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

func TestWiredS3FaultInjection(t *testing.T) {
	c := config.DefaultStartup()
	c.Mode = "dev"
	c.EnableFaultInjection = true
	d, err := app.Bootstrap(c)
	if err != nil {
		t.Fatal(err)
	}
	if d.Faults == nil {
		t.Fatal("expected non-nil d.Faults when enable_fault_injection is true")
	}

	a := new(app.Admission)
	nodeID := "dev-node"
	d.NodeID = nodeID
	server := httptest.NewServer(s3Handler(c.Region, nodeID, d, a))
	defer server.Close()

	cred := d.DevCredentials[0]
	signAndDo := func(method, path string) (int, string) {
		req, _ := http.NewRequest(method, server.URL+path, nil)
		hash := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
		req.Header.Set("X-Amz-Content-Sha256", hash)
		if err := v4.NewSigner().SignHTTP(context.Background(), aws.Credentials{AccessKeyID: cred.AccessKeyID, SecretAccessKey: cred.Secret}, req, hash, "s3", c.Region, time.Now()); err != nil {
			t.Fatal(err)
		}
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}

	// 1. Initial healthy call -> 200 OK
	code, body := signAndDo("GET", "/")
	if code != http.StatusOK {
		t.Fatalf("expected 200 OK initially, got %d: %s", code, body)
	}

	// 2. Inject s3_5xx fault
	rule, err := d.Faults.Add(time.Now(), faults.FaultRule{
		Type:     faults.FaultS35xx,
		Scope:    faults.ScopeNode,
		NodeID:   nodeID,
		Rate:     1.0,
		Duration: 60,
	})
	if err != nil {
		t.Fatalf("failed to add s3_5xx fault: %v", err)
	}

	// Request should fail with 500 InternalError
	code, body = signAndDo("GET", "/")
	if code != http.StatusInternalServerError {
		t.Fatalf("expected 500 InternalServerError with s3_5xx injected, got %d: %s", code, body)
	}
	if !strings.Contains(body, "InternalError") {
		t.Fatalf("expected S3 InternalError in body, got: %s", body)
	}

	// Delete s3_5xx rule -> restores normal service
	if err := d.Faults.Delete(rule.ID); err != nil {
		t.Fatalf("failed to delete fault rule: %v", err)
	}
	code, body = signAndDo("GET", "/")
	if code != http.StatusOK {
		t.Fatalf("expected 200 OK after deleting fault, got %d: %s", code, body)
	}

	// 3. Inject kill_node fault
	_, err = d.Faults.Add(time.Now(), faults.FaultRule{
		Type:     faults.FaultKillNode,
		Scope:    faults.ScopeNode,
		NodeID:   nodeID,
		Rate:     1.0,
		Duration: 60,
	})
	if err != nil {
		t.Fatalf("failed to add kill_node fault: %v", err)
	}

	// Request should trigger admission drain and return 503
	code, body = signAndDo("GET", "/")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 ServiceUnavailable with kill_node injected, got %d: %s", code, body)
	}
	if !a.Draining() {
		t.Fatal("expected admission gate to be draining after kill_node")
	}

	// Subsequent request also rejected because admission is draining
	code, body = signAndDo("GET", "/")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 ServiceUnavailable on drained node, got %d: %s", code, body)
	}
}

type dummyBody struct {
	*bytes.Reader
	md5 []byte
}

func (d *dummyBody) MD5() []byte                { return d.md5 }
func (d *dummyBody) Checksum() (string, []byte) { return "", nil }

type memS3Backend struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func (m *memS3Backend) PutObject(ctx context.Context, in *awss3.PutObjectInput, _ ...func(*awss3.Options)) (*awss3.PutObjectOutput, error) {
	data, err := io.ReadAll(in.Body)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := aws.ToString(in.Key)
	m.objects[key] = data
	sum := md5.Sum(data)
	return &awss3.PutObjectOutput{ETag: aws.String(`"` + hex.EncodeToString(sum[:]) + `"`)}, nil
}

func (m *memS3Backend) HeadObject(ctx context.Context, in *awss3.HeadObjectInput, _ ...func(*awss3.Options)) (*awss3.HeadObjectOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.objects[aws.ToString(in.Key)]
	if !ok {
		return nil, &types.NotFound{Message: aws.String("missing")}
	}
	return &awss3.HeadObjectOutput{ContentLength: aws.Int64(int64(len(data))), StorageClass: types.StorageClassStandard}, nil
}

func (m *memS3Backend) GetObject(ctx context.Context, in *awss3.GetObjectInput, _ ...func(*awss3.Options)) (*awss3.GetObjectOutput, error) {
	m.mu.Lock()
	data, ok := m.objects[aws.ToString(in.Key)]
	m.mu.Unlock()
	if !ok {
		return nil, &types.NoSuchKey{Message: aws.String("missing")}
	}
	if in.Range == nil {
		return &awss3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(data)), ContentLength: aws.Int64(int64(len(data)))}, nil
	}
	var lo, hi int64
	if _, err := fmt.Sscanf(*in.Range, "bytes=%d-%d", &lo, &hi); err != nil {
		return nil, err
	}
	sub := data[lo : hi+1]
	cr := fmt.Sprintf("bytes %d-%d/%d", lo, hi, len(data))
	return &awss3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(sub)), ContentLength: aws.Int64(int64(len(sub))), ContentRange: aws.String(cr)}, nil
}

func (m *memS3Backend) CopyObject(ctx context.Context, in *awss3.CopyObjectInput, _ ...func(*awss3.Options)) (*awss3.CopyObjectOutput, error) {
	return &awss3.CopyObjectOutput{}, nil
}

func (m *memS3Backend) RestoreObject(ctx context.Context, in *awss3.RestoreObjectInput, _ ...func(*awss3.Options)) (*awss3.RestoreObjectOutput, error) {
	return &awss3.RestoreObjectOutput{}, nil
}

func (m *memS3Backend) DeleteObject(ctx context.Context, in *awss3.DeleteObjectInput, _ ...func(*awss3.Options)) (*awss3.DeleteObjectOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, aws.ToString(in.Key))
	return &awss3.DeleteObjectOutput{}, nil
}

func TestCorruptPackS3Get(t *testing.T) {
	ctx := context.Background()
	f := contracttest.NewFixture()
	f.Quota.Limit = 1 << 20
	region := "ap-southeast-1"
	nodeID := "node-s3-get-test"

	db, err := badger.Open(badger.Options{Path: t.TempDir(), RequireProducers: true, Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	fences := make([]storage.WriterFence, 64)
	for i := range fences {
		fences[i] = storage.WriterFence{LeaseID: fmt.Sprintf("writer-lease-%d", i+1), ServiceIncarnation: f.Lease.Fence.ServiceIncarnation, Epoch: 1}
	}
	leases := make([]storage.WriterLease, len(fences))
	for i, fence := range fences {
		leases[i] = storage.WriterLease{Fence: fence, ExpiresAt: f.Now.Add(72 * time.Hour)}
	}
	if err := db.Bootstrap(ctx, badger.Bootstrap{
		Buckets: []storage.Bucket{f.Bucket}, Users: f.Users, Credentials: f.Credentials,
		Grants: []storage.Grant{
			{BucketID: f.Bucket.ID, UserID: f.Credentials[0].UserID, Read: true, Write: true, Revision: 1},
		},
		Quotas:  []storage.LogicalQuota{f.Quota},
		Writers: leases,
	}); err != nil {
		t.Fatal(err)
	}

	authority, err := db.StartLocalRuntime(context.Background(), f.Users[0].ID, 1<<30)
	if err != nil {
		t.Fatal(err)
	}

	memBackend := &memS3Backend{objects: map[string][]byte{}}
	store, err := packs3.New(memBackend, "physical-bucket", "")
	if err != nil {
		t.Fatal(err)
	}

	lru, err := cache.NewLRU(cache.Limits{CapacityBytes: 1 << 20, MaxEntries: 16, MaxCopyBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}

	reg := faults.NewRegistry(true)
	faultPacks := faults.WrapPackStore(coordination.ProtectPackStore(store), reg, nodeID)

	clk := clock.NewReal()
	limits := coordination.LeaseLimits{Lifetime: time.Minute, Heartbeat: 15 * time.Second}
	gate, err := coordination.NewR1Gate(clk, authority.Incarnation(), limits)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := coordination.NewWriterRegistry(gate, limits, coordination.Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Invalidate()

	budget := readpath.Budget{
		Capacity: readpath.Cost{ChunkSlots: 64, StoredBytes: 256 << 20, IOSlots: 64, IOBytes: 256 << 20, RawBytes: 256 << 20, WorkspaceBytes: 512 << 20, CopyBytes: 512 << 20},
		Prefetch: readpath.Cost{ChunkSlots: 32, StoredBytes: 128 << 20, IOSlots: 32, IOBytes: 128 << 20, RawBytes: 128 << 20, WorkspaceBytes: 256 << 20, CopyBytes: 256 << 20},
	}
	admission, err := readpath.NewAdmission(readpath.Limits{
		Node:   readpath.ScopeLimits{ActiveRequests: 100, Budget: budget},
		Bucket: readpath.ScopeLimits{ActiveRequests: 100, Budget: budget},
	})
	if err != nil {
		t.Fatal(err)
	}

	validator, err := config.NewPolicyValidator(config.DSGPolicyLimits())
	if err != nil {
		t.Fatal(err)
	}
	global, err := validator.NewGlobalSnapshot(1, config.DefaultGlobalPolicy())
	if err != nil {
		t.Fatal(err)
	}

	// WarmReader uses faultPacks at the read boundary
	reader, err := readpath.NewWarmReader(readpath.WarmOptions{
		Metadata: db, Packs: faultPacks, Cache: lru, Admission: admission, Gate: gate, Clock: clk,
		Global: global, Metrics: &readpath.WarmMetrics{}, TransportBytes: 4096,
		MaxManifestPages: 4, MaxManifestChunks: 20000,
	})
	if err != nil {
		t.Fatal(err)
	}

	cachePolicy := config.DefaultCachePolicy()
	cachePolicy.AdmitOnWrite = false // Ensures initial read must hit backend pack store
	p := global.Policy()
	ratio, _ := new(big.Rat).SetString(p.Compression.MinSavingRatio.String())

	objService, err := object.New(object.Options{
		Metadata: db, Packs: coordination.ProtectPackStore(store), Cache: lru, Registry: registry, Clock: clk,
		AdmitWriter: func(ctx context.Context, r object.WriterRequest) (object.AdmittedWriter, error) {
			c, err := authority.Admit(ctx, r.CredentialID, r.Object, r.OpID, r.WallDeadline)
			if err != nil {
				return object.AdmittedWriter{}, err
			}
			return object.AdmittedWriter{Fence: c.Writer, Producer: &c, ProtectedUntil: r.WallDeadline.Add(15 * time.Minute)}, nil
		},
		Chunking:    engine.ChunkingParams{Min: int(p.Chunking.MinBytes), Avg: int(p.Chunking.AvgBytes), Max: int(p.Chunking.MaxBytes)},
		Compression: engine.CompressionPolicy{Enabled: p.Compression.Enabled, Codec: p.Compression.Codec, Level: int(p.Compression.Level), MinSavingRatio: ratio},
		Events:      object.MetadataEvents{}, Reader: reader,
		BucketPolicy: func(id string) (config.BucketSnapshot, error) {
			return validator.NewBucketSnapshot(id, 1, cachePolicy)
		},
		Budget:         readpath.Budget{Capacity: readpath.Cost{ChunkSlots: 8, StoredBytes: 32 << 20, IOSlots: 8, IOBytes: 32 << 20, RawBytes: 32 << 20, WorkspaceBytes: 128 << 20, CopyBytes: 128 << 20}},
		MaxOutstanding: 4, RequestTimeout: 30 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}

	d := app.Dependencies{
		NodeID:         nodeID,
		Faults:         reg,
		Metadata:       db,
		Packs:          faultPacks,
		Cache:          lru,
		Objects:        objService,
		DevCredentials: []app.DevCredential{{AccessKeyID: f.Credentials[0].ID, UserID: f.Credentials[0].UserID, Secret: "test-secret"}},
	}

	server := httptest.NewServer(s3Handler(region, nodeID, d, new(app.Admission)))
	defer server.Close()

	cred := d.DevCredentials[0]
	signAndDo := func(method, path string, payload []byte) (int, []byte, error) {
		req, _ := http.NewRequest(method, server.URL+path, bytes.NewReader(payload))
		h := sha256.Sum256(payload)
		hash := hex.EncodeToString(h[:])
		req.Header.Set("X-Amz-Content-Sha256", hash)
		if err := v4.NewSigner().SignHTTP(context.Background(), aws.Credentials{AccessKeyID: cred.AccessKeyID, SecretAccessKey: cred.Secret}, req, hash, "s3", region, time.Now()); err != nil {
			t.Fatal(err)
		}
		resp, err := server.Client().Do(req)
		if err != nil {
			return 0, nil, err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		return resp.StatusCode, body, err
	}

	payload := []byte("hello corrupt_pack test payload!")
	objectPath := "/" + f.Bucket.Name + "/hello.txt"

	// 1. Write the object via S3 PUT
	code, body, err := signAndDo("PUT", objectPath, payload)
	if err != nil || code != http.StatusOK {
		t.Fatalf("expected 200 OK on PUT, got %d, err: %v, body: %s", code, err, string(body))
	}

	// 2. Pre-fault S3 GET: object read succeeds with verified content hash
	code, body, err = signAndDo("GET", objectPath, nil)
	if err != nil || code != http.StatusOK {
		t.Fatalf("expected 200 OK on pre-fault GET, got %d, err: %v, body: %s", code, err, string(body))
	}
	if !bytes.Equal(body, payload) {
		t.Fatalf("expected payload %q, got %q", string(payload), string(body))
	}

	// 3. Cache-bypass safeguard: Evict pack from cache so next GET must fetch through faultPacks
	packPage, err := db.ListPacksByState(ctx, storage.PackListRequest{State: "Warm", Limit: 100})
	if err != nil || len(packPage.Packs) == 0 {
		t.Fatalf("expected written packs, got %d packs, err: %v", len(packPage.Packs), err)
	}
	for _, p := range packPage.Packs {
		if err := lru.Evict(ctx, p.Ref); err != nil {
			t.Fatalf("failed to evict pack %s: %v", p.Ref.ID, err)
		}
	}

	// 4. Inject corrupt_pack fault rule
	faultRule, err := d.Faults.Add(time.Now(), faults.FaultRule{
		Type:     faults.FaultCorruptPack,
		Scope:    faults.ScopeNode,
		NodeID:   nodeID,
		Rate:     1.0,
		Duration: 10,
	})
	if err != nil {
		t.Fatalf("failed to add corrupt_pack fault rule: %v", err)
	}

	// With-fault S3 GET: corruption is detected at read boundary and request fails closed
	code, body, err = signAndDo("GET", objectPath, nil)
	if err == nil && code == http.StatusOK && bytes.Equal(body, payload) {
		t.Fatalf("expected failure/stream abort on with-fault GET, got 200 OK with full payload")
	}

	// Cache safeguard: verify that corrupt data was NOT cached
	// If it was cached, deleting the fault would still serve corrupt data
	if err := d.Faults.Delete(faultRule.ID); err != nil {
		t.Fatalf("failed to delete fault rule: %v", err)
	}

	// 5. After-fault S3 GET: reading from pack backend succeeds cleanly
	code, body, err = signAndDo("GET", objectPath, nil)
	if err != nil || code != http.StatusOK {
		t.Fatalf("expected 200 OK on after-fault GET, got %d, err: %v, body: %s", code, err, string(body))
	}
	if !bytes.Equal(body, payload) {
		t.Fatalf("expected payload %q, got %q", string(payload), string(body))
	}

	// 6. Subsequent GET (served from resident cache) also succeeds
	code, body, err = signAndDo("GET", objectPath, nil)
	if err != nil || code != http.StatusOK {
		t.Fatalf("expected 200 OK on cached GET, got %d, err: %v, body: %s", code, err, string(body))
	}
	if !bytes.Equal(body, payload) {
		t.Fatalf("expected cached payload %q, got %q", string(payload), string(body))
	}
}
