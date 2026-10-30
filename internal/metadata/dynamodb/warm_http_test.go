package dynamodb

import (
	"context"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	api "github.com/KinasTomes/miles/internal/api/s3"
	"github.com/KinasTomes/miles/internal/auth"
	"github.com/KinasTomes/miles/internal/benchmark"
	"github.com/KinasTomes/miles/internal/benchmarkdata"
	"github.com/KinasTomes/miles/internal/cache"
	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/coordination"
	"github.com/KinasTomes/miles/internal/engine"
	"github.com/KinasTomes/miles/internal/engine/write"
	"github.com/KinasTomes/miles/internal/object"
	"github.com/KinasTomes/miles/internal/readpath"
	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/KinasTomes/miles/internal/storage/contracttest"
	packs3 "github.com/KinasTomes/miles/internal/storage/s3"
	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Only the test fixture supplies lease authority and credentials. HTTP handlers,
// object service, producers, manifest/pack engine and Warm reader are real code.
type warmCredentials struct{ store *Store }

type warmObjectWriter struct {
	*object.Service
	t *testing.T
}

func (w warmObjectWriter) PutObject(ctx context.Context, r object.PutRequest) (object.PutResult, error) {
	out, err := w.Service.PutObject(ctx, r)
	if err != nil {
		w.t.Logf("Warm object service: %v", err)
	}
	return out, err
}

func (c warmCredentials) Credential(ctx context.Context, id string) (api.Credential, error) {
	r, err := c.store.GetCredential(ctx, id)
	return api.Credential{UserID: r.UserID, Secret: "mileslocal", Active: r.Active, SystemAdmin: r.SystemAdmin}, err
}

func warmAdmit(ctx context.Context, m *Store, history string, r object.WriterRequest) (object.AdmittedWriter, error) {
	fence := s.WriterFence{LeaseID: r.OpID, ServiceIncarnation: "history-1", Epoch: 1}
	err := m.update(ctx, func(tx *transaction) error {
		row, err := tx.observe(writerKey(fence.LeaseID))
		if err != nil {
			return err
		}
		row.after, err = nativeItem(s.WriterLease{Fence: fence, ExpiresAt: time.Now().Add(time.Minute)})
		if err != nil {
			return err
		}
		for k, v := range row.key.Attributes {
			row.after[k] = v
		}
		row.dirty = true
		return nil
	})
	c := s.MutationContext{HistoryID: history, LogicalOpID: r.OpID, MutationID: r.OpID, Object: r.Object, Writer: fence}
	return object.AdmittedWriter{Fence: fence, Producer: &c, ProtectedUntil: r.WallDeadline.Add(15 * time.Minute)}, err
}

func warmBudget(n int64) readpath.Budget {
	return readpath.Budget{
		Capacity: readpath.Cost{ChunkSlots: 64, StoredBytes: n, IOSlots: 64, IOBytes: n, RawBytes: n, WorkspaceBytes: 2 * n, CopyBytes: 2 * n},
		Prefetch: readpath.Cost{ChunkSlots: 32, StoredBytes: n / 2, IOSlots: 32, IOBytes: n / 2, RawBytes: n / 2, WorkspaceBytes: n, CopyBytes: n},
	}
}

func TestLocalWarmHTTPDynamoDBPrototype(t *testing.T) {
	endpoint := os.Getenv("MILES_WARM_S3_ENDPOINT")
	if endpoint == "" {
		if os.Getenv("MILES_WARM_S3_REQUIRED") == "1" {
			t.Fatal("S3 emulator endpoint required")
		}
		t.Skip("S3 emulator not explicitly configured")
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Port() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		t.Fatal("explicit loopback S3 endpoint required")
	}
	out := os.Getenv("MILES_WARM_HTTP_ARTIFACT_ROOT")
	if out == "" {
		out = filepath.Join(t.TempDir(), "runs")
	}
	if err = os.Mkdir(out, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err = benchmarkdata.Generate(filepath.Join(out, "dataset"), "smoke", "miles-w02-v1"); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(out, "dataset", "manifest.json")
	for _, concurrency := range []int{1, 4} {
		t.Run(fmt.Sprint(concurrency), func(t *testing.T) {
			f := contracttest.NewFixture()
			f.Now, f.Quota.Limit, f.Packs = time.Now().UTC(), 1<<30, nil
			m, _ := fixtureStore(t, f)
			m.now, m.authority, m.requireProducers = time.Now, fixtureAuthority{time.Now}, true
			ctx := context.Background()
			bucketOp, err := write.NewManifestID()
			if err != nil {
				t.Fatal(err)
			}
			admitted, err := warmAdmit(ctx, m, strings.Repeat("1", 32), object.WriterRequest{OpID: bucketOp, WallDeadline: time.Now().Add(30 * time.Minute)})
			if err != nil {
				t.Fatal(err)
			}
			// Seed through the real producer path, including its namespace ledger.
			bucket, created, err := m.CreateBucket(s.WithMutation(ctx, *admitted.Producer), s.BucketCreate{CredentialID: "owner-key", UserID: "owner", Name: "warm-benchmark"})
			if err != nil || !created {
				t.Fatal("strict bucket creation", bucket, err)
			}
			transport := http.DefaultTransport.(*http.Transport).Clone()
			transport.Proxy, transport.DisableCompression = nil, true
			t.Cleanup(transport.CloseIdleConnections)
			client := awss3.New(awss3.Options{Region: "ap-southeast-1", BaseEndpoint: aws.String(endpoint), UsePathStyle: true, RetryMaxAttempts: 1,
				Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
					return aws.Credentials{AccessKeyID: "mileslocal", SecretAccessKey: "mileslocal"}, nil
				}),
				HTTPClient: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
			})
			id, err := write.NewManifestID()
			if err != nil {
				t.Fatal(err)
			}
			physical, direct := "warm-packs-"+id, "warm-direct-"+id
			for _, bucket := range []string{physical, direct} {
				_, err = client.CreateBucket(ctx, &awss3.CreateBucketInput{Bucket: aws.String(bucket), CreateBucketConfiguration: &types.CreateBucketConfiguration{LocationConstraint: types.BucketLocationConstraintApSoutheast1}})
				if err != nil {
					t.Fatal(err)
				}
			}
			packs, err := packs3.New(client, physical, "")
			if err != nil {
				t.Fatal(err)
			}
			resident, err := cache.NewLRU(cache.Limits{CapacityBytes: 64 << 20, MaxEntries: 1024, MaxCopyBytes: 4 << 20})
			if err != nil {
				t.Fatal(err)
			}
			clk := clock.NewReal()
			limits := coordination.LeaseLimits{Lifetime: time.Minute, Heartbeat: 15 * time.Second}
			gate, err := coordination.NewR1Gate(clk, "history-1", limits)
			if err != nil {
				t.Fatal(err)
			}
			registry, err := coordination.NewWriterRegistry(gate, limits, coordination.Hooks{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(registry.Invalidate)
			budget := warmBudget(256 << 20)
			admission, err := readpath.NewAdmission(readpath.Limits{Node: readpath.ScopeLimits{ActiveRequests: 64, Budget: budget}, Bucket: readpath.ScopeLimits{ActiveRequests: 64, Budget: budget}})
			if err != nil {
				t.Fatal(err)
			}
			validator, err := config.NewPolicyValidator(config.DSGPolicyLimits())
			if err != nil {
				t.Fatal(err)
			}
			p := config.DefaultGlobalPolicy()
			global, err := validator.NewGlobalSnapshot(1, p)
			if err != nil {
				t.Fatal(err)
			}
			reader, err := readpath.NewWarmReader(readpath.WarmOptions{Metadata: m, Packs: packs, Cache: resident, Admission: admission, Gate: gate, Clock: clk, Global: global, Metrics: &readpath.WarmMetrics{}, TransportBytes: 128 << 10, MaxManifestPages: 1024, MaxManifestChunks: 65536})
			if err != nil {
				t.Fatal(err)
			}
			ratio, _ := new(big.Rat).SetString(p.Compression.MinSavingRatio.String())
			cachePolicy := config.DefaultCachePolicy()
			cachePolicy.AdmitOnWrite = false
			objects, err := object.New(object.Options{Metadata: m, Packs: coordination.ProtectPackStore(packs), Cache: resident, Registry: registry, Clock: clk,
				AdmitWriter: func(ctx context.Context, r object.WriterRequest) (object.AdmittedWriter, error) {
					return warmAdmit(ctx, m, strings.Repeat("1", 32), r)
				},
				Chunking:    engine.ChunkingParams{Min: int(p.Chunking.MinBytes), Avg: int(p.Chunking.AvgBytes), Max: int(p.Chunking.MaxBytes)},
				Compression: engine.CompressionPolicy{Enabled: p.Compression.Enabled, Codec: p.Compression.Codec, Level: int(p.Compression.Level), MinSavingRatio: ratio},
				Events:      object.MetadataEvents{}, Reader: reader,
				BucketPolicy: func(id string) (config.BucketSnapshot, error) { return validator.NewBucketSnapshot(id, 1, cachePolicy) },
				Budget:       warmBudget(64 << 20), MaxOutstanding: 4, RequestTimeout: 30 * time.Minute,
			})
			if err != nil {
				t.Fatal(err)
			}
			handlers := api.NewBucketHandlers(m)
			for op, h := range api.NewObjectHandlers(warmObjectWriter{objects, t}, objects, objects) {
				handlers[op] = h
			}
			server := httptest.NewServer(&api.Router{Region: "ap-southeast-1", Now: func() time.Time { return clk.Now().WallUTC }, Credentials: warmCredentials{m}, Access: auth.NewSnapshots(m, clk), Handlers: handlers})
			t.Cleanup(server.Close)
			trial := filepath.Join(out, fmt.Sprintf("concurrency-%d", concurrency))
			if err = os.Mkdir(trial, 0700); err != nil {
				t.Fatal(err)
			}
			credential := filepath.Join(trial, "credentials.json")
			if err = os.WriteFile(credential, []byte(`{"access_key_id":"owner-key","secret_access_key":"mileslocal"}`), 0600); err != nil {
				t.Fatal(err)
			}
			for _, target := range []struct{ name, kind, origin, bucket, backend string }{{"direct", "local-s3-smoke", endpoint, direct, "none"}, {"miles", "miles-warm", server.URL, bucket.Name, "dynamodb-local"}} {
				c := benchmark.Config{Target: target.kind, Endpoint: target.origin, Region: "ap-southeast-1", Bucket: target.bucket, CredentialsFile: credential,
					Environment: benchmark.Environment{Topology: "single-process W4 gateway test fixture; native DynamoDB Local and Moto", Host: "local test host", CPU: "not a performance acceptance run", Disk: "native DynamoDB Local in-memory", Network: "loopback HTTP", Metadata: target.backend, GatewayCommit: os.Getenv("MILES_WARM_SOURCE_REVISION"), PolicyRevision: "1", Encryption: "none; emulator only", CacheState: "fresh 64MiB memory LRU, admit-on-write disabled; read passes separate", Isolation: "new metadata tables, direct and physical buckets per concurrency", AsyncWork: "strict native producers/outbox; no exporter, GC or multi-node workers"},
				}
				if c.Environment.GatewayCommit == "" {
					c.Environment.GatewayCommit = "test-checkout; revision not supplied"
				}
				if target.name == "direct" {
					c.Environment.GatewayCommit, c.Environment.PolicyRevision = "", ""
					c.Environment.CacheState = "emulator internals uncontrolled"
				}
				r, err := benchmark.Run(ctx, c, benchmark.Options{Manifest: manifest, Out: filepath.Join(trial, target.name), Concurrency: concurrency, ReadRepeats: 3})
				if err != nil {
					t.Fatalf("%s: %v", target.name, err)
				}
				verified := 0
				for _, sample := range r.Samples {
					if sample.Operation == "GetObject" && sample.Error == "" && len(sample.SHA256) == 64 {
						verified++
					}
				}
				if r.Status != "passed" || verified != 36 || r.MVPThresholdApplied {
					t.Fatal("Warm HTTP run incomplete", target.name, verified)
				}
			}
			quota, err := m.GetQuota(ctx, f.Quota.OwnerID)
			if err != nil || quota.Used != 0 || quota.Reserved != 0 || quota.Staged != 0 {
				t.Fatal("logical cleanup did not settle quota", quota, err)
			}
		})
	}
}
