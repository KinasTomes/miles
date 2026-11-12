package s3

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/auth"
	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/coordination"
	"github.com/KinasTomes/miles/internal/engine"
	"github.com/KinasTomes/miles/internal/metadata/badger"
	"github.com/KinasTomes/miles/internal/object"
	"github.com/KinasTomes/miles/internal/readpath"
	"github.com/KinasTomes/miles/internal/storage"
	"github.com/KinasTomes/miles/internal/storage/contracttest"
	"github.com/KinasTomes/miles/internal/storage/fake"
	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

type debugEvents struct{}

func (debugEvents) ObjectCommit(_ context.Context, e object.CommitEvent) (storage.OutboxEvent, error) {
	history := strings.Repeat("a", 32)
	id, err := storage.EventID(history, e.OpID, "Objects", "object_commit")
	if err != nil {
		return storage.OutboxEvent{}, err
	}
	return storage.OutboxEvent{
		Version: storage.ModelVersion, ID: id, HistoryID: history, Epoch: 0,
		Entity: "Objects", Action: "object_commit", Object: e.Object, Revision: e.Revision,
		OpID: e.OpID, PayloadPointer: "journal/input/" + e.OpID, PayloadSHA256: strings.Repeat("b", 64),
	}, nil
}

type spyCache struct {
	*fake.MemoryCache
	hasCalls atomic.Int64
}

func (s *spyCache) Has(ctx context.Context, k storage.CacheKey) bool {
	s.hasCalls.Add(1)
	return s.MemoryCache.Has(ctx, k)
}

type debugTestEnv struct {
	router      *Router
	db          *badger.Store
	packs       *fake.Packs
	cache       *spyCache
	clock       *clock.Virtual
	creds       map[string]Credential
	restoreMu   sync.Mutex
	restoreSeen map[storage.PackRef]bool
	restoreFail bool
	autoRestore bool
}

