package dynamodb

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/coordination"
	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/KinasTomes/miles/internal/storage/contracttest"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

// A modeled trusted clock oracle for native transaction tests. No claim about
// host UTC/NTP bounds is made; the real adapter still goes through policy validation.
type r1Clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *r1Clock) Bounds(context.Context) (s.R1Bounds, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return s.R1Bounds{Lower: c.now, Upper: c.now, Horizon: c.now.Add(10 * time.Minute)}, nil
}
func (c *r1Clock) Now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *r1Clock) Advance(d time.Duration) { c.mu.Lock(); defer c.mu.Unlock(); c.now = c.now.Add(d) }
func sharedFixture(t *testing.T) (*Store, *Store, contracttest.Fixture, *r1Clock) {
	t.Helper()
	f := contracttest.NewFixture()
	for i := range f.Packs {
		f.Packs[i].StateRevision = 1
	}
	m, client := fixtureStore(t, f)
	c := &r1Clock{now: f.Now}
	a := &SharedAuthority{Time: c, Lifetime: 30 * time.Second}
	m.authority = a
	m.now = c.Now
	peer, err := New(Options{API: client, Tables: m.tables, Now: c.Now, Authority: &SharedAuthority{Time: c, Lifetime: 30 * time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	if err = m.InitializeR1(context.Background(), "shared-service", strings.Repeat("1", 32), 0); err != nil {
		t.Fatal(err)
	}
	return m, peer, f, c
}
func r1Request(f contracttest.Fixture, id string, epoch uint64) s.R1Acquire {
	return s.R1Acquire{Fence: s.WriterFence{LeaseID: id, Epoch: epoch, ServiceIncarnation: "shared-service"}, Owner: "worker-a", Kind: "writer", Object: f.Request("root", 0).Head.Object, Deadline: f.Now.Add(5 * time.Minute)}
}
func TestLocalR1AdmissionClosureRenewalAndTakeover(t *testing.T) {
	ctx := context.Background()
	m, peer, f, c := sharedFixture(t)
	r := r1Request(f, "same-slot", 1)
	type result struct {
		err   error
		owner string
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	for _, worker := range []*Store{m, peer} {
		go func(worker *Store) {
			q := r
			if worker == peer {
				q.Owner = "worker-b"
			}
			<-start
			_, err := worker.AcquireR1(ctx, q)
			results <- result{err, q.Owner}
		}(worker)
	}
	close(start)
	winners := 0
	for range 2 {
		out := <-results
		if out.err == nil {
			winners++
			r.Owner = out.owner
		} else if !errors.Is(out.err, s.ErrConflict) {
			t.Fatal(out.err)
		}
	}
	if winners != 1 {
		t.Fatal("overlapping producer owners", winners)
	}
	lease, err := peer.CheckR1(ctx, r.Fence)
	if err != nil {
		t.Fatal(err)
	}
	revision := lease.Revision
	c.Advance(10 * time.Second)
	lease, err = m.RenewR1(ctx, r.Fence)
	if err != nil || lease.Revision <= revision {
		t.Fatal("renewal", err, lease)
	}
	if err = m.BeginR1Action(ctx, r.Fence, "uncertain-transaction"); err != nil {
		t.Fatal(err)
	}
	if err = m.PinR1(ctx, r.Fence, []s.PackRef{f.Packs[0].Ref}); err != nil {
		t.Fatal(err)
	}
	c.Advance(30 * time.Second)
	if _, err = peer.RenewR1(ctx, r.Fence); !errors.Is(err, s.ErrFenced) {
		t.Fatal("renewed at expiry equality", err)
	}
	if err = peer.ReclaimR1(ctx, r.Fence, 5*time.Second); !errors.Is(err, s.ErrR1Busy) {
		t.Fatal("ignored grace", err)
	}
	c.Advance(5 * time.Second)
	if err = peer.ReclaimR1(ctx, r.Fence, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if err = peer.ReleaseR1(ctx, r.Fence); !errors.Is(err, s.ErrR1Busy) {
		t.Fatal("unknown released pins", err)
	}
	next := r
	next.Fence.Epoch++
	next.Owner = "worker-b"
	if _, err = peer.AcquireR1(ctx, next); !errors.Is(err, s.ErrFenced) {
		t.Fatal("takeover before reconciliation", err)
	}
	// Trusted reconciliation has now confirmed original transaction terminal.
	if err = m.SettleR1Action(ctx, r.Fence, "uncertain-transaction"); err != nil {
		t.Fatal(err)
	}
	if err = m.ReleaseR1(ctx, r.Fence); err != nil {
		t.Fatal(err)
	}
	if _, err = peer.AcquireR1(ctx, next); err != nil {
		t.Fatal("takeover", err)
	}
	if _, err = m.CheckR1(ctx, r.Fence); !errors.Is(err, s.ErrFenced) {
		t.Fatal("stale worker accepted", err)
	}
	if err = peer.CloseR1(ctx, next.Fence); err != nil {
		t.Fatal(err)
	}
	if err = peer.ReleaseR1(ctx, next.Fence); err != nil {
		t.Fatal(err)
	}
	g, err := m.GetR1Gate(ctx)
	if err != nil || g.Active != 0 || g.Resolving != 0 {
		t.Fatal("leaked membership", g, err)
	}
	if err = m.SetR1Gate(ctx, g.Revision, "Closing"); err != nil {
		t.Fatal(err)
	}
	newRequest := r1Request(f, "new-slot", 1)
	if _, err = peer.AcquireR1(ctx, newRequest); !errors.Is(err, s.ErrR1Busy) {
		t.Fatal("gate busy accepted admission", err)
	}
	g, _ = m.GetR1Gate(ctx)
	if err = m.RotateR1(ctx, g.Revision, "new-incarnation"); err != nil {
		t.Fatal(err)
	}
	if _, err = peer.AcquireR1(ctx, newRequest); !errors.Is(err, s.ErrFenced) {
		t.Fatal("old service incarnation admitted", err)
	}
}

type r1DelayedAPI struct {
	API
	entered, release chan struct{}
}

type r1LostAckAPI struct {
	API
	lost bool
}

func (a *r1LostAckAPI) TransactWriteItems(ctx context.Context, in *ddb.TransactWriteItemsInput, opts ...func(*ddb.Options)) (*ddb.TransactWriteItemsOutput, error) {
	out, err := a.API.TransactWriteItems(ctx, in, opts...)
	if err == nil && !a.lost {
		a.lost = true
		return nil, errors.New("lost R1 transaction acknowledgement")
	}
	return out, err
}
func TestLocalR1LostAcknowledgementsKeepExactMembership(t *testing.T) {
	ctx := context.Background()
	m, peer, f, _ := sharedFixture(t)
	r := r1Request(f, "lost-ack", 1)
	m.api = &r1LostAckAPI{API: m.api}
	if _, err := m.AcquireR1(ctx, r); !errors.Is(err, s.ErrUnavailable) {
		t.Fatal("lost acquire ACK not unknown", err)
	}
	if _, err := m.AcquireR1(ctx, r); err != nil {
		t.Fatal("same acquire retry", err)
	}
	g, err := peer.GetR1Gate(ctx)
	if err != nil || g.Active != 1 || g.Resolving != 1 {
		t.Fatal("duplicate acquire membership", g, err)
	}
	m.api = &r1LostAckAPI{API: m.api}
	if err = m.BeginR1Action(ctx, r.Fence, "not-yet-dispatched"); !errors.Is(err, s.ErrUnavailable) {
		t.Fatal(err)
	}
	l, err := peer.GetR1Lease(ctx, r.Fence.LeaseID)
	if err != nil || len(l.Pending) != 1 {
		t.Fatal("unknown registration lost", l, err)
	}
	m.api = &r1LostAckAPI{API: m.api}
	if err = m.CloseR1(ctx, r.Fence); !errors.Is(err, s.ErrUnavailable) {
		t.Fatal(err)
	}
	if _, err = peer.CheckR1(ctx, r.Fence); !errors.Is(err, s.ErrFenced) {
		t.Fatal("closed authority revived after lost ACK", err)
	}
	if err = peer.ReleaseR1(ctx, r.Fence); !errors.Is(err, s.ErrR1Busy) {
		t.Fatal("unknown released", err)
	}
	// Original synchronous caller knows no action was dispatched because the
	// registration returned unknown. Peers cannot infer this from a missing head.
	if err = m.SettleR1Action(ctx, r.Fence, "not-yet-dispatched"); err != nil {
		t.Fatal(err)
	}
	if err = m.ReleaseR1(ctx, r.Fence); err != nil {
		t.Fatal(err)
	}
	g, _ = peer.GetR1Gate(ctx)
	if g.Active != 0 || g.Resolving != 0 {
		t.Fatal("reconciliation membership", g)
	}
}

func TestLocalR1PagedPinsReleaseResumesAfterLostACK(t *testing.T) {
	ctx := context.Background()
	m, peer, f, _ := sharedFixture(t)
	r := r1Request(f, "many-pins", 1)
	r.Kind = "reader"
	if _, err := m.AcquireR1(ctx, r); err != nil {
		t.Fatal(err)
	}
	refs := make([]s.PackRef, 80)
	if err := m.update(ctx, func(tx *transaction) error {
		for i := range refs {
			refs[i] = s.PackRef{ID: fmt.Sprintf("paged-%03d", i), Generation: "generation-1"}
			if err := put(tx, packKey(refs[i]), s.PackInfo{Version: 1, Ref: refs[i], Size: 64, Class: s.Standard, State: "Warm", StateRevision: 1}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for start := 0; start < len(refs); start += 32 {
		if err := m.PinR1(ctx, r.Fence, refs[start:min(start+32, len(refs))]); err != nil {
			t.Fatal("bounded pin batch", err)
		}
	}
	if err := m.PinR1(ctx, r.Fence, refs[:10]); err != nil {
		t.Fatal("idempotent pin", err)
	}
	l, err := m.GetR1Lease(ctx, r.Fence.LeaseID)
	if err != nil || l.PinCount != 80 {
		t.Fatal("membership count", l, err)
	}
	if _, _, err = peer.PutChunkIfAbsent(ctx, f.Chunk(f.Bucket.Domain), r.Fence); !errors.Is(err, s.ErrFenced) {
		t.Fatal("reader lease used as writer", err)
	}
	if err = m.CloseR1(ctx, r.Fence); err != nil {
		t.Fatal(err)
	}
	m.api = &r1LostAckAPI{API: m.api}
	if err = m.ReleaseR1(ctx, r.Fence); !errors.Is(err, s.ErrUnavailable) {
		t.Fatal("lost release ACK", err)
	}
	l, err = peer.GetR1Lease(ctx, r.Fence.LeaseID)
	if err != nil || l.PinCount != 48 || l.PinCursor == "" || l.State != "Closed" {
		t.Fatal("release progress not durable", l, err)
	}
	g, _ := peer.GetR1Gate(ctx)
	if g.Active != 1 || g.Resolving != 1 {
		t.Fatal("partial release dropped protection", g)
	}
	if err = peer.ReleaseR1(ctx, r.Fence); err != nil {
		t.Fatal("resumable release", err)
	}
	g, _ = peer.GetR1Gate(ctx)
	if g.Active != 0 || g.Resolving != 0 {
		t.Fatal("final release membership", g)
	}
	if err = peer.view(ctx, func(tx *transaction) error {
		for _, ref := range refs {
			pin, err := r1Read[r1Pins](tx, r1PinKey(ref))
			if err != nil {
				return err
			}
			if pin.Count != 0 {
				return s.ErrConflict
			}
		}
		return nil
	}); err != nil {
		t.Fatal("double-decrement or lost pin", err)
	}
}

func (a r1DelayedAPI) TransactWriteItems(ctx context.Context, in *ddb.TransactWriteItemsInput, opts ...func(*ddb.Options)) (*ddb.TransactWriteItemsOutput, error) {
	close(a.entered)
	<-a.release
	return a.API.TransactWriteItems(ctx, in, opts...)
}
func TestLocalR1DispatchSettlementBeforeAndAfterClosure(t *testing.T) {
	for _, closure := range []bool{false, true} {
		name := "late-before-closure"
		if closure {
			name = "late-after-closure"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			m, peer, f, c := sharedFixture(t)
			r := r1Request(f, "late", 1)
			if _, err := m.AcquireR1(ctx, r); err != nil {
				t.Fatal(err)
			}
			tx := m.transaction(ctx)
			if err := m.fence(tx, r.Fence); err != nil {
				t.Fatal(err)
			}
			chunk := f.Chunk(f.Bucket.Domain)
			if err := put(tx, chunkKey(chunk.Key), chunk); err != nil {
				t.Fatal(err)
			}
			barrier := r1DelayedAPI{API: m.api, entered: make(chan struct{}), release: make(chan struct{})}
			m.api = barrier
			settled := make(chan error, 1)
			go func() { settled <- tx.commit() }()
			<-barrier.entered
			c.Advance(31 * time.Second)
			if closure {
				if err := peer.CloseR1(ctx, r.Fence); err != nil {
					t.Fatal(err)
				}
			}
			close(barrier.release)
			err := <-settled
			if !closure && err != nil {
				t.Fatal("valid dispatched transaction lost after expiry", err)
			}
			if closure && !errors.Is(err, s.ErrConflict) {
				t.Fatal("post-closure publication accepted", err)
			}
			if _, _, err = peer.PutChunkIfAbsent(ctx, chunk, r.Fence); !errors.Is(err, s.ErrFenced) {
				t.Fatal("stale new dispatch accepted", err)
			}
		})
	}
}

type r1Proof struct{ Ready, Terminal bool }
type r1Guard struct{}

func (r1Guard) Prepare(_ context.Context, _ s.R1Maintenance, observe Observe) error {
	p, err := r1Observed[r1Proof](observe, stringKey("SystemConfig", "config_id", "r1-test-root-proof"))
	if err != nil {
		return err
	}
	if !p.Ready {
		return s.ErrR1Busy
	}
	return nil
}
func (r1Guard) Terminal(_ context.Context, _ s.R1Maintenance, observe Observe) error {
	p, err := r1Observed[r1Proof](observe, stringKey("SystemConfig", "config_id", "r1-test-root-proof"))
	if err != nil {
		return err
	}
	if !p.Terminal {
		return s.ErrR1Busy
	}
	return nil
}

type r1UnknownPhysical struct {
	s.PackStore
	calls int
}

func (p *r1UnknownPhysical) DeletePack(context.Context, s.PackRef) error {
	p.calls++
	return s.ErrUnavailable
}
func (p *r1UnknownPhysical) ChangeStorageClass(context.Context, s.PackRef, s.StorageClass) error {
	p.calls++
	return s.ErrUnavailable
}
func TestLocalR1MaintenanceUnknownKeepsClosedPhysicalState(t *testing.T) {
	for _, action := range []string{"delete", "transition"} {
		t.Run(action, func(t *testing.T) {
			ctx := context.Background()
			m, peer, f, _ := sharedFixture(t)
			g, _ := m.GetR1Gate(ctx)
			if err := m.SetR1Gate(ctx, g.Revision, "Closing"); err != nil {
				t.Fatal(err)
			}
			r := s.R1Maintenance{ID: "action-" + action, Incarnation: "shared-service", Pack: f.Packs[0].Ref, Action: action, StateRevision: 1, Target: s.GlacierIR}
			physical := &r1UnknownPhysical{}
			if err := coordination.RunSharedMaintenance(ctx, m, physical, r); !errors.Is(err, s.ErrR1Busy) || physical.calls != 0 {
				t.Fatal("missing root proof dispatched", err)
			}
			guard := m.authority.(*SharedAuthority)
			guard.Maintenance = r1Guard{}
			if err := m.update(ctx, func(tx *transaction) error {
				return r1Put(tx, stringKey("SystemConfig", "config_id", "r1-test-root-proof"), r1Proof{Ready: true})
			}); err != nil {
				t.Fatal(err)
			}
			if err := coordination.RunSharedMaintenance(ctx, m, physical, r); !errors.Is(err, s.ErrUnavailable) || physical.calls != 1 {
				t.Fatal("unknown dispatch", err)
			}
			pending, err := peer.GetR1Maintenance(ctx, r.ID)
			if err != nil || pending.State != "Unknown" {
				t.Fatal("unknown not durable", pending, err)
			}
			packs, err := peer.GetPacks(ctx, []s.PackRef{r.Pack})
			want := "Deleting"
			if action == "transition" {
				want = "Transitioning"
			}
			if err != nil || packs[r.Pack].State != want {
				t.Fatal("state reopened", packs, err)
			}
			g, _ = peer.GetR1Gate(ctx)
			if err = peer.SetR1Gate(ctx, g.Revision, "Open"); !errors.Is(err, s.ErrConflict) {
				t.Fatal("unknown gate reopened", err)
			}
			if err = m.FinishR1Maintenance(ctx, r.ID); !errors.Is(err, s.ErrR1Busy) {
				t.Fatal("HEAD/timeout treated as terminal", err)
			}
			if err := m.update(ctx, func(tx *transaction) error {
				return r1Put(tx, stringKey("SystemConfig", "config_id", "r1-test-root-proof"), r1Proof{Ready: true, Terminal: true})
			}); err != nil {
				t.Fatal(err)
			}
			if err = m.FinishR1Maintenance(ctx, r.ID); err != nil {
				t.Fatal("terminal reconciliation", err)
			}
			if err = coordination.RunSharedMaintenance(ctx, m, physical, r); !errors.Is(err, s.ErrR1Busy) || physical.calls != 1 {
				t.Fatal("replayed destructive dispatch", err)
			}
		})
	}
}
