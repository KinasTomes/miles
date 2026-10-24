package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/clock"
	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/KinasTomes/miles/internal/storage/contracttest"
	"github.com/KinasTomes/miles/internal/storage/fake"
)

// snapshotCase is a row of authorization.json "snapshot": times are
// milliseconds of the monotonic clock of the node that reads the snapshot.
type snapshotCase struct {
	Name       string
	Phase      string // admit, publish or stream
	ReadAt     int64  `json:"read_at"`
	At         int64
	RevokedAt  *int64 `json:"revoked_at"`
	ExpiresAt  *int64 `json:"expires_at"`
	Expect     string // allow, deny or may_allow
	MustReread bool   `json:"must_reread"`
}

// world is one fixture row's metadata: the credential writer-key of the
// contract fixture is revoked at revokedAt, and every load is counted.
type world struct {
	t         *testing.T
	fixture   contracttest.Fixture
	store     *fake.Metadata
	clock     *clock.Virtual
	start     time.Time
	elapsed   int64 // ms
	revokedAt *int64
	revoked   bool
	loads     int
}

func newWorld(t *testing.T, revokedAt *int64) *world {
	f := contracttest.NewFixture()
	clk, err := clock.NewVirtual(f.Now, 100)
	if err != nil {
		t.Fatal(err)
	}
	store := fake.NewMetadata(fake.Seed{Now: func() time.Time { return f.Now }, Buckets: []s.Bucket{f.Bucket}, Users: f.Users, Credentials: f.Credentials, Grants: []s.Grant{f.Grant}, Quotas: []s.LogicalQuota{f.Quota}, Packs: f.Packs, Writers: []s.WriterLease{f.Lease}})
	return &world{t: t, fixture: f, store: store, clock: clk, start: f.Now, revokedAt: revokedAt}
}

// advance moves the node clock to ms and applies the revocation once its
// commit time has come.
func (w *world) advance(ms int64) {
	w.t.Helper()
	if err := w.clock.Advance(time.Duration(ms-w.elapsed) * time.Millisecond); err != nil {
		w.t.Fatal(err)
	}
	w.elapsed = ms
	if w.revokedAt != nil && *w.revokedAt <= ms && !w.revoked {
		c := w.fixture.Credentials[1] // writer-key
		c.Active = false
		w.store.SetCredential(c)
		w.revoked = true
	}
}

func (w *world) Authorize(ctx context.Context, credentialID, bucket string) (s.Authorization, error) {
	w.loads++
	return w.store.Authorize(ctx, credentialID, bucket)
}

func readSnapshotCases(t *testing.T) []snapshotCase {
	data, err := os.ReadFile("../../tests/fixtures/s3/authorization.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct{ Snapshot []snapshotCase }
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	return f.Snapshot
}

// TestSnapshotFixture runs the snapshot rows of authorization.json: admit
// rows through Snapshots (a credential revoke) or a token Cache (expiry),
// publish rows through the commit of the fake metadata store, which checks
// the credential itself whatever the snapshot says. The stream row is the
// GET handler's (W03-PHONG-01): an admitted stream is not re-authorized.
func TestSnapshotFixture(t *testing.T) {
	ctx := context.Background()
	for _, c := range readSnapshotCases(t) {
		t.Run(c.Name, func(t *testing.T) {
			w := newWorld(t, c.RevokedAt)
			var got string
			switch {
			case c.Phase == "stream":
				t.Skip("an admitted GET stream is the GET handler's (W03-PHONG-01)")
			case c.Phase == "admit" && c.ExpiresAt != nil:
				got = w.admitToken(ctx, c)
			case c.Phase == "admit":
				snaps := NewSnapshots(w, w.clock)
				w.advance(c.ReadAt)
				snaps.Authorize(ctx, "writer-key", "demo")
				w.loads = 0
				w.advance(c.At)
				_, err := snaps.Authorize(ctx, "writer-key", "demo")
				got = verdict(t, err)
			case c.Phase == "publish":
				got = w.publish(ctx, c)
			default:
				t.Fatalf("unknown phase %q", c.Phase)
			}
			switch {
			case c.Expect == "may_allow" && got != "allow" && got != "deny",
				c.Expect != "may_allow" && got != c.Expect:
				t.Errorf("got %s, want %s", got, c.Expect)
			case c.MustReread && w.loads == 0:
				t.Error("served from a snapshot past its bound")
			}
		})
	}
}

// admitToken caches a bearer token whose load fails once expires_at passed,
// like bearer verification does.
func (w *world) admitToken(ctx context.Context, c snapshotCase) string {
	expires := w.start.Add(time.Duration(*c.ExpiresAt) * time.Millisecond)
	cache := NewCache(w.clock, func(context.Context, string) (string, time.Time, error) {
		w.loads++
		if !w.clock.Now().WallUTC.Before(expires) {
			return "", time.Time{}, s.ErrAccessDenied
		}
		return "token of writer", expires, nil
	})
	w.advance(c.ReadAt)
	cache.Get(ctx, "token")
	w.loads = 0
	w.advance(c.At)
	_, err := cache.Get(ctx, "token")
	return verdict(w.t, err)
}

// publish admits a PUT at read_at, then commits it at at: the result must
// follow the credential at commit time even while the snapshot still allows.
func (w *world) publish(ctx context.Context, c snapshotCase) string {
	snaps := NewSnapshots(w, w.clock)
	r := w.fixture.Request("put", 0)
	contracttest.Prepare(w.t, w.store, w.fixture, r, 4)
	w.advance(c.ReadAt)
	if _, err := snaps.Authorize(ctx, r.CredentialID, w.fixture.Bucket.Name); err != nil {
		w.t.Fatal("admission:", err)
	}
	w.advance(c.At)
	_, err := w.store.CommitObject(ctx, r)
	return verdict(w.t, err)
}

func verdict(t *testing.T, err error) string {
	t.Helper()
	switch {
	case err == nil:
		return "allow"
	case errors.Is(err, s.ErrAccessDenied):
		return "deny"
	}
	t.Fatal(err)
	return ""
}

// A cache hit must not make a snapshot younger, and a failed read must not
// be kept: a bucket that was missing is read again on the next request.
func TestSnapshotAgeAndFailures(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t, nil)
	snaps := NewSnapshots(w, w.clock)
	for _, ms := range []int64{0, 2000, 4999} {
		w.advance(ms)
		if _, err := snaps.Authorize(ctx, "writer-key", "demo"); err != nil {
			t.Fatal(err)
		}
	}
	if w.loads != 1 {
		t.Fatalf("%d loads within 5 s of the first read, want 1", w.loads)
	}
	w.advance(5000)
	snaps.Authorize(ctx, "writer-key", "demo")
	if w.loads != 2 {
		t.Fatalf("hits at 2 s and 4.999 s extended the snapshot read at 0: %d loads", w.loads)
	}
	for range 2 {
		if _, err := snaps.Authorize(ctx, "writer-key", "missing"); !errors.Is(err, s.ErrNotFound) {
			t.Fatal(err)
		}
	}
	if w.loads != 4 {
		t.Fatalf("a failed read was cached: %d loads", w.loads)
	}
}

