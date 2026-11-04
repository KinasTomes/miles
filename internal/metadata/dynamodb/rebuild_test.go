package dynamodb

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/jobs"
	"github.com/KinasTomes/miles/internal/recovery"
	"github.com/KinasTomes/miles/internal/storage/fake"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/KinasTomes/miles/internal/storage/contracttest"
	"github.com/aws/aws-sdk-go-v2/aws"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

type testOfflineProof struct {
	denied bool
	calls  int
}

func (p *testOfflineProof) Check(ctx context.Context, r s.IndexInstall, _ Observe) error {
	p.calls++
	if p.denied || ctx.Err() != nil || r.SourceHistory == r.History {
		return s.ErrFenced
	}
	return nil
}
func emptyRecoveryStore(t *testing.T) *Store {
	t.Helper()
	m, client := fixtureStore(t, contracttest.NewFixture())
	// Only isolated test-generated table names are cleared; no deployment table
	// or user dataset is used by this fixture.
	for logical, physical := range m.tables {
		out, e := client.Scan(t.Context(), &ddb.ScanInput{TableName: aws.String(physical), ConsistentRead: aws.Bool(true)})
		if e != nil {
			t.Fatal(e)
		}
		if len(out.LastEvaluatedKey) != 0 {
			t.Fatal("unexpected fixture size")
		}
		for _, item := range out.Items {
			key := Item{}
			spec := Tables()[logical]
			key[spec.Partition] = item[spec.Partition]
			if spec.Sort != "" {
				key[spec.Sort] = item[spec.Sort]
			}
			if _, e = client.DeleteItem(t.Context(), &ddb.DeleteItemInput{TableName: aws.String(physical), Key: key}); e != nil {
				t.Fatal(e)
			}
		}
	}
	return m
}
func installFixture(t *testing.T) (s.IndexInstall, []s.IndexRecord) {
	t.Helper()
	records := []s.IndexRecord{}
	add := func(kind string, ids []string, v any) {
		b, e := json.Marshal(v)
		if e != nil {
			t.Fatal(e)
		}
		records = append(records, s.IndexRecord{Kind: kind, IDs: ids, Body: b})
	}
	add("users", []string{"owner"}, s.User{ID: "owner", Revision: 2, Active: true, DefaultDomain: "default"})
	add("credentials", []string{"old-key"}, s.Credential{ID: "old-key", UserID: "owner", Revision: 1, Active: false})
	add("admin-tokens", []string{"old-token"}, s.AdminToken{ID: "old-token", UserID: "owner", Scope: s.TokenScopeSystem, Hash: make([]byte, 32), Revision: 1, Active: false})
	add("quotas", []string{"owner"}, s.LogicalQuota{OwnerID: "owner", Limit: 100, Used: 4})
	add("buckets", []string{"bucket"}, s.Bucket{Name: "bucket", ID: strings.Repeat("4", 32), OwnerID: "owner", Domain: "default", Revision: 1, Active: true})
	add("bucket-namespace", []string{"bucket"}, s.BucketNamespace{Version: 1, LiveObjects: 1})
	validator, _ := config.NewPolicyValidator(config.DSGPolicyLimits())
	snapshot, e := validator.NewGlobalSnapshot(1, config.DefaultGlobalPolicy())
	if e != nil {
		t.Fatal(e)
	}
	policy, e := snapshot.StorageSnapshot()
	if e != nil {
		t.Fatal(e)
	}
	add("policies", []string{"0000000000000001"}, policy)
	add("policy-current", nil, uint64(1))
	sort.Slice(records, func(i, j int) bool {
		return s.IndexRecordKey(records[i].Kind, records[i].IDs) < s.IndexRecordKey(records[j].Kind, records[j].IDs)
	})
	chain := ""
	for _, r := range records {
		var e error
		chain, e = s.IndexRecordDigest(chain, r)
		if e != nil {
			t.Fatal(e)
		}
	}
	return s.IndexInstall{SourceHistory: strings.Repeat("1", 32), SourceIncarnation: strings.Repeat("2", 32), TargetSHA256: strings.Repeat("a", 64), History: strings.Repeat("3", 32), Incarnation: strings.Repeat("4", 32), GenesisSHA256: strings.Repeat("b", 64), GenesisLength: 100, RecordsSHA256: chain}, records
}
func TestLocalRebuiltIndexOfflineProofResumeAndDisabledCredentials(t *testing.T) {
	m := emptyRecoveryStore(t)
	request, records := installFixture(t)
	if e := m.BeginIndexInstall(t.Context(), request); !errors.Is(e, s.ErrFenced) {
		t.Fatal(e)
	}
	proof := &testOfflineProof{}
	m.offlineRecovery = proof
	if e := m.BeginIndexInstall(t.Context(), request); e != nil {
		t.Fatal(e)
	}
	g, e := m.GetR1Gate(t.Context())
	if e != nil || g.State != "Closing" || g.Incarnation != request.Incarnation {
		t.Fatal(g, e)
	}
	if _, e = m.AcquireR1(t.Context(), s.R1Acquire{Fence: s.WriterFence{LeaseID: "new-worker", ServiceIncarnation: request.Incarnation, Epoch: 1}, Kind: "writer", Owner: "node", Deadline: m.now().Add(time.Minute)}); e == nil {
		t.Fatal("admitted during partial install")
	}
	for i, r := range records {
		if e = m.StageIndexRecord(t.Context(), request, r); e != nil {
			t.Fatal(r.Kind, e)
		}
		if i == 2 {
			if e = m.BeginIndexInstall(t.Context(), request); e != nil {
				t.Fatal(e)
			}
			for _, prior := range records[:i+1] {
				if e = m.StageIndexRecord(t.Context(), request, prior); e != nil {
					t.Fatal("restart", e)
				}
			}
		}
	}
	proof.denied = true
	if e = m.FinishIndexInstall(s.WithIndexReadiness(t.Context(), request, time.Now().Add(time.Hour), func(context.Context) error { return nil }), request); !errors.Is(e, s.ErrFenced) {
		t.Fatal(e)
	}
	g, _ = m.GetR1Gate(t.Context())
	if g.State != "Closing" {
		t.Fatal("opened without isolation proof")
	}
	proof.denied = false
	if e = m.FinishIndexInstall(s.WithIndexReadiness(t.Context(), request, time.Now().Add(time.Hour), func(context.Context) error { return nil }), request); e != nil {
		t.Fatal(e)
	}
	if e = m.FinishIndexInstall(s.WithIndexReadiness(t.Context(), request, time.Now().Add(time.Hour), func(context.Context) error { return nil }), request); e != nil {
		t.Fatal("lost final ACK", e)
	}
	for _, r := range records {
		if e = m.StageIndexRecord(t.Context(), request, r); e != nil {
			t.Fatal("retry completed install", e)
		}
	}
	g, e = m.GetR1Gate(t.Context())
	if e != nil || g.State != "Open" || !g.RecoveryEnabled {
		t.Fatal(g, e)
	}
	c, e := m.GetCredential(t.Context(), "old-key")
	if e != nil || c.Active {
		t.Fatal(c, e)
	}
	token, e := m.GetAdminToken(t.Context(), "old-token")
	if e != nil || token.Active {
		t.Fatal(token, e)
	}
	q, e := m.GetQuota(t.Context(), "owner")
	if e != nil || q.Used != 4 || q.Reserved != 0 || q.Staged != 0 || q.ActiveUploads != 0 {
		t.Fatal(q, e)
	}
	state, e := m.GetRecoveryState(t.Context(), request.History)
	if e != nil || state.Incarnation != request.Incarnation || state.NextToSeal != 0 {
		t.Fatal(state, e)
	}
	changed := request
	changed.Incarnation = strings.Repeat("5", 32)
	if e = m.BeginIndexInstall(t.Context(), changed); !errors.Is(e, s.ErrConflict) {
		t.Fatal(e)
	}
	if proof.calls < 3 {
		t.Fatal("missing final proof")
	}
	if e = m.update(t.Context(), func(tx *transaction) error {
		g, e := r1Read[s.R1Gate](tx, r1GateKey())
		if e != nil {
			return e
		}
		g.Incarnation = strings.Repeat("9", 32)
		g.Revision++
		return r1Put(tx, r1GateKey(), g)
	}); e != nil {
		t.Fatal(e)
	}
	if e = m.FinishIndexInstall(s.WithIndexReadiness(t.Context(), request, time.Now().Add(time.Hour), func(context.Context) error { return nil }), request); !errors.Is(e, s.ErrFenced) {
		t.Fatal("stale install reported ready", e)
	}
}
func TestLocalRebuiltIndexRejectsPartialWrongDigestAndOldAuthority(t *testing.T) {
	m := emptyRecoveryStore(t)
	request, records := installFixture(t)
	m.offlineRecovery = &testOfflineProof{}
	if e := m.BeginIndexInstall(t.Context(), request); e != nil {
		t.Fatal(e)
	}
	if e := m.StageIndexRecord(t.Context(), request, records[0]); e != nil {
		t.Fatal(e)
	}
	if e := m.FinishIndexInstall(s.WithIndexReadiness(t.Context(), request, time.Now().Add(time.Hour), func(context.Context) error { return nil }), request); !errors.Is(e, s.ErrConflict) {
		t.Fatal(e)
	}
	bad := records[1]
	bad.Kind = "leases"
	if e := m.StageIndexRecord(t.Context(), request, bad); e == nil {
		t.Fatal("installed old authority")
	}
	bad = records[0]
	var v s.AdminToken
	if json.Unmarshal(bad.Body, &v) != nil {
		t.Fatal("decode")
	}
	v.Active = true
	bad.Body, _ = json.Marshal(v)
	if e := m.StageIndexRecord(t.Context(), request, bad); e == nil {
		t.Fatal("rewrote installed token")
	}
}
func TestLocalRecoveryCatalogCASAndStaleWorker(t *testing.T) {
	m, _, f, clock := sharedFixture(t)
	w := s.WriterFence{LeaseID: "checkpoint", ServiceIncarnation: "shared-service", Epoch: 1}
	if _, e := m.AcquireR1(t.Context(), s.R1Acquire{Fence: w, Kind: "recovery", Owner: "node", Deadline: f.Now.Add(time.Minute)}); e != nil {
		t.Fatal(e)
	}
	next := s.RecoveryCatalogHead{Version: 1, History: strings.Repeat("1", 32), Incarnation: w.ServiceIncarnation, Revision: 1, Key: "support/one", SHA256: strings.Repeat("a", 64), Length: 100}
	if e := m.CompareRecoveryCatalog(t.Context(), s.RecoveryCatalogHead{}, next, w); e != nil {
		t.Fatal(e)
	}
	if e := m.CompareRecoveryCatalog(t.Context(), s.RecoveryCatalogHead{}, next, w); e != nil {
		t.Fatal("lost ACK", e)
	}
	different := next
	different.SHA256 = strings.Repeat("b", 64)
	if e := m.CompareRecoveryCatalog(t.Context(), s.RecoveryCatalogHead{}, different, w); !errors.Is(e, s.ErrConflict) {
		t.Fatal(e)
	}
	clock.Advance(time.Minute)
	later := next
	later.Revision++
	later.Key = "support/two"
	if e := m.CompareRecoveryCatalog(t.Context(), next, later, w); e == nil {
		t.Fatal("stale catalog publication")
	}
	got, e := m.GetRecoveryCatalog(t.Context(), next.History)
	if e != nil || !reflect.DeepEqual(got, next) {
		t.Fatal(got, e)
	}
}

