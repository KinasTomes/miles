package dynamodb

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/jobs"
	opstest "github.com/KinasTomes/miles/internal/jobs/contracttest"
	"github.com/KinasTomes/miles/internal/recovery"
	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/KinasTomes/miles/internal/storage/contracttest"
	"github.com/aws/aws-sdk-go-v2/aws"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

type recoveryFixture struct {
	m, peer  *Store
	utc      *r1Clock
	local    *clock.Virtual
	objects  recovery.ImmutableStore
	physical *handoffS3
	catalog  recovery.Catalog
	worker   s.WriterFence
}

func newRecoveryFixture(t *testing.T) *recoveryFixture {
	t.Helper()
	ctx := context.Background()
	f := contracttest.NewFixture()
	m, client := fixtureStore(t, f)
	utc := &r1Clock{now: f.Now}
	m.authority = &SharedAuthority{Time: utc, Lifetime: 5 * time.Minute}
	m.now = utc.Now
	m.requireProducers = true
	peer, err := New(Options{API: client, Tables: m.tables, Now: utc.Now, Authority: &SharedAuthority{Time: utc, Lifetime: 5 * time.Minute}, RequireProducers: true})
	if err != nil {
		t.Fatal(err)
	}
	h, inc := strings.Repeat("1", 32), strings.Repeat("2", 32)
	if err = m.InitializeR1(ctx, inc, h, 0); err != nil {
		t.Fatal(err)
	}
	physical := &handoffS3{objects: map[string][]byte{}}
	objects, err := recovery.NewS3Objects(physical, "journal-bucket", "", "kms-fixture")
	if err != nil {
		t.Fatal(err)
	}
	base, _ := recovery.Encode(map[string]any{"r2_version": 1, "fixture": "audited seed; not a production checkpoint"})
	sum := sha256.Sum256(base)
	catalog := recovery.Catalog{Version: 1, History: h, Incarnation: inc, Genesis: recovery.Reference{Length: len(base), SHA256: hex.EncodeToString(sum[:])}}
	if err = recovery.ProvisionHistory(ctx, m, objects, catalog, base); err != nil {
		t.Fatal(err)
	}
	local, _ := clock.NewVirtual(f.Now, 10000)
	out := &recoveryFixture{m: m, peer: peer, utc: utc, local: local, objects: objects, physical: physical, catalog: catalog}
	out.worker = out.acquire(t, "journal-job", "recovery", "", 1)
	return out
}
func (f *recoveryFixture) acquire(t *testing.T, id, kind, op string, generation uint64) s.WriterFence {
	t.Helper()
	w := s.WriterFence{LeaseID: id, ServiceIncarnation: f.catalog.Incarnation, Epoch: generation}
	r := s.R1Acquire{Fence: w, Owner: "worker", Kind: kind, OpID: op, Deadline: f.utc.Now().Add(9 * time.Minute)}
	if kind == "writer" || kind == "reader" {
		r.Object = contracttest.NewFixture().Request("obj", 0).Head.Object
	}
	if _, err := f.m.AcquireR1(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	return w
}
func (f *recoveryFixture) mutation(t *testing.T, label string) (s.ControlMutation, s.WriterFence) {
	t.Helper()
	id := sha256.Sum256([]byte(label))
	op := hex.EncodeToString(id[:16])
	g, err := f.m.GetR1Gate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	w := f.acquire(t, "producer-"+label, "control", op, 1)
	c := s.MutationContext{HistoryID: f.catalog.History, Epoch: g.Epoch, LogicalOpID: op, MutationID: op, Writer: w}
	return s.ControlMutation{Context: c, CredentialID: "admin-key", Entity: "Users", ID: "user-" + label, User: s.User{ID: "user-" + label, Active: true, DefaultDomain: "default"}}, w
}
func (f *recoveryFixture) release(t *testing.T, w s.WriterFence) {
	t.Helper()
	ctx := context.Background()
	if err := f.m.CloseR1(ctx, w); err != nil {
		t.Fatal(err)
	}
	if err := f.m.ReleaseR1(ctx, w); err != nil {
		t.Fatal(err)
	}
}
func (f *recoveryFixture) request(epoch uint64) s.RecoveryCutRequest {
	return s.RecoveryCutRequest{HistoryID: f.catalog.History, Epoch: epoch, CloseID: fmt.Sprintf("%032x", epoch+7), Worker: f.worker}
}
func (f *recoveryFixture) service(m *Store, w s.WriterFence) recovery.Worker {
	return recovery.Worker{Store: m, Payloads: m, Objects: f.objects, Catalog: f.catalog, Clock: f.local, Fence: w}
}
func closeRecovery(t *testing.T, m *Store, r s.RecoveryCutRequest) s.RecoveryEpoch {
	t.Helper()
	e, err := m.BeginRecoveryCut(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; e.State == "Closing"; i++ {
		if i > 10000 {
			t.Fatal("closure did not terminate")
		}
		e, err = m.StepRecoveryCut(context.Background(), r)
		if err != nil {
			t.Fatal(err)
		}
	}
	return e
}
func TestLocalRecoveryCloseSealPaginationAndIndependentChain(t *testing.T) {
	f := newRecoveryFixture(t)
	ctx := context.Background()
	// More than 100 producers and 128 members exceed transaction/leaf caps.
	for i := 0; i < 137; i++ {
		r, w := f.mutation(t, fmt.Sprintf("%03d", i))
		result, err := f.m.MutateControl(ctx, r)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.peer.MutateControl(ctx, r); err != nil {
			t.Fatal("stable retry", err)
		}
		if result.Event.ID == "" {
			t.Fatal("missing event")
		}
		f.release(t, w)
	}
	r := f.request(0)
	closed := closeRecovery(t, f.m, r)
	if closed.State != "Closed" || closed.ProducerCount != 137 || closed.EventCount != 137 || closed.Phase != "complete" {
		t.Fatal(closed)
	}
	if _, err := recovery.VerifyCut(ctx, f.objects, f.catalog, 0); err == nil {
		t.Fatal("Closed was advertised as complete")
	}
	if err := f.service(f.peer, f.worker).Flush(ctx); err != nil {
		t.Fatal(err)
	}
	sealed, err := f.peer.GetRecoveryEpoch(ctx, f.catalog.History, 0)
	if err != nil || sealed.State != "Sealed" {
		t.Fatal(sealed, err)
	}
	first, err := recovery.VerifyCut(ctx, f.objects, f.catalog, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Epoch1 is explicitly empty and still needs a marker chained to epoch0.
	if _, err = f.service(f.m, f.worker).CloseAndSeal(ctx, f.request(1)); err != nil {
		t.Fatal(err)
	}
	last, err := recovery.VerifyCut(ctx, f.objects, f.catalog, 1)
	if err != nil || last.Reference.SHA256 == first.Reference.SHA256 {
		t.Fatal(last, err)
	}
	f.physical.mu.Lock()
	delete(f.physical.objects, "journal/epochs/"+f.catalog.History+"/0/complete.json")
	f.physical.mu.Unlock()
	if _, err = recovery.VerifyCut(ctx, f.objects, f.catalog, 1); err == nil {
		t.Fatal("skipped missing predecessor")
	}
	t.Log("137 native producers/mutations; paged closure, multi-leaf seal, explicit empty successor, DB-independent chain verified")
}
func TestLocalRecoveryCutAdmittedProducerReaderAndAbort(t *testing.T) {
	f := newRecoveryFixture(t)
	ctx := context.Background()
	r, w := f.mutation(t, "healthy")
	cut := f.request(0)
	if _, err := f.m.BeginRecoveryCut(ctx, cut); err != nil {
		t.Fatal(err)
	}
	if err := f.peer.SetR1Gate(ctx, 999, "Open"); !errors.Is(err, s.ErrConflict) {
		t.Fatal("generic gate reopened recovery cut", err)
	}
	// New readers and non-authoritative recovery workers are still admitted.
	reader := f.acquire(t, "reader", "reader", "", 1)
	_ = reader
	if _, err := f.peer.AcquireR1(ctx, s.R1Acquire{Fence: s.WriterFence{LeaseID: "new-producer", ServiceIncarnation: f.catalog.Incarnation, Epoch: 1}, Owner: "b", Kind: "control", Deadline: f.utc.Now().Add(time.Minute)}); !errors.Is(err, s.ErrR1Busy) {
		t.Fatal(err)
	}
	if _, err := f.m.StepRecoveryCut(ctx, cut); !errors.Is(err, s.ErrR1Busy) {
		t.Fatal("healthy authority closed without proof", err)
	}
	if _, err := f.peer.MutateControl(ctx, r); err != nil {
		t.Fatal("admitted producer could not finish", err)
	}
	f.utc.Advance(120 * time.Second)
	if _, err := f.peer.StepRecoveryCut(ctx, cut); !errors.Is(err, s.ErrRecoveryCloseTimeout) {
		t.Fatal("equality did not expire wait budget", err)
	}
	if err := f.peer.AbortRecoveryCut(ctx, cut); err != nil {
		t.Fatal(err)
	}
	g, _ := f.m.GetR1Gate(ctx)
	if g.State != "Open" || g.Epoch != 0 {
		t.Fatal(g)
	}
	if _, err := f.m.CheckR1(ctx, w); err != nil {
		t.Fatal("healthy writer was killed for a cut", err)
	}
	f.release(t, w)
	cut.CloseID = strings.Repeat("c", 32)
	closeRecovery(t, f.m, cut)
	if err := f.m.AbortRecoveryCut(ctx, cut); !errors.Is(err, s.ErrConflict) {
		t.Fatal("reopened Closed", err)
	}
}

type omitOutbox struct{ s.RecoveryStore }

func (o omitOutbox) ListOutbox(ctx context.Context, q s.OutboxQuery) (s.OutboxPage, error) {
	p, e := o.RecoveryStore.ListOutbox(ctx, q)
	if len(p.Events) > 0 {
		p.Events = p.Events[1:]
	}
	return p, e
}
func TestLocalRecoveryMissingMemberPayloadAndStaleExporter(t *testing.T) {
	f := newRecoveryFixture(t)
	ctx := context.Background()
	r, w := f.mutation(t, "missing")
	out, err := f.m.MutateControl(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	f.release(t, w)
	cut := f.request(0)
	closeRecovery(t, f.m, cut)
	// A truncated scan with no cursor must not turn into a complete seal.
	sealer := recovery.Sealer{Exporter: recovery.Exporter{Metadata: f.m, Payloads: f.m, Objects: f.objects, Worker: f.worker}, Store: omitOutbox{f.m}, Catalog: f.catalog}
	if _, err = sealer.Seal(ctx, cut); !errors.Is(err, s.ErrConflict) {
		t.Fatal("missing member sealed", err)
	}
	assertNoMarker := func() {
		t.Helper()
		e, _ := f.m.GetRecoveryEpoch(ctx, f.catalog.History, 0)
		if e.State != "Closed" {
			t.Fatal(e)
		}
		f.physical.mu.Lock()
		defer f.physical.mu.Unlock()
		if _, ok := f.physical.objects["journal/epochs/"+f.catalog.History+"/0/complete.json"]; ok {
			t.Fatal("premature completion")
		}
	}
	assertNoMarker()
	// Native mutation membership independently detects a deleted base event.
	eventAddress, _ := resolve(key("events", out.Event.ID))
	raw := f.m.api.(*ddb.Client)
	if _, err = raw.DeleteItem(ctx, &ddb.DeleteItemInput{TableName: aws.String(f.m.tables["Outbox"]), Key: eventAddress.key.Attributes}); err != nil {
		t.Fatal(err)
	}
	if _, err = f.service(f.m, f.worker).CloseAndSeal(ctx, cut); err == nil {
		t.Fatal("deleted base event sealed")
	}
	assertNoMarker()
	if err = f.m.update(ctx, func(tx *transaction) error { return put(tx, key("events", out.Event.ID), out.Event) }); err != nil {
		t.Fatal(err)
	}
	// Missing retained payload holds Closed, rather than substituting live state.
	payloadKey, _ := resolve(key("payload", out.Event.PayloadPointer))
	saved, err := raw.GetItem(ctx, &ddb.GetItemInput{TableName: aws.String(f.m.tables["SystemConfig"]), Key: payloadKey.key.Attributes, ConsistentRead: aws.Bool(true)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = raw.DeleteItem(ctx, &ddb.DeleteItemInput{TableName: aws.String(f.m.tables["SystemConfig"]), Key: payloadKey.key.Attributes}); err != nil {
		t.Fatal(err)
	}
	if _, err = f.service(f.m, f.worker).CloseAndSeal(ctx, cut); err == nil {
		t.Fatal("missing retained input sealed")
	}
	assertNoMarker()
	if _, err = raw.PutItem(ctx, &ddb.PutItemInput{TableName: aws.String(f.m.tables["SystemConfig"]), Item: saved.Item}); err != nil {
		t.Fatal(err)
	}
	if _, err = f.service(f.m, f.worker).CloseAndSeal(ctx, cut); err != nil {
		t.Fatal(err)
	}
	f.release(t, f.worker)
	if err = (recovery.Exporter{Metadata: f.m, Payloads: f.m, Objects: f.objects, Worker: f.worker}).Export(ctx, out.Event.ID); !errors.Is(err, s.ErrFenced) {
		t.Fatal("stale exporter marked progress", err)
	}
}

type failSealPublish struct {
	s.RecoveryStore
	fail bool
}

func (f *failSealPublish) PublishRecoverySeal(ctx context.Context, r s.RecoveryCutRequest, revision uint64, key, sum string) error {
	if f.fail {
		f.fail = false
		return s.ErrUnavailable
	}
	return f.RecoveryStore.PublishRecoverySeal(ctx, r, revision, key, sum)
}
func TestLocalRecoveryWorkerCrashTakeoverAndMarkerBeforePointer(t *testing.T) {
	f := newRecoveryFixture(t)
	ctx := context.Background()
	r, w := f.mutation(t, "takeover")
	if _, err := f.m.MutateControl(ctx, r); err != nil {
		t.Fatal(err)
	}
	f.release(t, w)
	cut := f.request(0)
	closeRecovery(t, f.m, cut)
	flaky := &failSealPublish{RecoveryStore: f.m, fail: true}
	sealer := recovery.Sealer{Exporter: recovery.Exporter{Metadata: f.m, Payloads: f.m, Objects: f.objects, Worker: f.worker}, Store: flaky, Catalog: f.catalog}
	if _, err := sealer.Seal(ctx, cut); !errors.Is(err, s.ErrUnavailable) {
		t.Fatal(err)
	}
	marker, err := recovery.VerifyCut(ctx, f.objects, f.catalog, 0)
	if err != nil {
		t.Fatal("verified marker was not independently discoverable", err)
	}
	e, _ := f.m.GetRecoveryEpoch(ctx, f.catalog.History, 0)
	if e.State != "Closed" {
		t.Fatal("lost publication changed state", e)
	}
	// New authority cannot overlap the still live worker.
	other := f.acquire(t, "other-job", "recovery", "", 1)
	steal := cut
	steal.Worker = other
	if _, err = f.peer.BeginRecoveryCut(ctx, steal); !errors.Is(err, s.ErrR1Busy) {
		t.Fatal("overlapping workers", err)
	}
	f.utc.Advance(305 * time.Second)
	if err = f.peer.ReclaimR1(ctx, f.worker, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if err = f.peer.ReleaseR1(ctx, f.worker); err != nil {
		t.Fatal(err)
	}
	next := f.acquire(t, f.worker.LeaseID, "recovery", "", 2)
	cut.Worker = next
	if _, err = f.peer.BeginRecoveryCut(ctx, cut); err != nil {
		t.Fatal(err)
	}
	retry, err := f.service(f.peer, next).CloseAndSeal(ctx, cut)
	if err != nil || !reflect.DeepEqual(retry, marker) {
		t.Fatal("takeover changed immutable bytes", retry, marker, err)
	}
	if err = f.m.PublishRecoverySeal(ctx, f.request(0), e.Revision, marker.Key, marker.Reference.SHA256); !errors.Is(err, s.ErrFenced) {
		t.Fatal("stale worker published", err)
	}
}

func TestLocalRecoveryResumesPartialProofAndRetainsReusedSlots(t *testing.T) {
	f := newRecoveryFixture(t)
	ctx := context.Background()
	var first s.WriterFence
	for i := 0; i < 31; i++ {
		r, w := f.mutation(t, fmt.Sprintf("partial-%02d", i))
		if i == 0 {
			first = w
		}
		if _, err := f.m.MutateControl(ctx, r); err != nil {
			t.Fatal(err)
		}
		f.release(t, w)
	}
	op := strings.Repeat("a", 32)
	reused := f.acquire(t, first.LeaseID, "control", op, 2)
	if _, err := f.m.MutateControl(ctx, s.ControlMutation{Context: s.MutationContext{HistoryID: f.catalog.History, LogicalOpID: op, MutationID: op, Writer: reused}, CredentialID: "admin-key", Entity: "Users", ID: "reused-slot", User: s.User{ID: "reused-slot", DefaultDomain: "default", Active: true}}); err != nil {
		t.Fatal(err)
	}
	f.release(t, reused)
	cut := f.request(0)
	if _, err := f.m.BeginRecoveryCut(ctx, cut); err != nil {
		t.Fatal(err)
	}
	before, err := f.m.StepRecoveryCut(ctx, cut)
	if err != nil || before.Cursor == "" || before.ProducerCount != 24 {
		t.Fatal(before, err)
	}
	f.release(t, f.worker)
	next := f.acquire(t, f.worker.LeaseID, "recovery", "", 2)
	cut.Worker = next
	resumed, err := f.peer.BeginRecoveryCut(ctx, cut)
	if err != nil || resumed.Cursor != before.Cursor || resumed.ProofSHA256 != before.ProofSHA256 {
		t.Fatal("lost durable progress", resumed, err)
	}
	closed := closeRecovery(t, f.peer, cut)
	if closed.ProducerCount != 32 || closed.EventCount != 32 {
		t.Fatal("reused slot erased an admitted generation", closed)
	}
	if _, err = f.service(f.peer, next).CloseAndSeal(ctx, cut); err != nil {
		t.Fatal(err)
	}
}

type loseClosedACK struct {
	API
	table string
	lost  bool
}

func (a *loseClosedACK) TransactWriteItems(ctx context.Context, in *ddb.TransactWriteItemsInput, options ...func(*ddb.Options)) (*ddb.TransactWriteItemsOutput, error) {
	out, err := a.API.TransactWriteItems(ctx, in, options...)
	if err != nil {
		return out, err
	}
	for _, action := range in.TransactItems {
		if action.Put != nil && aws.ToString(action.Put.TableName) == a.table {
			e, err := decodeItem[s.RecoveryEpoch](action.Put.Item)
			if err == nil && e.State == "Closed" && !a.lost {
				a.lost = true
				return nil, errors.New("lost final closure ACK")
			}
		}
	}
	return out, nil
}
func TestLocalRecoveryUnknownClosedCommitCannotReopen(t *testing.T) {
	f := newRecoveryFixture(t)
	ctx := context.Background()
	cut := f.request(0)
	if _, err := f.m.BeginRecoveryCut(ctx, cut); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.StepRecoveryCut(ctx, cut); err != nil {
		t.Fatal(err)
	}
	original := f.m.api
	flaky := &loseClosedACK{API: original, table: f.m.tables["RecoveryEpochs"]}
	f.m.api = flaky
	if _, err := f.m.StepRecoveryCut(ctx, cut); !errors.Is(err, s.ErrUnavailable) {
		t.Fatal(err)
	}
	f.m.api = original
	if !flaky.lost {
		t.Fatal("fault seam did not fire")
	}
	gate, _ := f.peer.GetR1Gate(ctx)
	epoch, _ := f.peer.GetRecoveryEpoch(ctx, f.catalog.History, 0)
	if gate.Epoch != 1 || gate.State != "Open" || epoch.State != "Closed" {
		t.Fatal("non-atomic Closed/current tuple", gate, epoch)
	}
	if err := f.peer.AbortRecoveryCut(ctx, cut); !errors.Is(err, s.ErrConflict) {
		t.Fatal("unknown commit reopened E", err)
	}
	if _, err := f.service(f.peer, f.worker).CloseAndSeal(ctx, cut); err != nil {
		t.Fatal(err)
	}
}

type advancingRecoveryClock struct{ *clock.Virtual }

func (c advancingRecoveryClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	wall := c.Now().WallUTC
	if err := c.Advance(d); err != nil {
		return err
	}
	return c.SetWall(wall.Add(-time.Hour))
}
func TestLocalRecoveryWorkerMonotonicBudgetSurvivesWallRollback(t *testing.T) {
	f := newRecoveryFixture(t)
	ctx := context.Background()
	_, healthy := f.mutation(t, "waiting")
	worker := f.service(f.m, f.worker)
	worker.Clock = advancingRecoveryClock{f.local}
	if _, err := worker.CloseAndSeal(ctx, f.request(0)); !errors.Is(err, s.ErrRecoveryCloseTimeout) {
		t.Fatal(err)
	}
	gate, _ := f.peer.GetR1Gate(ctx)
	if gate.State != "Open" || gate.Epoch != 0 {
		t.Fatal("timeout did not release admission", gate)
	}
	if _, err := f.peer.CheckR1(ctx, healthy); err != nil {
		t.Fatal("timeout killed a healthy producer", err)
	}
}

func TestLocalRecoveryAutomaticallyClosesExpiredJobBeforeTransfer(t *testing.T) {
	f := newRecoveryFixture(t)
	ctx := context.Background()
	cut := f.request(0)
	if _, err := f.m.BeginRecoveryCut(ctx, cut); err != nil {
		t.Fatal(err)
	}
	if err := f.m.BeginR1Action(ctx, f.worker, "unauthorized-maintenance"); !errors.Is(err, s.ErrFenced) {
		t.Fatal("recovery lease gained physical action authority", err)
	}
	f.utc.Advance(305 * time.Second)
	next := f.acquire(t, "replacement-job", "recovery", "", 1)
	cut.Worker = next
	if _, err := f.service(f.peer, next).CloseAndSeal(ctx, cut); !errors.Is(err, s.ErrRecoveryCloseTimeout) {
		t.Fatal("takeover ignored persisted close budget", err)
	}
	old, err := f.m.GetR1Lease(ctx, f.worker.LeaseID)
	if err != nil || old.State != "Released" {
		t.Fatal("old worker authority/protection leaked", old, err)
	}
	gate, _ := f.m.GetR1Gate(ctx)
	if gate.State != "Open" || gate.Epoch != 0 {
		t.Fatal(gate)
	}
	cut.CloseID = strings.Repeat("b", 32)
	if _, err = f.service(f.peer, next).CloseAndSeal(ctx, cut); err != nil {
		t.Fatal(err)
	}
}

func TestLocalRecoveryWorkerCadenceHeartbeatAndJoinedCancellation(t *testing.T) {
	f := newRecoveryFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	exports := make(chan struct{}, 4)
	worker := f.service(f.m, f.worker)
	worker.Observe = func(code string) {
		if code == "export_ok" {
			exports <- struct{}{}
		}
	}
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	select {
	case <-exports:
	case err := <-done:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("initial export did not run")
	}
	for i := 0; i < 4; i++ {
		deadline := time.Now().Add(5 * time.Second)
		for f.local.Pending() < 2 {
			if time.Now().After(deadline) {
				t.Fatal("worker did not register export and heartbeat timers")
			}
			runtime.Gosched()
		}
		f.utc.Advance(15 * time.Second)
		if err := f.local.Advance(15 * time.Second); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-exports:
	case err := <-done:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("60s export did not run")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("worker/heartbeat failed to join")
	}
	lease, err := f.m.GetR1Lease(context.Background(), f.worker.LeaseID)
	if err != nil || lease.Revision <= 1 || lease.State != "Active" {
		t.Fatal("renewal failed or worker released its caller's lease", lease, err)
	}
}

func TestLocalRecoveryLeaseCanRunJournalJobButCannotRunGC(t *testing.T) {
	f := newRecoveryFixture(t)
	ctx := context.Background()
	policy := opstest.Policy(1)
	if err := f.m.update(ctx, func(tx *transaction) error {
		if err := put(tx, policyKey(1), policy); err != nil {
			return err
		}
		return put(tx, "policy-current", uint64(1))
	}); err != nil {
		t.Fatal(err)
	}
	node := opstest.Node("node-a")
	node.ServiceIncarnation = f.catalog.Incarnation
	if _, err := f.m.PutNode(ctx, 0, node); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []jobs.Type{"journal_flush", "gc"} {
		j, err := (jobs.Service{Store: f.m, Policies: f.m, Now: f.utc.Now}).Create(ctx, jobs.Create{Type: kind})
		if err != nil {
			t.Fatal(err)
		}
		fence := f.acquire(t, j.ID, "recovery", "", 1)
		_, err = f.m.ChangeJob(ctx, jobs.Change{JobID: j.ID, ExpectedRevision: j.Revision, Kind: "claim", Fence: fence, NodeID: node.ID, NodeIncarnation: node.Incarnation})
		if kind == "journal_flush" && err != nil {
			t.Fatal(err)
		}
		if kind == "gc" && !errors.Is(err, s.ErrFenced) {
			t.Fatal("journal authority used for destructive job", err)
		}
	}
}

func TestLocalRecoveryEnablementBlocksLegacyPeerBypass(t *testing.T) {
	f := newRecoveryFixture(t)
	ctx := context.Background()
	legacy, err := New(Options{API: f.m.api, Tables: f.m.tables, Now: f.utc.Now, Authority: f.m.authority})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = legacy.CreateBucket(ctx, s.BucketCreate{CredentialID: "owner-key", UserID: "owner", Name: "legacy-untracked"}); !errors.Is(err, s.ErrFenced) {
		t.Fatal("legacy peer bypassed shared strict enablement", err)
	}
	if _, err = f.m.GetRecoveryEpoch(ctx, "bad-history", 0); !errors.Is(err, s.ErrInvalid) {
		t.Fatal(err)
	}
}