func newDebugTestEnv(t *testing.T) *debugTestEnv {
	t.Helper()
	ctx := context.Background()
	f := contracttest.NewFixture()
	c, err := clock.NewVirtual(f.Now, 100)
	if err != nil {
		t.Fatal(err)
	}
	db, err := badger.Open(badger.Options{Path: t.TempDir(), Now: func() time.Time { return c.Now().WallUTC }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	fences := make([]storage.WriterFence, 64)
	leases := make([]storage.WriterLease, 64)
	for i := range fences {
		fences[i] = storage.WriterFence{
			LeaseID:            "debug-lease-" + strconv.Itoa(i+1),
			ServiceIncarnation: f.Lease.Fence.ServiceIncarnation,
			Epoch:              1,
		}
		leases[i] = storage.WriterLease{Fence: fences[i], ExpiresAt: f.Now.Add(72 * time.Hour)}
	}

	users := []storage.User{
		{ID: "owner", Active: true, Revision: 1, DefaultDomain: "default"},
		{ID: "rwuser", Active: true, Revision: 1, DefaultDomain: "default"},
		{ID: "admin-debug", Active: true, Revision: 1, DefaultDomain: "admin-domain"},
		{ID: "admin-nodebug", Active: true, Revision: 1, DefaultDomain: "default"},
		{ID: "admin-grantee", Active: true, Revision: 1, DefaultDomain: "default"},
		{ID: "admin-writeonly", Active: true, Revision: 1, DefaultDomain: "default"},
		{ID: "admin-readonly", Active: true, Revision: 1, DefaultDomain: "default"},
		{ID: "nonadmin-debug", Active: true, Revision: 1, DefaultDomain: "default"},
	}
	creds := []storage.Credential{
		{ID: "OWNERKEY000000000001", UserID: "owner", Active: true, Revision: 1},
		{ID: "GRANTEEKEY0000000002", UserID: "rwuser", Active: true, Revision: 1},
		{ID: "ADMINDEBUG0000000003", UserID: "admin-debug", Active: true, SystemAdmin: true, DebugAllowed: true, Revision: 1},
		{ID: "ADMINNODEBUG00000004", UserID: "admin-nodebug", Active: true, SystemAdmin: true, DebugAllowed: false, Revision: 1},
		{ID: "ADMINGRANT0000000005", UserID: "admin-grantee", Active: true, SystemAdmin: true, DebugAllowed: true, Revision: 1},
		{ID: "ADMINWRITE0000000006", UserID: "admin-writeonly", Active: true, SystemAdmin: true, DebugAllowed: true, Revision: 1},
		{ID: "ADMINREAD00000000007", UserID: "admin-readonly", Active: true, SystemAdmin: true, DebugAllowed: true, Revision: 1},
		{ID: "FAKEADMINDEBUG000008", UserID: "nonadmin-debug", Active: true, SystemAdmin: false, DebugAllowed: true, Revision: 1},
	}
	buckets := []storage.Bucket{
		{Name: "demo", ID: "bkt-demo-debug", OwnerID: "owner", Domain: "default", Revision: 1, Active: true, CreatedAt: f.Now},
		{Name: "admin-own", ID: "bkt-admin-own", OwnerID: "admin-debug", Domain: "admin-domain", Revision: 1, Active: true, CreatedAt: f.Now},
	}
	grants := []storage.Grant{
		{BucketID: "bkt-demo-debug", UserID: "rwuser", Read: true, Write: true, Revision: 1},
		{BucketID: "bkt-demo-debug", UserID: "admin-nodebug", Read: true, Write: true, Revision: 1},
		{BucketID: "bkt-demo-debug", UserID: "admin-grantee", Read: true, Write: true, Revision: 1},
		{BucketID: "bkt-demo-debug", UserID: "admin-writeonly", Read: false, Write: true, Revision: 1},
		{BucketID: "bkt-demo-debug", UserID: "admin-readonly", Read: true, Write: false, Revision: 1},
		{BucketID: "bkt-demo-debug", UserID: "nonadmin-debug", Read: true, Write: true, Revision: 1},
	}
	var quotas []storage.LogicalQuota
	for _, u := range users {
		quotas = append(quotas, storage.LogicalQuota{OwnerID: u.ID, Limit: 1 << 30})
	}
	if err := db.Bootstrap(ctx, badger.Bootstrap{
		Buckets: buckets, Users: users, Credentials: creds, Grants: grants, Quotas: quotas, Writers: leases,
	}); err != nil {
		t.Fatal(err)
	}

	packs := fake.NewPacks(func() time.Time { return c.Now().WallUTC })
	memCache := &spyCache{MemoryCache: fake.NewCache()}
	limits := coordination.LeaseLimits{Lifetime: time.Minute, Heartbeat: 15 * time.Second}
	gate, err := coordination.NewR1Gate(c, f.Lease.Fence.ServiceIncarnation, limits)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := coordination.NewWriterRegistry(gate, limits, coordination.Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	bud := readpath.Cost{ChunkSlots: 64 << 20, StoredBytes: 64 << 20, IOSlots: 64 << 20, IOBytes: 64 << 20, RawBytes: 64 << 20, WorkspaceBytes: 64 << 20, CopyBytes: 64 << 20}
	pref := readpath.Cost{ChunkSlots: 32 << 20, StoredBytes: 32 << 20, IOSlots: 32 << 20, IOBytes: 32 << 20, RawBytes: 32 << 20, WorkspaceBytes: 32 << 20, CopyBytes: 32 << 20}
	admission, err := readpath.NewAdmission(readpath.Limits{
		Node:   readpath.ScopeLimits{ActiveRequests: 64, Budget: readpath.Budget{Capacity: bud, Prefetch: pref}},
		Bucket: readpath.ScopeLimits{ActiveRequests: 64, Budget: readpath.Budget{Capacity: bud, Prefetch: pref}},
	})
	if err != nil {
		t.Fatal(err)
	}
	validator, err := config.NewPolicyValidator(config.DSGPolicyLimits())
	if err != nil {
		t.Fatal(err)
	}
	gp := config.DefaultGlobalPolicy()
	gp.Tiering.AutoRestore = true
	global, err := validator.NewGlobalSnapshot(1, gp)
	if err != nil {
		t.Fatal(err)
	}
	env := &debugTestEnv{
		db: db, packs: packs, cache: memCache, clock: c,
		creds: map[string]Credential{}, restoreSeen: map[storage.PackRef]bool{}, autoRestore: true,
	}
	utc := readUTCSource(func(ctx context.Context) (storage.R1Bounds, error) {
		now := c.Now().WallUTC
		return storage.R1Bounds{Lower: now, Upper: now, Horizon: now.Add(time.Hour)}, ctx.Err()
	})
	reader, err := readpath.NewWarmReader(readpath.WarmOptions{
		Metadata: db, Packs: packs, Cache: memCache, Admission: admission, Gate: gate, Clock: c, UTC: utc,
		Global: global, Metrics: &readpath.WarmMetrics{}, TransportBytes: 4096, MaxManifestPages: 16, MaxManifestChunks: 20000,
		Restore: func(ctx context.Context, _ *coordination.WarmView, _ storage.ObjectHead, ref storage.PackRef, _ uint64, _ time.Time, _ *storage.RestoreRequest) (bool, error) {
			env.restoreMu.Lock()
			defer env.restoreMu.Unlock()
			if !env.autoRestore {
				return false, storage.ErrInvalid
			}
			if env.restoreFail {
				return false, storage.ErrUnavailable
			}
			if env.restoreSeen[ref] {
				return false, nil
			}
			env.restoreSeen[ref] = true
			return true, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var admMu sync.Mutex
	nextFence := 0
	svc, err := object.New(object.Options{
		Cache:    memCache,
		Metadata: db, Packs: packs, Registry: registry, Clock: c, ReadUTC: utc,
		AdmitWriter: func(ctx context.Context, r object.WriterRequest) (object.AdmittedWriter, error) {
			admMu.Lock()
			defer admMu.Unlock()
			if nextFence >= len(fences) {
				return object.AdmittedWriter{}, storage.ErrUnavailable
			}
			fence := fences[nextFence]
			nextFence++
			return object.AdmittedWriter{Fence: fence, ProtectedUntil: r.WallDeadline.Add(15 * time.Minute)}, nil
		},
		Chunking:    engine.ChunkingParams{Min: 16 << 10, Avg: 16 << 10, Max: 16 << 10},
		Compression: engine.CompressionPolicy{Enabled: true, Codec: "zstd", Level: 3, MinSavingRatio: big.NewRat(1, 10)},
		Events:      debugEvents{},
		Reader:      reader,
		BucketPolicy: func(id string) (config.BucketSnapshot, error) {
			return validator.NewBucketSnapshot(id, 1, config.DefaultCachePolicy())
		},
		Budget:         readpath.Budget{Capacity: bud, Prefetch: pref},
		MaxOutstanding: 4,
		RequestTimeout: 30 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, cr := range creds {
		env.creds[cr.ID] = Credential{
			UserID: cr.UserID, Secret: "secret-" + cr.UserID, Active: cr.Active,
			SystemAdmin: cr.SystemAdmin, DebugAllowed: cr.DebugAllowed,
		}
	}
	handlers := NewBucketHandlers(db)
	for op, h := range NewObjectHandlers(svc, svc, svc, nil) {
		handlers[op] = h
	}
	env.router = &Router{
		Region:      authRegion,
		Credentials: credentialMap(env.creds),
		Access:      auth.NewSnapshots(db, c),
		Now:         func() time.Time { return c.Now().WallUTC },
		Handlers:    handlers,
	}
	return env
}

type readUTCSource func(context.Context) (storage.R1Bounds, error)

func (f readUTCSource) Bounds(ctx context.Context) (storage.R1Bounds, error) { return f(ctx) }

func (e *debugTestEnv) send(t *testing.T, credID, method, path string, body []byte, signedDebug, unsignedDebug bool) *httptest.ResponseRecorder {
	t.Helper()
	cr := e.creds[credID]
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, "http://s3.test"+path, reader)
	if len(body) > 0 {
		req.Header.Set("Content-Length", strconv.Itoa(len(body)))
	}
	sum := sha256.Sum256(body)
	payloadHash := hex.EncodeToString(sum[:])
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	if signedDebug {
		req.Header.Set("X-Dsg-Debug", "1")
	}
	if err := v4.NewSigner().SignHTTP(context.Background(), aws.Credentials{AccessKeyID: credID, SecretAccessKey: cr.Secret}, req, payloadHash, "s3", authRegion, e.clock.Now().WallUTC); err != nil {
		t.Fatal(err)
	}
	if unsignedDebug {
		req.Header.Set("X-Dsg-Debug", "1")
	}
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

func (e *debugTestEnv) sendPresigned(t *testing.T, credID, method, path string, body []byte, signedDebug bool) *httptest.ResponseRecorder {
	t.Helper()
	cr := e.creds[credID]
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, "http://s3.test"+path, reader)
	q := req.URL.Query()
	q.Set("X-Amz-Expires", "900")
	req.URL.RawQuery = q.Encode()
	if signedDebug {
		req.Header.Set("X-Dsg-Debug", "1")
	}
	uri, headers, err := v4.NewSigner().PresignHTTP(context.Background(), aws.Credentials{AccessKeyID: credID, SecretAccessKey: cr.Secret}, req, unsignedPayload, "s3", authRegion, e.clock.Now().WallUTC,
		func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true })
	if err != nil {
		t.Fatal(err)
	}
	outReq := httptest.NewRequest(method, uri, bytes.NewReader(body))
	outReq.Header = headers
	outReq.Header.Del("Host")
	outReq.ContentLength = int64(len(body))
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, outReq)
	return rec
}

func assertNoDSGHeaders(t *testing.T, h http.Header) {
	t.Helper()
	for name := range h {
		if strings.HasPrefix(strings.ToLower(name), "x-dsg-") {
			t.Fatalf("unexpected physical debug header leaked: %s=%v", name, h[name])
		}
	}
}

func TestDebugHeadersGateAndPhysicalCounts(t *testing.T) {
	e := newDebugTestEnv(t)
	chunkA := bytes.Repeat([]byte("chunk-alpha-"), (16<<10)/len("chunk-alpha-")+1)[:16<<10]
	chunkB := bytes.Repeat([]byte("chunk-bravo-"), (16<<10)/len("chunk-bravo-")+1)[:16<<10]
	repeated := bytes.Repeat(chunkA, 5)

	// 1. System admin with debug_allowed who owns admin-own: PUT emits exact physical headers.
	rec := e.send(t, "ADMINDEBUG0000000003", http.MethodPut, "/admin-own/rep.bin", repeated, true, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner debug PUT: %d %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Dsg-Chunks-Total") != "5" ||
		rec.Header().Get("X-Dsg-Chunks-Dup") != "4" ||
		rec.Header().Get("X-Dsg-Chunks-Revived") != "0" ||
		rec.Header().Get("X-Dsg-Bytes-Logical") != strconv.Itoa(len(repeated)) {
		t.Fatalf("unexpected write debug headers: %+v", rec.Header())
	}
	stored, err := strconv.Atoi(rec.Header().Get("X-Dsg-Bytes-Stored"))
	if err != nil || stored <= 0 {
		t.Fatalf("expected positive X-Dsg-Bytes-Stored, got %q (%v)", rec.Header().Get("X-Dsg-Bytes-Stored"), err)
	}

	// 2. System admin with debug_allowed and explicit grant on demo: dedup PUT & revival PUT.
	rec1 := e.send(t, "ADMINGRANT0000000005", http.MethodPut, "/demo/first.bin", repeated, true, false)
	if rec1.Code != http.StatusOK || rec1.Header().Get("X-Dsg-Chunks-Dup") != "4" || rec1.Header().Get("X-Dsg-Chunks-Revived") != "0" {
		t.Fatalf("grantee first PUT: %d %+v", rec1.Code, rec1.Header())
	}
	rec2 := e.send(t, "ADMINGRANT0000000005", http.MethodPut, "/demo/dup.bin", repeated, true, false)
	if rec2.Code != http.StatusOK ||
		rec2.Header().Get("X-Dsg-Chunks-Total") != "5" ||
		rec2.Header().Get("X-Dsg-Chunks-Dup") != "5" ||
		rec2.Header().Get("X-Dsg-Chunks-Revived") != "0" ||
		rec2.Header().Get("X-Dsg-Bytes-Stored") != "0" {
		t.Fatalf("grantee dedup PUT: %d %+v", rec2.Code, rec2.Header())
	}

	// Archive the pack holding chunkA -> revival PUT records revived=1, dup=4, stored>0.
	keyA, err := engine.HashChunk("default", chunkA)
	if err != nil {
		t.Fatal(err)
	}
	chunks, err := e.db.GetChunks(context.Background(), []storage.ChunkKey{keyA}, true)
	if err != nil {
		t.Fatal(err)
	}
	packRef := chunks[keyA].Location.Pack
	packInfos, err := e.db.GetPacks(context.Background(), []storage.PackRef{packRef})
	if err != nil {
		t.Fatal(err)
	}
	coldInfo := packInfos[packRef]
	coldInfo.Class = storage.Glacier
	coldInfo.State = "Cold"
	coldInfo.StateRevision++
	if err := e.db.SetPackForTest(coldInfo); err != nil {
		t.Fatal(err)
	}
	if err := e.packs.ChangeStorageClass(context.Background(), packRef, storage.Glacier); err != nil {
		t.Fatal(err)
	}

	recRev := e.send(t, "ADMINGRANT0000000005", http.MethodPut, "/demo/revive.bin", repeated, true, false)
	if recRev.Code != http.StatusOK ||
		recRev.Header().Get("X-Dsg-Chunks-Total") != "5" ||
		recRev.Header().Get("X-Dsg-Chunks-Dup") != "4" ||
		recRev.Header().Get("X-Dsg-Chunks-Revived") != "1" ||
		recRev.Header().Get("X-Dsg-Bytes-Logical") != strconv.Itoa(len(repeated)) {
		t.Fatalf("revival PUT: %d %+v", recRev.Code, recRev.Header())
	}
	if s, err := strconv.Atoi(recRev.Header().Get("X-Dsg-Bytes-Stored")); err != nil || s <= 0 {
		t.Fatalf("revival BytesStored = %q", recRev.Header().Get("X-Dsg-Bytes-Stored"))
	}

	// 3. GET cache status: miss -> hit -> partial.
	twoChunks := append(bytes.Clone(chunkA), chunkB...)
	if r := e.send(t, "OWNERKEY000000000001", http.MethodPut, "/demo/two.bin", twoChunks, false, false); r.Code != http.StatusOK {
		t.Fatalf("two.bin PUT: %d %s", r.Code, r.Body.String())
	}
	// Reset cache so two.bin starts cold.
	headTwo, err := e.db.GetObject(context.Background(), storage.ObjectKey{Bucket: "demo", BucketID: "bkt-demo-debug", Key: "two.bin"})
	if err != nil {
		t.Fatal(err)
	}
	pages, err := e.db.GetManifest(context.Background(), headTwo.ManifestID)
	if err != nil {
		t.Fatal(err)
	}
	keyB, err := engine.HashChunk("default", chunkB)
	if err != nil {
		t.Fatal(err)
	}
	cMap, err := e.db.GetChunks(context.Background(), []storage.ChunkKey{keyA, keyB}, true)
	if err != nil || len(pages) == 0 {
		t.Fatal(err)
	}
	_ = e.cache.Evict(context.Background(), cMap[keyA].Location.Pack)
	_ = e.cache.Evict(context.Background(), cMap[keyB].Location.Pack)

	getMiss := e.send(t, "ADMINGRANT0000000005", http.MethodGet, "/demo/two.bin", nil, true, false)
	if getMiss.Code != http.StatusOK || getMiss.Header().Get("X-Dsg-Cache") != "miss" {
		t.Fatalf("GET miss: %d %+v", getMiss.Code, getMiss.Header())
	}
	getHit := e.send(t, "ADMINGRANT0000000005", http.MethodGet, "/demo/two.bin", nil, true, false)
	if getHit.Code != http.StatusOK || getHit.Header().Get("X-Dsg-Cache") != "hit" {
		t.Fatalf("GET hit: %d %+v", getHit.Code, getHit.Header())
	}
	// Evict only chunkB's pack (revive.bin put chunkA in a separate pack from two.bin's chunkB).
	_ = e.cache.Evict(context.Background(), cMap[keyB].Location.Pack)
	getPartial := e.send(t, "ADMINGRANT0000000005", http.MethodGet, "/demo/two.bin", nil, true, false)
	if getPartial.Code != http.StatusOK || getPartial.Header().Get("X-Dsg-Cache") != "partial" {
		t.Fatalf("GET partial: %d %+v", getPartial.Code, getPartial.Header())
	}

	// 4. Cold GLACIER GET error: x-dsg-restore requested -> in-progress -> omitted on failure/off.
	if r := e.send(t, "OWNERKEY000000000001", http.MethodPut, "/demo/cold.bin", chunkB, false, false); r.Code != http.StatusOK {
		t.Fatalf("cold.bin PUT: %d %s", r.Code, r.Body.String())
	}
	coldChunks, err := e.db.GetChunks(context.Background(), []storage.ChunkKey{keyB}, true)
	if err != nil {
		t.Fatal(err)
	}
	coldRef := coldChunks[keyB].Location.Pack
	coldPacks, err := e.db.GetPacks(context.Background(), []storage.PackRef{coldRef})
	if err != nil {
		t.Fatal(err)
	}
	coldPack := coldPacks[coldRef]
	coldPack.Class = storage.Glacier
	coldPack.State = "Cold"
	coldPack.StateRevision++
	if err := e.db.SetPackForTest(coldPack); err != nil {
		t.Fatal(err)
	}
	if err := e.packs.ChangeStorageClass(context.Background(), coldRef, storage.Glacier); err != nil {
		t.Fatal(err)
	}

	rReq := e.send(t, "ADMINGRANT0000000005", http.MethodGet, "/demo/cold.bin", nil, true, false)
	if rReq.Code != http.StatusForbidden || errorCode(rReq) != ErrInvalidObjectState || rReq.Header().Get("X-Dsg-Restore") != "requested" {
		t.Fatalf("cold GET requested: %d %s %+v", rReq.Code, rReq.Body.String(), rReq.Header())
	}
	rProg := e.send(t, "ADMINGRANT0000000005", http.MethodGet, "/demo/cold.bin", nil, true, false)
	if rProg.Code != http.StatusForbidden || errorCode(rProg) != ErrInvalidObjectState || rProg.Header().Get("X-Dsg-Restore") != "in-progress" {
		t.Fatalf("cold GET in-progress: %d %s %+v", rProg.Code, rProg.Body.String(), rProg.Header())
	}
	e.restoreMu.Lock()
	e.restoreFail = true
	e.restoreMu.Unlock()
	rFail := e.send(t, "ADMINGRANT0000000005", http.MethodGet, "/demo/cold.bin", nil, true, false)
	if rFail.Code != http.StatusForbidden || errorCode(rFail) != ErrInvalidObjectState {
		t.Fatalf("cold GET restoreFail: %d %s", rFail.Code, rFail.Body.String())
	}
	assertNoDSGHeaders(t, rFail.Header())
}

func TestDebugHeadersDeniedAndNeverLeakedToRegularUsers(t *testing.T) {
	e := newDebugTestEnv(t)
	payload := bytes.Repeat([]byte("privacy-guard-"), (16<<10)/len("privacy-guard-")+1)[:16<<10]

	// Seed an existing object and a Cold object.
	if rec := e.send(t, "OWNERKEY000000000001", http.MethodPut, "/demo/warm.bin", payload, false, false); rec.Code != http.StatusOK {
		t.Fatal(rec.Code, rec.Body.String())
	}
	coldBody := bytes.Repeat([]byte("cold-secret-"), (16<<10)/len("cold-secret-")+1)[:16<<10]
	if rec := e.send(t, "OWNERKEY000000000001", http.MethodPut, "/demo/cold.bin", coldBody, false, false); rec.Code != http.StatusOK {
		t.Fatal(rec.Code, rec.Body.String())
	}
	keyCold, _ := engine.HashChunk("default", coldBody)
	chunks, _ := e.db.GetChunks(context.Background(), []storage.ChunkKey{keyCold}, true)
	coldRef := chunks[keyCold].Location.Pack
	coldPacks, _ := e.db.GetPacks(context.Background(), []storage.PackRef{coldRef})
	coldPack := coldPacks[coldRef]
	coldPack.Class = storage.Glacier
	coldPack.State = "Cold"
	coldPack.StateRevision++
	_ = e.db.SetPackForTest(coldPack)
	_ = e.packs.ChangeStorageClass(context.Background(), coldRef, storage.Glacier)

	// Normal users (owner, grantee), system_admin without debug_allowed,
	// non-admin with debug_allowed, and unsigned/invalid debug headers must NEVER receive x-dsg-*.
	for _, tc := range []struct {
		name          string
		cred          string
		signedDebug   bool
		unsignedDebug bool
	}{
		{"bucket owner with signed debug", "OWNERKEY000000000001", true, false},
		{"normal grantee with signed debug", "GRANTEEKEY0000000002", true, false},
		{"system_admin without debug_allowed", "ADMINNODEBUG00000004", true, false},
		{"debug_allowed without system_admin", "FAKEADMINDEBUG000008", true, false},
		{"admin+debug with unsigned header", "ADMINGRANT0000000005", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			putRec := e.send(t, tc.cred, http.MethodPut, "/demo/user-put.bin", payload, tc.signedDebug, tc.unsignedDebug)
			if putRec.Code != http.StatusOK {
				t.Fatalf("PUT: %d %s", putRec.Code, putRec.Body.String())
			}
			assertNoDSGHeaders(t, putRec.Header())

			beforeHas := e.cache.hasCalls.Load()
			getRec := e.send(t, tc.cred, http.MethodGet, "/demo/warm.bin", nil, tc.signedDebug, tc.unsignedDebug)
			if getRec.Code != http.StatusOK {
				t.Fatalf("GET: %d %s", getRec.Code, getRec.Body.String())
			}
			assertNoDSGHeaders(t, getRec.Header())
			if afterHas := e.cache.hasCalls.Load(); afterHas != beforeHas {
				t.Fatalf("non-debug GET invoked Cache.Has %d times", afterHas-beforeHas)
			}

			coldRec := e.send(t, tc.cred, http.MethodGet, "/demo/cold.bin", nil, tc.signedDebug, tc.unsignedDebug)
			if coldRec.Code != http.StatusForbidden || errorCode(coldRec) != ErrInvalidObjectState {
				t.Fatalf("Cold GET: %d %s", coldRec.Code, coldRec.Body.String())
			}
			assertNoDSGHeaders(t, coldRec.Header())
		})
	}

	// Wrong header values or multi-valued X-Dsg-Debug must not unlock debug.
	for _, tc := range []struct {
		name   string
		values []string
	}{
		{"value zero", []string{"0"}},
		{"duplicate values", []string{"1", "0"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "http://s3.test/demo/warm.bin", nil)
			req.Header.Set("X-Amz-Content-Sha256", emptySHA256)
			for _, v := range tc.values {
				req.Header.Add("X-Dsg-Debug", v)
			}
			_ = v4.NewSigner().SignHTTP(context.Background(), aws.Credentials{AccessKeyID: "ADMINGRANT0000000005", SecretAccessKey: "secret-admin-grantee"}, req, emptySHA256, "s3", authRegion, e.clock.Now().WallUTC)
			rec := httptest.NewRecorder()
			e.router.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d %s", rec.Code, rec.Body.String())
			}
			assertNoDSGHeaders(t, rec.Header())
		})
	}

	// System admin with debug_allowed must STILL pass separate owner/grant authorization.
	for _, tc := range []struct {
		name, cred, method, path string
		body                     []byte
	}{
		{"admin+debug no grant GET warm", "ADMINDEBUG0000000003", http.MethodGet, "/demo/warm.bin", nil},
		{"admin+debug no grant GET cold", "ADMINDEBUG0000000003", http.MethodGet, "/demo/cold.bin", nil},
		{"admin+debug no grant PUT", "ADMINDEBUG0000000003", http.MethodPut, "/demo/denied.bin", payload},
		{"admin+debug write-only GET", "ADMINWRITE0000000006", http.MethodGet, "/demo/warm.bin", nil},
		{"admin+debug read-only PUT", "ADMINREAD00000000007", http.MethodPut, "/demo/denied.bin", payload},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := e.send(t, tc.cred, tc.method, tc.path, tc.body, true, false)
			if rec.Code != http.StatusForbidden || errorCode(rec) != ErrAccessDenied {
				t.Fatalf("got %d %s, want 403 AccessDenied", rec.Code, rec.Body.String())
			}
			assertNoDSGHeaders(t, rec.Header())
		})
	}

	// Presigned requests: query ?x-dsg-debug=1 is rejected; signed header obeys the same gate.
	qDebug := e.sendPresigned(t, "OWNERKEY000000000001", http.MethodGet, "/demo/warm.bin?x-dsg-debug=1", nil, false)
	if qDebug.Code != http.StatusNotImplemented || errorCode(qDebug) != ErrNotImplemented {
		t.Fatalf("query debug got %d %s, want 501 NotImplemented", qDebug.Code, qDebug.Body.String())
	}
	assertNoDSGHeaders(t, qDebug.Header())

	presignedUser := e.sendPresigned(t, "OWNERKEY000000000001", http.MethodGet, "/demo/warm.bin", nil, true)
	if presignedUser.Code != http.StatusOK {
		t.Fatalf("presigned owner GET: %d %s", presignedUser.Code, presignedUser.Body.String())
	}
	assertNoDSGHeaders(t, presignedUser.Header())

	presignedNoGrant := e.sendPresigned(t, "ADMINDEBUG0000000003", http.MethodGet, "/demo/warm.bin", nil, true)
	if presignedNoGrant.Code != http.StatusForbidden || errorCode(presignedNoGrant) != ErrAccessDenied {
		t.Fatalf("presigned admin no-grant GET: %d %s", presignedNoGrant.Code, presignedNoGrant.Body.String())
	}
	assertNoDSGHeaders(t, presignedNoGrant.Header())

	presignedAdmin := e.sendPresigned(t, "ADMINGRANT0000000005", http.MethodGet, "/demo/warm.bin", nil, true)
	if presignedAdmin.Code != http.StatusOK || !slices.Contains([]string{"hit", "miss", "partial"}, presignedAdmin.Header().Get("X-Dsg-Cache")) {
		t.Fatalf("presigned admin+grant GET: %d %+v", presignedAdmin.Code, presignedAdmin.Header())
	}

	// Admin bearer token never authenticates S3 or unlocks debug headers.
	bearerReq := httptest.NewRequest(http.MethodGet, "http://s3.test/demo/warm.bin", nil)
	bearerReq.Header.Set("Authorization", "Bearer 0123456789abcdef0123456789abcdef.secret")
	bearerReq.Header.Set("X-Dsg-Debug", "1")
	bearerRec := httptest.NewRecorder()
	e.router.ServeHTTP(bearerRec, bearerReq)
	if bearerRec.Code != http.StatusBadRequest || errorCode(bearerRec) != ErrInvalidArgument {
		t.Fatalf("bearer on S3: %d %s", bearerRec.Code, bearerRec.Body.String())
	}
	assertNoDSGHeaders(t, bearerRec.Header())
}