func TestLocalCheckpointProducerToOfflineNewHistory(t *testing.T) {
	f := newRecoveryFixtureBase(t, true)
	r, producer := f.mutation(t, "owner-change")
	r.ID = "owner"
	r.ExpectedRevision = 1
	r.User = s.User{ID: "owner", Revision: 1, Active: true, DefaultDomain: "recovered"}
	r.SetQuota = true
	r.QuotaLimit = 32
	if _, e := f.m.MutateControl(t.Context(), r); e != nil {
		t.Fatal(e)
	}
	f.release(t, producer)
	if _, e := f.service(f.m, f.worker).CloseAndSeal(t.Context(), f.request(0)); e != nil {
		t.Fatal(e)
	}
	physical := fake.NewPacks(f.utc.Now)
	worker := recovery.CheckpointWorker{Store: f.m, Objects: f.objects, Packs: physical, Catalog: f.catalog}
	j := jobs.Job{ID: "checkpoint-native", Worker: f.worker, Params: json.RawMessage(`{"target_epoch":0}`)}
	result, e := worker.Step(t.Context(), j, config.GlobalSnapshot{})
	if e != nil || !result.Done || !result.Settled {
		t.Fatal(result, e)
	}
	// A restart after a lost job acknowledgement consumes the same checkpoint.
	again, e := worker.Step(t.Context(), j, config.GlobalSnapshot{})
	if e != nil || !reflect.DeepEqual(result.Result, again.Result) {
		t.Fatal(again, e)
	}
	target := emptyRecoveryStore(t)
	proof := &testOfflineProof{}
	target.offlineRecovery = proof
	retention := recovery.Retention{Checkpointer: recovery.Checkpointer{Objects: f.objects, Catalog: f.catalog}, Packs: physical}
	// No source metadata store is supplied: immutable completion slots suffice.
	now := f.utc.Now()
	request := recovery.RecoverRequest{Target: now, Bounds: s.R1Bounds{Lower: now, Upper: now, Horizon: now.Add(time.Hour)}, History: strings.Repeat("7", 32), Incarnation: strings.Repeat("8", 32)}
	catalog, e := (recovery.Recoverer{Retention: retention, Installer: target}).Recover(t.Context(), request)
	if e != nil {
		t.Fatal(e)
	}
	user, e := read[s.User](target, t.Context(), key("users", "owner"))
	if e != nil || user.Revision != 2 || user.DefaultDomain != "recovered" {
		t.Fatal(user, e)
	}
	q, e := target.GetQuota(t.Context(), "owner")
	if e != nil || q.Limit != 32 || q.Used != 0 || q.Reserved != 0 || q.ActiveUploads != 0 {
		t.Fatal(q, e)
	}
	old, e := target.GetCredential(t.Context(), "admin-key")
	if e != nil || old.Active {
		t.Fatal(old, e)
	}
	gate, e := target.GetR1Gate(t.Context())
	if e != nil || gate.HistoryID != catalog.History || gate.Incarnation != catalog.Incarnation || gate.State != "Open" {
		t.Fatal(gate, e)
	}
	if _, e = target.AcquireR1(t.Context(), s.R1Acquire{Fence: f.worker, Kind: "recovery", Owner: "old-node", Deadline: now.Add(time.Minute)}); e == nil {
		t.Fatal("old worker entered recovered history")
	}
	selected, e := recovery.LoadSelectedCatalog(t.Context(), f.objects, catalog.History, catalog.Incarnation)
	if e != nil || selected != catalog {
		t.Fatal(selected, e)
	}
}

