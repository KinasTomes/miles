package app

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"path/filepath"
	"time"

	"github.com/KinasTomes/miles/internal/api/admin"
	"github.com/KinasTomes/miles/internal/cache"
	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/coordination"
	"github.com/KinasTomes/miles/internal/engine"
	"github.com/KinasTomes/miles/internal/jobs"
	"github.com/KinasTomes/miles/internal/metadata/dynamodb"
	"github.com/KinasTomes/miles/internal/object"
	"github.com/KinasTomes/miles/internal/observability"
	"github.com/KinasTomes/miles/internal/recovery"
	"github.com/KinasTomes/miles/internal/storage"
	packs3 "github.com/KinasTomes/miles/internal/storage/s3"
	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
)

func sharedStore(c config.Startup, clients runtimeClients, utc storage.R1TimeSource) (*dynamodb.Store, error) {
	tables := map[string]string{}
	for name := range dynamodb.Tables() {
		tables[name] = c.DDBTablePrefix + name
	}
	return dynamodb.New(dynamodb.Options{API: clients.metadata, Tables: tables, Authority: &dynamodb.SharedAuthority{Time: utc, Lifetime: time.Minute}, RequireProducers: true})
}

func bootstrapShared(c config.Startup) (Dependencies, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	clients, err := newRuntimeClients(ctx, c)
	if err != nil {
		return Dependencies{}, err
	}
	id, err := storage.NewBucketID()
	if err != nil {
		return Dependencies{}, err
	}
	clk := clock.NewReal()
	utc := coordination.Chrony{Clock: clk, Reference: c.UTCReference, RestartID: id}
	k, seal, cursor, err := clients.keys(ctx, c.RuntimeSecretARN)
	if err != nil {
		clients.transport.CloseIdleConnections()
		return Dependencies{}, err
	}
	d, err := composeShared(ctx, c, clients, clk, utc, k.KeyID, seal, cursor)
	if err != nil {
		clients.transport.CloseIdleConnections()
	}
	return d, err
}

