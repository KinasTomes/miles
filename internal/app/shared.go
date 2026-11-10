package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"path/filepath"
	"time"

	"github.com/KinasTomes/miles/internal/api/admin"
	"github.com/KinasTomes/miles/internal/cache"
	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/coordination"
	"github.com/KinasTomes/miles/internal/engine"
	"github.com/KinasTomes/miles/internal/faults"
	"github.com/KinasTomes/miles/internal/jobs"
	"github.com/KinasTomes/miles/internal/lifecycle"
	"github.com/KinasTomes/miles/internal/metadata/dynamodb"
	"github.com/KinasTomes/miles/internal/object"
	"github.com/KinasTomes/miles/internal/observability"
	"github.com/KinasTomes/miles/internal/recovery"
	"github.com/KinasTomes/miles/internal/stats"
	"github.com/KinasTomes/miles/internal/storage"
	packs3 "github.com/KinasTomes/miles/internal/storage/s3"
	"github.com/aws/aws-sdk-go-v2/aws"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
)

func sharedStore(c config.Startup, clients runtimeClients, utc storage.R1TimeSource) (*dynamodb.Store, error) {
	tables := map[string]string{}
	for name := range dynamodb.Tables() {
		tables[name] = c.DDBTablePrefix + name
	}
	var onRetry func()
	if clients.telemetry != nil {
		onRetry = clients.telemetry.Retry
	}
	return dynamodb.New(dynamodb.Options{OnRetry: onRetry, API: clients.metadata, Tables: tables, Authority: &dynamodb.SharedAuthority{Time: utc, Lifetime: time.Minute}, RequireProducers: true})
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
	faultReg := faults.NewRegistry(c.EnableFaultInjection)
	runtimeMetrics := new(observability.Runtime)
	clients = clients.observed(runtimeMetrics, c.DDBTablePrefix+"Leases")
	if c.EnableFaultInjection {
		dOpt := clients.metadata.Options()
		dOpt.HTTPClient = faults.WrapHTTPClient(dOpt.HTTPClient, faultReg, c.RuntimeNodeID)
		clients.metadata = ddb.New(dOpt)
		sOpt := clients.packs.Options()
		sOpt.HTTPClient = faults.WrapS3HTTPClient(sOpt.HTTPClient, faultReg, c.RuntimeNodeID)
		clients.packs = awss3.New(sOpt)
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
	catalog, err := recovery.LoadSelectedCatalog(ctx, journalObjects, g.HistoryID, g.Incarnation)
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
	faultPacks := faults.WrapPackStore(coordination.ProtectPackStore(packs), faultReg, c.RuntimeNodeID)
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
		return objectRuntime(object.Options{Metadata: m, Packs: coordination.ProtectPackStore(packs), Cache: resident, Registry: registry, Clock: clk, ReadUTC: utc, Uploads: m, Events: object.MetadataEvents{},
			Restore: func(ctx context.Context, view *coordination.WarmView, head storage.ObjectHead, ref storage.PackRef, revision uint64, required time.Time, explicit *storage.RestoreRequest) (bool, error) {
				f, err := view.SharedFence(ctx)
				if err != nil {
					return false, err
				}
				_, created, err := m.QueueReadRestore(ctx, f, head, ref, revision, required, explicit)
				return created, err
			},
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
				if errors.Is(e, storage.ErrNotFound) && !errors.Is(e, storage.ErrBucketNotFound) {
					return v.NewBucketSnapshot(id, 1, config.DefaultCachePolicy())
				}
				if e != nil {
					return config.BucketSnapshot{}, e
				}
				return v.DecodeBucket(id, record.Revision, record.Document)
			},
		}, m, faultPacks, gate, global, admission, &runtimeMetrics.Warm)
	}
	sealer, err := admin.NewAESSealer(keyID, seal)
	if err != nil {
		return d, err
	}
	a := &admin.Options{Faults: faultReg, NodeID: c.RuntimeNodeID, Store: m, Clock: clk, Sealer: sealer, Cursors: admin.Keyring{Current: keyID, Keys: map[string][]byte{keyID: cursor}}, Admit: func(ctx context.Context, _ string) (storage.MutationContext, error) { return metadata.admit(ctx) }, Finish: func(ctx context.Context, c storage.MutationContext) { metadata.finish(ctx, c) }}
	if err = a.Cursors.Validate(); err != nil {
		return d, err
	}
	probe := func(ctx context.Context) error {
		if faultReg.ShouldInject(time.Now(), faults.FaultKillNode, c.RuntimeNodeID) {
			return errors.New("fault injected: kill_node")
		}
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
	nodeRegistry.CacheSnapshot = func() jobs.CacheSample {
		s := resident.Stats()
		tier := func(t cache.TierStats) jobs.CacheTier {
			return jobs.CacheTier{UsedBytes: t.UsedBytes, CapacityBytes: t.CapacityBytes, Entries: t.Entries, MaxEntries: t.MaxEntries, Evictions: t.Evictions}
		}
		return jobs.CacheSample{Hits: s.Hits, Misses: s.Misses, Errors: s.Errors, Lookups: s.Lookups, Puts: s.Puts, Memory: tier(s.Memory), Disk: tier(s.Disk)}
	}
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
	// Opaque prototype bases cannot establish checkpoint completeness. Keep
	// this capability unavailable until an audited typed base is provisioned.
	if _, e := (recovery.Checkpointer{Objects: journalObjects, Catalog: catalog}).Load(ctx, recovery.AttachmentReference{Key: "recovery/histories/" + catalog.History + "/genesis-" + catalog.Genesis.SHA256 + ".json", Reference: catalog.Genesis}); e == nil {
		retention := recovery.Retention{Store: m, Checkpointer: recovery.Checkpointer{Objects: journalObjects, Catalog: catalog}}
		marker := &lifecycle.Marker{Store: m, Pinned: m.ObservePackPins, RecoveryRoots: retention.ProtectedRoots, MaxRecords: 10000, MaxReferences: 32768}
		gcWorker := &lifecycle.Worker{Store: m, Packs: packs, Catalog: &lifecycle.Catalog{Store: m, Now: func() time.Time { return clk.Now().WallUTC }, MaxRecords: 10000, MaxReferences: 32768}, UTC: utc, Marker: marker, Sweep: packs, VerifyCutAfter: retention.VerifyCutAfter}
		runner.Executors["compaction"] = gcWorker.Executor("compaction")
		runner.Executors["gc"] = gcWorker.Executor("gc")
		runner.Executors["recovery_checkpoint"] = (recovery.CheckpointWorker{Store: m, Objects: journalObjects, Packs: packs, Catalog: catalog}).Executor()
	} else if !errors.Is(e, storage.ErrInvalid) {
		return d, e
	}
	uploads := uploadJobs{store: m, utc: utc, objects: func(ctx context.Context, bucket string) (uploadRecovery, error) { return objects(ctx, bucket) }}
	runner.Executors["purge_uploads"] = uploads.executor("purge_uploads")
	runner.Executors["recover_uploads"] = uploads.executor("recover_uploads")
	packCatalog := &lifecycle.Catalog{Store: m, Now: func() time.Time { return clk.Now().WallUTC }, MaxRecords: 10000, MaxReferences: 32768}
	lifecycleWorker := &lifecycle.Worker{Store: m, Packs: packs, Catalog: packCatalog, UTC: utc, Jobs: &runner.Service}
	for _, kind := range []jobs.Type{"tiering", "pack_restore", "pack_promote"} {
		runner.Executors[kind] = lifecycleWorker.Executor(kind)
	}
	// Use the existing cluster-owner periodic scheduler. Selection queues exact
	// children only; actual movement still reserves the native maintenance gate.
	tiering := runner.Executors["tiering"]
	tiering.Periodic = time.Minute
	runner.Executors["tiering"] = tiering
	scrubWorker := &jobs.ScrubWorker{Store: m, Packs: faultPacks}
	runner.Executors["scrub"] = scrubWorker.Executor()
	measurements, err := stats.New(stats.Options{Store: m, Inventory: packs3.VersionInventory{Client: clients.packs, Bucket: c.PackBucket}, Now: func() time.Time { return clk.Now().WallUTC }, MaxRecords: 10000, MaxReferences: 100000, MaxPhysicalObjects: 10000, Timeout: 20 * time.Second, MaxAge: 2 * time.Minute, Interval: 30 * time.Second})
	if err != nil {
		return d, err
	}
	a.Statistics = measurements
	a.PackInventory = packCatalog
	a.Operations = cluster
	a.CacheStatistics = cluster
	obsInterval := time.Duration(c.ObservationIntervalSeconds) * time.Second
	if obsInterval <= 0 {
		obsInterval = 30 * time.Second
	}
	operational := &Operational{Registry: nodeRegistry, Metrics: metrics, Runtime: runtimeMetrics, Collect: runtimeObserver(m, runtimeMetrics, clk, g.HistoryID, c.DeepObservation), CollectInterval: obsInterval, Cluster: cluster}
	if clients.metrics != nil {
		operational.CloudWatch, err = observability.NewCloudWatch(clients.metrics, c.Profile, c.DDBTablePrefix)
		if err != nil {
			return d, err
		}
	}
	if c.Profile == "cloud" {
		operational.OnDrain = func(ctx context.Context) error { return requestCloudDrain(ctx, c.LocalDataPath, identity) }
	}
	closeRuntime := func(ctx context.Context) error {
		e := registry.Quiescent()
		if e != nil {
			e = fmt.Errorf("shutdown writer reconciliation: %w", e)
		}
		flushErr := journal.flush(ctx)
		if flushErr != nil {
			flushErr = fmt.Errorf("shutdown journal flush: %w", flushErr)
		}
		registry.Invalidate()
		clients.transport.CloseIdleConnections()
		return errors.Join(e, flushErr, metadata.closeError(), resident.Close())
	}
	return Dependencies{NodeID: c.RuntimeNodeID, Faults: faultReg, Statistics: measurements, Metadata: metadata, Packs: faultPacks, Cache: resident, ObjectsForRequest: objects, S3ListTokenKey: cursor, Admin: a, Probe: probe, Journal: journal,
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