func TestLocalRebuiltIndexCanonicalManifestAndReceiptProjections(t *testing.T) {
	m := emptyRecoveryStore(t)
	m.offlineRecovery = &testOfflineProof{}
	request, records := installFixture(t)
	f := contracttest.NewFixture()
	add := func(kind string, ids []string, v any) {
		b, e := json.Marshal(v)
		if e != nil {
			t.Fatal(e)
		}
		records = append(records, s.IndexRecord{Kind: kind, IDs: ids, Body: b})
	}
	head := f.Request("rebuilt", 0).Head
	head.Object = s.ObjectKey{Bucket: "bucket", BucketID: strings.Repeat("4", 32), Key: "key"}
	head.Revision = 1
	head.PageCount = 1
	head.PageIndex = []uint64{0}
	head.ChunkCount = 1
	chunk := f.Chunk("default")
	p := f.Packs[0]
	p.StateRevision = 1
	page := s.ManifestPage{Version: 1, ManifestID: head.ManifestID, Domain: head.Domain, Chunks: []s.ChunkRef{{Key: chunk.Key, RawBytes: 4}}}
	add("heads", []string{head.Object.Bucket, head.Object.BucketID, head.Object.Key}, head)
	add("manifest-page", []string{page.ManifestID, "0"}, page)
	add("chunks", []string{chunk.Key.Domain, chunk.Key.Hash}, chunk)
	add("packs", []string{p.Ref.ID, p.Ref.Generation}, p)
	receipt := s.CompletionReceipt{Version: 1, UploadID: "upload", Object: head.Object, OpID: head.OpID, Initiator: "owner", InputDigest: strings.Repeat("a", 64), Head: head, CommittedAt: f.Now, ExpiresAt: f.Now.Add(7 * 24 * time.Hour)}
	add("full-receipt", []string{"upload"}, receipt)
	add("receipts", []string{"upload"}, s.CompletedReceipt{Version: 1, UploadID: "upload", BucketID: head.Object.BucketID, Key: head.Object.Key, OpID: head.OpID, Initiator: "owner", InputDigest: receipt.InputDigest, ManifestID: head.ManifestID, ETag: head.ETag, ExpiresAt: receipt.ExpiresAt})
	add("upload-record", []string{"upload"}, s.MultipartUpload{Version: 1, ID: "upload", Object: head.Object, State: "Completed", Revision: 3})
	add("uploads", []string{"upload"}, s.Completion{UploadID: "upload", BucketID: head.Object.BucketID, Key: head.Object.Key, State: "Completed", Revision: 3})
	sort.Slice(records, func(i, j int) bool {
		return s.IndexRecordKey(records[i].Kind, records[i].IDs) < s.IndexRecordKey(records[j].Kind, records[j].IDs)
	})
	chain := ""
	for _, r := range records {
		var e error
		chain, e = s.IndexRecordDigest(chain, r)
		if e != nil {
			t.Fatal(e)
		}
	}
	request.RecordsSHA256 = chain
	if e := m.BeginIndexInstall(t.Context(), request); e != nil {
		t.Fatal(e)
	}
	for _, r := range records {
		if e := m.StageIndexRecord(t.Context(), request, r); e != nil {
			t.Fatal(r.Kind, e)
		}
	}
	if e := m.FinishIndexInstall(s.WithIndexReadiness(t.Context(), request, time.Now().Add(time.Hour), func(context.Context) error { return nil }), request); e != nil {
		t.Fatal(e)
	}
	actual, e := m.GetObject(t.Context(), head.Object)
	if e != nil || !reflect.DeepEqual(actual, head) {
		t.Fatal(actual, e)
	}
	manifest, e := m.GetManifestPage(t.Context(), page.ManifestID, 0)
	if e != nil || !reflect.DeepEqual(manifest, page) {
		t.Fatal(manifest, e)
	}
	chunks, e := m.GetChunks(t.Context(), []s.ChunkKey{chunk.Key}, true)
	if e != nil || chunks[chunk.Key] != chunk {
		t.Fatal(chunks, e)
	}
	legacy, e := m.GetCompletionReceipt(t.Context(), "upload")
	if e != nil || !legacy.ExpiresAt.Equal(receipt.ExpiresAt) {
		t.Fatal(legacy, e)
	}
	full, e := read[s.CompletionReceipt](m, t.Context(), key("full-receipt", "upload"))
	if e != nil || !reflect.DeepEqual(full, receipt) {
		t.Fatal(full, e)
	}
	u, e := m.GetUpload(t.Context(), "upload")
	if e != nil || u.State != "Completed" || u.Writer != (s.WriterFence{}) {
		t.Fatal(u, e)
	}
}