// delayedLoad reads the authorization at the load's start and returns it
// holdMs later, after the world moved on without the caller: the metadata
// answered, the response only then arrived.
type delayedLoad struct {
	w      *world
	holdMs int64
}

func (d delayedLoad) Authorize(ctx context.Context, credentialID, bucket string) (s.Authorization, error) {
	a, err := d.w.Authorize(ctx, credentialID, bucket)
	if err == nil {
		d.w.advance(d.w.elapsed + d.holdMs)
	}
	return a, err
}

// A load that returns past the bound of what it read must not serve it:
// the revoke committed at 1 s reaches an admission whose load read the
// active state at 0 and answered at 6 s, so the reread denies. Regression
// of the PR 24 review: the response was served although valid_until, 5 s
// after the read, had passed.
func TestSnapshotOutlivedByItsLoad(t *testing.T) {
	ctx := context.Background()
	revokedAt := int64(1000)
	w := newWorld(t, &revokedAt)
	snaps := NewSnapshots(delayedLoad{w: w, holdMs: 6000}, w.clock)
	if _, err := snaps.Authorize(ctx, "writer-key", "demo"); !errors.Is(err, s.ErrAccessDenied) {
		t.Fatalf("a load answered at 6 s served the state it read at 0: %v", err)
	}
	if w.loads != 2 {
		t.Fatalf("%d loads, want the reread after the first outlived its bound", w.loads)
	}
}

// A slow load does not make the snapshot younger: the read at 0 answered
// at 2 is kept until 5 s after its read, so a hit at 5.5 s reads again.
func TestSlowLoadDoesNotRefreshSnapshotAge(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t, nil)
	snaps := NewSnapshots(delayedLoad{w: w, holdMs: 2000}, w.clock)
	for _, ms := range []int64{0, 5500} {
		w.advance(ms)
		if _, err := snaps.Authorize(ctx, "writer-key", "demo"); err != nil {
			t.Fatal(err)
		}
	}
	if w.loads != 2 {
		t.Fatalf("%d loads, want the 5.5 s hit past the 5 s bound of the read at 0", w.loads)
	}
}

// A bearer token that expires while its load is in flight is not accepted
// when the load returns: the read at 0 with expires_at 1 s, answered at
// 2 s, is refused, and the reread of the already expired token fails as
// ErrExpiredDuringLoad instead of extending the token past its expiry.
func TestBearerExpiringDuringItsLoad(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t, nil)
	expires := w.start.Add(time.Second)
	loads := 0
	cache := NewCache(w.clock, func(context.Context, string) (string, time.Time, error) {
		loads++
		w.advance(2000)
		return "token of writer", expires, nil
	})
	if v, err := cache.Get(ctx, "token"); !errors.Is(err, ErrExpiredDuringLoad) {
		t.Fatalf("bearer expired at 1 s, answered at 2 s: got (%q, %v), want ErrExpiredDuringLoad", v, err)
	}
	if loads != 2 {
		t.Fatalf("%d loads, want the refused reread of the expired token", loads)
	}
}

// Expired snapshots leave the map once it doubles, so its size follows the
// keys read in the last 5 s, not every key ever seen.
func TestCacheDropsExpiredEntries(t *testing.T) {
	ctx := context.Background()
	clk, err := clock.NewVirtual(time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC), 10)
	if err != nil {
		t.Fatal(err)
	}
	cache := NewCache(clk, func(_ context.Context, k string) (string, time.Time, error) { return k, time.Time{}, nil })
	for i := range 1000 {
		cache.Get(ctx, fmt.Sprint(i))
	}
	clk.Advance(MaxAge)
	for i := range 100 {
		cache.Get(ctx, fmt.Sprint("new", i))
	}
	if n := len(cache.entries); n != 100 {
		t.Fatalf("%d entries, want the 100 read after every old snapshot expired", n)
	}
}