// composeShared never provisions identities, gates, policy defaults or history.
// Those are offline, installation-wide operations, not node startup behavior.
func composeShared(ctx context.Context, c config.Startup, clients runtimeClients, clk clock.Clock, utc storage.R1TimeSource, keyID string, seal, cursor []byte) (d Dependencies, err error) {
	if _, err = utc.Bounds(ctx); err != nil {
		return d, err
	}
	m, err := sharedStore(c, clients, utc)
	if err != nil {
		return d, err
	}
	g, err := m.GetR1Gate(ctx)
	if err != nil || g.Validate() != nil || !g.RecoveryEnabled {
		return d, errors.New("shared recovery history must be provisioned before starting nodes")
	}
	id, err := storage.NewBucketID()
	if err != nil {
		return d, err
	}
	limits := coordination.LeaseLimits{Lifetime: time.Minute, Heartbeat: 15 * time.Second}
	journalObjects, err := recovery.NewS3Objects(clients.packs, c.PackBucket, "", c.PackKMSKeyID)
	if err != nil {
		return d, err
	}
	catalog, err := recovery.LoadCatalog(ctx, journalObjects)
	if err != nil {
		return d, err
	}
	if catalog.History != g.HistoryID || catalog.Incarnation != g.Incarnation {
		return d, storage.ErrFenced
	}
	gate, err := coordination.NewSharedR1Gate(clk, g.Incarnation, c.RuntimeNodeID, limits, m, utc)
	if err != nil {
		return d, err
	}
	registry, err := coordination.NewWriterRegistry(gate, limits, coordination.Hooks{})
	if err != nil {
		return d, err
	}
	if err = runtimeDirectory(c.LocalDataPath); err != nil {
		return d, err
	}
	lru, err := cache.NewLRU(cache.Limits{CapacityBytes: 64 << 20, MaxEntries: 1024, MaxCopyBytes: 4 << 20})
	if err != nil {
		return d, err
	}
	disk, err := cache.NewDisk(filepath.Join(c.LocalDataPath, "cache"), cache.Limits{CapacityBytes: 1 << 30, MaxEntries: 16384, MaxCopyBytes: 4 << 20})
	if err != nil {
		return d, err
	}
	defer func() {
		if err != nil {
			_ = disk.Close()
			registry.Invalidate()
		}
	}()
	resident, err := cache.NewNode(c.RuntimeNodeID+"-"+id, lru, disk)
	if err != nil {
		return d, err
	}
	packs, err := packs3.New(clients.packs, c.PackBucket, c.PackKMSKeyID)
	if err != nil {
		return d, err
	}
	metadata := &sharedMetadata{Store: m, utc: utc, owner: c.RuntimeNodeID + "-" + id}
	journal := &journalRuntime{store: m, objects: journalObjects, catalog: catalog, clock: clk, authority: sharedJobAuthority{metadata}}
	v, err := config.NewPolicyValidator(config.DSGPolicyLimits())
	if err != nil {
		return d, err
	}
	admission, err := objectReadAdmission()
	if err != nil {
		return d, err
	}
	objects := func(ctx context.Context, bucket string) (*object.Service, error) {
		record, e := m.GetPolicy(ctx, 0)
		if e != nil {
			return nil, e
		}
		global, e := v.DecodeGlobal(record)
		if e != nil {
			return nil, e
		}
		p := global.Policy()
		ratio, ok := new(big.Rat).SetString(p.Compression.MinSavingRatio.String())
		if !ok {
			return nil, storage.ErrInvalid
		}
		return objectRuntime(object.Options{Metadata: m, Packs: coordination.ProtectPackStore(packs), Cache: resident, Registry: registry, Clock: clk, Uploads: m, Events: object.MetadataEvents{},
			AdmitWriter: func(ctx context.Context, r object.WriterRequest) (object.AdmittedWriter, error) {
				mutation, w, e := registry.AdmitProducer(ctx, r.Object, r.OpID, r.Deadline)
				if e != nil {
					return object.AdmittedWriter{}, e
				}
				_, stop, e := w.KeepAlive(ctx)
				if e != nil {
					_ = w.Release()
					return object.AdmittedWriter{}, e
				}
				bounds, e := utc.Bounds(ctx)
				if e != nil {
					stop()
					_ = w.Release()
					return object.AdmittedWriter{}, e
				}
				protected := maxTime(r.WallDeadline, bounds.Upper.Add(30*time.Minute)).Add(15 * time.Minute)
				return object.AdmittedWriter{Producer: &mutation, Fence: mutation.Writer, ProtectedUntil: protected, Finish: func() { stop(); _ = w.Release() }}, nil
			},
			Chunking: engine.ChunkingParams{Min: int(p.Chunking.MinBytes), Avg: int(p.Chunking.AvgBytes), Max: int(p.Chunking.MaxBytes)}, Compression: engine.CompressionPolicy{Enabled: p.Compression.Enabled, Codec: p.Compression.Codec, Level: int(p.Compression.Level), MinSavingRatio: ratio},
			BucketPolicy: func(id string) (config.BucketSnapshot, error) {
				record, e := m.GetBucketPolicy(ctx, bucket, id, 0)
				if e != nil {
					return config.BucketSnapshot{}, e
				}
				return v.DecodeBucket(id, record.Revision, record.Document)
			},
		}, m, packs, gate, global, admission)
	}
	sealer, err := admin.NewAESSealer(keyID, seal)
	if err != nil {
		return d, err
	}
	a := &admin.Options{Store: m, Clock: clk, Sealer: sealer, Cursors: admin.Keyring{Current: keyID, Keys: map[string][]byte{keyID: cursor}}, Admit: func(ctx context.Context, _ string) (storage.MutationContext, error) { return metadata.admit(ctx) }, Finish: func(ctx context.Context, c storage.MutationContext) { metadata.finish(ctx, c) }}
	if err = a.Cursors.Validate(); err != nil {
		return d, err
	}
	probe := func(ctx context.Context) error {
		if _, e := utc.Bounds(ctx); e != nil {
			return e
		}
		current, e := m.GetR1Gate(ctx)
		if e != nil {
			return e
		}
		if current.Incarnation != g.Incarnation || current.HistoryID != g.HistoryID || current.State != "Open" && current.State != "RecoveryCut" {
			return storage.ErrFenced
		}
		if _, e = m.GetPolicy(ctx, 0); e != nil {
			return e
		}
		_, e = clients.packs.HeadBucket(ctx, &awss3.HeadBucketInput{Bucket: aws.String(c.PackBucket)})
		return e
	}
	_, err = objects(ctx, "")
	if err != nil {
		return d, err
	}
	metrics := new(observability.Operations)
	identity := jobs.Node{ID: c.RuntimeNodeID + "-" + id, Incarnation: id, ServiceIncarnation: g.Incarnation, Profile: c.Profile, Address: c.AdminAddress, AppliedPolicyRevision: 1}
	nodeRegistry := &jobs.Registry{Store: m, Clock: clk, Identity: identity}
	runner := &jobs.Runner{Service: jobs.Service{Store: m, Policies: m, Now: func() time.Time { return clk.Now().WallUTC }, Observe: metrics.JobUpdate}, Authority: sharedJobAuthority{metadata}, Node: identity, Clock: clk, Executors: map[jobs.Type]jobs.Executor{}}
	cluster := &jobs.Cluster{Runner: runner, Registry: nodeRegistry}
	runner.ObserveError = metrics.DispatchFailure
	runner.Executors["cache_purge"] = cluster.CacheExecutor(resident.PurgeNode)
	runner.Executors["journal_flush"] = jobs.Executor{Kind: "recovery", Periodic: time.Minute, Validate: func(p json.RawMessage) error {
		var params map[string]json.RawMessage
		if json.Unmarshal(p, &params) != nil || params == nil || len(params) != 0 {
			return storage.ErrInvalid
		}
		return nil
	}, Step: func(ctx context.Context, j jobs.Job, _ config.GlobalSnapshot) (jobs.Step, error) {
		if j.DryRun || j.State == jobs.Cancelling {
			return jobs.Step{Settled: true, Done: true, Result: json.RawMessage(`{"exported":false}`)}, nil
		}
		w := recovery.Worker{Store: m, Payloads: m, Objects: journalObjects, Catalog: catalog, Clock: clk, Fence: j.Worker}
		e := w.Flush(ctx)
		if e != nil {
			return jobs.Step{}, e
		}
		return jobs.Step{Settled: true, Done: true, Result: json.RawMessage(`{"exported":true}`)}, nil
	}}
	runner.Executors["drain"] = cluster.DrainExecutor()
	uploads := uploadJobs{store: m, objects: objects, utc: utc}
	runner.Executors["purge_uploads"] = uploads.executor("purge_uploads")
	runner.Executors["recover_uploads"] = uploads.executor("recover_uploads")
	a.Operations = cluster
	operational := &Operational{Registry: nodeRegistry, Metrics: metrics, Cluster: cluster}
	if clients.metrics != nil {
		operational.CloudWatch, err = observability.NewCloudWatch(clients.metrics, c.Profile)
		if err != nil {
			return d, err
		}
	}
	if c.Profile == "cloud" {
		operational.OnDrain = func(ctx context.Context) error { return requestCloudDrain(ctx, c.LocalDataPath, identity) }
	}
	closeRuntime := func(ctx context.Context) error {
		e := registry.Quiescent()
		flushErr := journal.flush(ctx)
		registry.Invalidate()
		clients.transport.CloseIdleConnections()
		return errors.Join(e, flushErr, metadata.closeError(), resident.Close())
	}
	return Dependencies{Metadata: metadata, Packs: coordination.ProtectPackStore(packs), Cache: resident, ObjectsForRequest: objects, S3ListTokenKey: cursor, Admin: a, Probe: probe, Journal: journal,
		Operations:       operational,
		closeWithContext: closeRuntime,
		Close:            func() error { return closeRuntime(context.Background()) }}, nil
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