// A final proof can stall after the library checked readiness. Native dispatch
// must enforce the same bound after that proof and all metadata reads.
type slowFinishProof struct {
	calls int
	delay time.Duration
}

type finalInstallAPI struct {
	API
	policyDelay time.Duration
	sends       int
	noRetry     bool
	deadline    time.Time
}

func (a *finalInstallAPI) GetItem(ctx context.Context, in *ddb.GetItemInput, opts ...func(*ddb.Options)) (*ddb.GetItemOutput, error) {
	if id, ok := in.Key["config_id"].(*types.AttributeValueMemberS); ok && id.Value == "global-policy" && a.policyDelay > 0 {
		time.Sleep(a.policyDelay)
	}
	return a.API.GetItem(ctx, in, opts...)
}
func (a *finalInstallAPI) TransactWriteItems(ctx context.Context, in *ddb.TransactWriteItemsInput, opts ...func(*ddb.Options)) (*ddb.TransactWriteItemsOutput, error) {
	for _, item := range in.TransactItems {
		if item.Put != nil {
			if id, ok := item.Put.Item["config_id"].(*types.AttributeValueMemberS); ok && id.Value == "coordination/r1/current" {
				if state, ok := item.Put.Item["state"].(*types.AttributeValueMemberS); ok && state.Value == "Open" {
					a.sends++
					a.deadline, _ = ctx.Deadline()
					options := ddb.Options{}
					for _, f := range opts {
						f(&options)
					}
					_, a.noRetry = options.Retryer.(aws.NopRetryer)
				}
			}
		}
	}
	return a.API.TransactWriteItems(ctx, in, opts...)
}

