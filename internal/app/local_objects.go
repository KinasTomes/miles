package app

import (
	"context"
	"math/big"
	"time"

	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/coordination"
	"github.com/KinasTomes/miles/internal/engine"
	"github.com/KinasTomes/miles/internal/engine/write"
	"github.com/KinasTomes/miles/internal/metadata/badger"
	"github.com/KinasTomes/miles/internal/object"
	"github.com/KinasTomes/miles/internal/readpath"
	"github.com/KinasTomes/miles/internal/storage"
	packs3 "github.com/KinasTomes/miles/internal/storage/s3"
)

// Only CreateBucket needs extra admission here; object.Service owns per-operation
// admission for PUT/DELETE. No request header can supply history, epoch or fence.
type localMetadata struct {
	*badger.Store
	authority *badger.LocalAuthority
}

func (m *localMetadata) CreateBucket(ctx context.Context, r storage.BucketCreate) (storage.Bucket, bool, error) {
	id, err := write.NewManifestID()
	if err != nil {
		return storage.Bucket{}, false, err
	}
	deadline := time.Now().Add(30 * time.Minute)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	c, err := m.authority.Admit(ctx, r.CredentialID, storage.ObjectKey{}, id, deadline)
	if err != nil {
		return storage.Bucket{}, false, err
	}
	return m.Store.CreateBucket(storage.WithMutation(ctx, c), r)
}

func localObjects(m *badger.Store, packs *packs3.Store, resident storage.Cache, cachePolicy config.CachePolicy) (*object.Service, *localMetadata, func(), error) {
	authority, err := m.StartLocalRuntime(context.Background(), DevUser, 1<<30)
	if err != nil {
		return nil, nil, nil, err
	}
	clk := clock.NewReal()
	lease := coordination.LeaseLimits{Lifetime: time.Minute, Heartbeat: 15 * time.Second}
	gate, err := coordination.NewR1Gate(clk, authority.Incarnation(), lease)
	if err != nil {
		return nil, nil, nil, err
	}
	registry, err := coordination.NewWriterRegistry(gate, lease, coordination.Hooks{})
	if err != nil {
		return nil, nil, nil, err
	}
	// Explicit development budgets, separate from resident LRU and process RSS.
	node := readpath.Budget{
		Capacity: readpath.Cost{ChunkSlots: 64, StoredBytes: 256 << 20, IOSlots: 64, IOBytes: 256 << 20, RawBytes: 256 << 20, WorkspaceBytes: 512 << 20, CopyBytes: 512 << 20},
		Prefetch: readpath.Cost{ChunkSlots: 32, StoredBytes: 128 << 20, IOSlots: 32, IOBytes: 128 << 20, RawBytes: 128 << 20, WorkspaceBytes: 256 << 20, CopyBytes: 256 << 20},
	}
	request := readpath.Budget{
		Capacity: readpath.Cost{ChunkSlots: 8, StoredBytes: 32 << 20, IOSlots: 8, IOBytes: 32 << 20, RawBytes: 32 << 20, WorkspaceBytes: 128 << 20, CopyBytes: 128 << 20},
		Prefetch: readpath.Cost{ChunkSlots: 4, StoredBytes: 16 << 20, IOSlots: 4, IOBytes: 16 << 20, RawBytes: 16 << 20, WorkspaceBytes: 64 << 20, CopyBytes: 64 << 20},
	}
	admission, err := readpath.NewAdmission(readpath.Limits{Node: readpath.ScopeLimits{ActiveRequests: 64, Budget: node}, Bucket: readpath.ScopeLimits{ActiveRequests: 64, Budget: node}})
	if err != nil {
		return nil, nil, nil, err
	}
	validator, err := config.NewPolicyValidator(config.DSGPolicyLimits())
	if err != nil {
		return nil, nil, nil, err
	}
	// Fixed, validated local development policy; no Admin policy mutation is wired.
	policy := config.DefaultGlobalPolicy()
	global, err := validator.NewGlobalSnapshot(1, policy)
	if err != nil {
		return nil, nil, nil, err
	}
	reader, err := readpath.NewWarmReader(readpath.WarmOptions{
		Metadata: m, Packs: packs, Cache: resident, Admission: admission, Gate: gate, Clock: clk, Global: global, Metrics: &readpath.WarmMetrics{},
		// net/http read/write, TLS record and SDK stream buffers; no custom buffering.
		// Resident connection pools and runtime/RSS headroom are outside this budget.
		TransportBytes: 128 << 10, MaxManifestPages: 1024, MaxManifestChunks: 65536,
	})
	if err != nil {
		return nil, nil, nil, err
	}
	ratio, ok := new(big.Rat).SetString(policy.Compression.MinSavingRatio.String())
	if !ok {
		return nil, nil, nil, storage.ErrInvalid
	}
	service, err := object.New(object.Options{
		Metadata: m, Packs: coordination.ProtectPackStore(packs), Cache: resident, Registry: registry, Clock: clk,
		AdmitWriter: func(ctx context.Context, r object.WriterRequest) (object.AdmittedWriter, error) {
			c, err := authority.Admit(ctx, r.CredentialID, r.Object, r.OpID, r.WallDeadline)
			if err != nil {
				return object.AdmittedWriter{}, err
			}
			return object.AdmittedWriter{Fence: c.Writer, Producer: &c, ProtectedUntil: r.WallDeadline.Add(15 * time.Minute)}, nil
		},
		Chunking:    engine.ChunkingParams{Min: int(policy.Chunking.MinBytes), Avg: int(policy.Chunking.AvgBytes), Max: int(policy.Chunking.MaxBytes)},
		Compression: engine.CompressionPolicy{Enabled: policy.Compression.Enabled, Codec: policy.Compression.Codec, Level: int(policy.Compression.Level), MinSavingRatio: ratio},
		Events:      object.MetadataEvents{}, Reader: reader,
		BucketPolicy: func(id string) (config.BucketSnapshot, error) {
			return validator.NewBucketSnapshot(id, 1, cachePolicy)
		},
		Budget: request, MaxOutstanding: 4, RequestTimeout: 30 * time.Minute, Uploads: m,
	})
	if err != nil {
		return nil, nil, nil, err
	}
	return service, &localMetadata{Store: m, authority: authority}, registry.Invalidate, nil
}