func (p *slowFinishProof) Check(ctx context.Context, r s.IndexInstall, o Observe) error {
	p.calls++
	if p.calls > 1 {
		time.Sleep(p.delay)
	}
	return nil
}
func TestLocalInstallFinalDispatchRejectsExpiredReadiness(t *testing.T) {
	for _, failure := range []error{s.ErrFenced, s.ErrCold} {
		t.Run(failure.Error(), func(t *testing.T) {
			m := emptyRecoveryStore(t)
			proof := &slowFinishProof{delay: 100 * time.Millisecond}
			m.offlineRecovery = proof
			request, records := installFixture(t)
			if e := m.BeginIndexInstall(t.Context(), request); e != nil {
				t.Fatal(e)
			}
			for _, record := range records {
				if e := m.StageIndexRecord(t.Context(), request, record); e != nil {
					t.Fatal(e)
				}
			}
			deadline := time.Now().Add(40 * time.Millisecond)
			api := &finalInstallAPI{API: m.api}
			m.api = api
			validUntil := time.Now().Add(time.Hour)
			if failure == s.ErrFenced {
				validUntil = deadline
			}
			ctx := s.WithIndexReadiness(t.Context(), request, validUntil, func(context.Context) error {
				if failure == s.ErrCold && !time.Now().Before(deadline) {
					return failure
				}
				return nil
			})
			if e := m.FinishIndexInstall(ctx, request); !errors.Is(e, failure) {
				t.Fatal("expired readiness opened gate", e)
			}
			if api.sends != 0 {
				t.Fatal("expired proof reached SDK", api.sends)
			}
			gate, e := m.GetR1Gate(t.Context())
			if e != nil || gate.State != "Closing" {
				t.Fatal(gate, e)
			}
			// A newly checked proof can resume the identical staged plan.
			freshDeadline := time.Now().Add(time.Hour)
			ctx = s.WithIndexReadiness(t.Context(), request, freshDeadline, func(context.Context) error { return nil })
			if e = m.FinishIndexInstall(ctx, request); e != nil {
				t.Fatal(e)
			}
			if api.sends != 1 || !api.noRetry || !api.deadline.Equal(freshDeadline) {
				t.Fatal("final send permits hidden retries", api.sends, api.noRetry)
			}
		})
	}
}

func TestLocalInstallFinalDispatchChecksAfterPolicyIO(t *testing.T) {
	m := emptyRecoveryStore(t)
	m.offlineRecovery = &testOfflineProof{}
	request, records := installFixture(t)
	if e := m.BeginIndexInstall(t.Context(), request); e != nil {
		t.Fatal(e)
	}
	for _, r := range records {
		if e := m.StageIndexRecord(t.Context(), request, r); e != nil {
			t.Fatal(e)
		}
	}
	api := &finalInstallAPI{API: m.api, policyDelay: 100 * time.Millisecond}
	m.api = api
	deadline := time.Now().Add(40 * time.Millisecond)
	ctx := s.WithIndexReadiness(t.Context(), request, time.Now().Add(time.Hour), func(context.Context) error {
		if !time.Now().Before(deadline) {
			return s.ErrFenced
		}
		return nil
	})
	if e := m.FinishIndexInstall(ctx, request); !errors.Is(e, s.ErrFenced) {
		t.Fatal(e)
	}
	gate, e := m.GetR1Gate(t.Context())
	if e != nil || gate.State != "Closing" || api.sends != 0 {
		t.Fatal(gate, e, api.sends)
	}
}
func TestLocalInstallFinalDispatchRequiresBoundProof(t *testing.T) {
	m := emptyRecoveryStore(t)
	m.offlineRecovery = &testOfflineProof{}
	request, records := installFixture(t)
	if e := m.BeginIndexInstall(t.Context(), request); e != nil {
		t.Fatal(e)
	}
	for _, r := range records {
		if e := m.StageIndexRecord(t.Context(), request, r); e != nil {
			t.Fatal(e)
		}
	}
	if e := m.FinishIndexInstall(t.Context(), request); !errors.Is(e, s.ErrFenced) {
		t.Fatal(e)
	}
	wrong := request
	wrong.TargetSHA256 = strings.Repeat("c", 64)
	ctx := s.WithIndexReadiness(t.Context(), wrong, time.Now().Add(time.Hour), func(context.Context) error { return nil })
	if e := m.FinishIndexInstall(ctx, request); !errors.Is(e, s.ErrFenced) {
		t.Fatal(e)
	}
	gate, e := m.GetR1Gate(t.Context())
	if e != nil || gate.State != "Closing" {
		t.Fatal(gate, e)
	}
}
