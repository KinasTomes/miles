package object

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"hash"
	"hash/crc32"
	"hash/crc64"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/multipart/core"
	"github.com/KinasTomes/miles/internal/storage"
)

const mib = 1 << 20

func checksumOf(alg string, data []byte) []byte {
	var h hash.Hash
	switch alg {
	case "CRC32":
		h = crc32.NewIEEE()
	case "CRC32C":
		h = crc32.New(crc32.MakeTable(crc32.Castagnoli))
	case "CRC64NVME":
		h = crc64.New(crc64.MakeTable(0x9a6c9329ac4bc9b5))
	case "SHA1":
		h = sha1.New()
	case "SHA256":
		h = sha256.New()
	}
	h.Write(data)
	return h.Sum(nil)
}

// stage creates an upload of key in mode alg/kind and publishes parts by
// number; it returns the upload and the part data and results by number.
func (f *serviceFixture) stage(t *testing.T, ctx context.Context, key, alg, kind string, parts map[uint32][]byte) (storage.MultipartUpload, map[uint32]PartResult) {
	t.Helper()
	u, err := f.service.CreateUpload(ctx, CreateUploadRequest{Bucket: f.bucket, Key: key, Metadata: map[string]string{"content-type": "text/plain"},
		ChecksumAlgorithm: alg, ChecksumType: kind, CredentialID: "writer-key"})
	if err != nil {
		t.Fatal(err)
	}
	results := map[uint32]PartResult{}
	for n, data := range parts {
		b := body(data)
		b.alg, b.crc = alg, checksumOf(alg, data)
		res, err := f.service.UploadPart(ctx, PartRequest{Upload: u, Number: n, Body: b, ContentLength: int64(len(data)), CredentialID: "writer-key"})
		if err != nil {
			t.Fatalf("part %d: %v", n, err)
		}
		results[n] = res
	}
	return u, results
}

// input is the normalized Complete input of parts in order; checksums adds
// each part's Checksum<ALG>.
func input(u storage.MultipartUpload, results map[uint32]PartResult, checksums bool, parts ...uint32) core.Input {
	in := core.Input{V: 1, BucketID: u.Object.BucketID, Key: u.Object.Key, ChecksumAlgorithm: u.ChecksumAlgorithm, ChecksumType: u.ChecksumType}
	for _, n := range parts {
		p := core.Part{PartNumber: int(n), ETag: results[n].ETag}
		if checksums {
			p.Checksum = new(results[n].ChecksumValue)
		}
		in.Parts = append(in.Parts, p)
	}
	return in
}

// complete runs Complete as the initiator on the current upload record.
func (f *serviceFixture) complete(ctx context.Context, id string, in core.Input) (storage.ObjectHead, error) {
	u, err := f.service.GetUpload(ctx, id)
	if err != nil {
		return storage.ObjectHead{}, err
	}
	return f.service.CompleteUpload(ctx, CompleteRequest{Upload: u, Input: in, UserID: "writer", CredentialID: "writer-key"})
}

func (f *serviceFixture) quota(t *testing.T, ctx context.Context) storage.LogicalQuota {
	t.Helper()
	q, err := f.metadata.GetQuota(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	return q
}

func (f *serviceFixture) state(t *testing.T, ctx context.Context, id string) storage.MultipartUpload {
	t.Helper()
	u, err := f.metadata.GetUpload(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func multipartETag(parts ...[]byte) string {
	all := md5.New()
	for _, p := range parts {
		sum := md5.Sum(p)
		all.Write(sum[:])
	}
	return `"` + hex.EncodeToString(all.Sum(nil)) + "-" + strconv.Itoa(len(parts)) + `"`
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func TestCompleteLargeUploadReadsBackOnce(t *testing.T) {
	ctx := context.Background()
	f := newServiceFixtureQuota(t, nil, 64*mib)
	p1, p2, p3 := random(t, 5*mib), random(t, 5*mib), random(t, 3<<10)
	u, res := f.stage(t, ctx, "big.bin", "CRC64NVME", "FULL_OBJECT", map[uint32][]byte{1: p1, 2: p2, 3: p3})
	in := input(u, res, false, 1, 2, 3)
	head, err := f.complete(ctx, u.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	all := concat(p1, p2, p3)
	if head.Revision != 1 || head.Size != int64(len(all)) || head.ETag != multipartETag(p1, p2, p3) || head.ETagKind != "multipart" {
		t.Fatalf("head %+v", head)
	}
	want := base64.StdEncoding.EncodeToString(checksumOf("CRC64NVME", all))
	if head.Checksum != (storage.Checksum{Algorithm: "CRC64NVME", Type: "FULL_OBJECT", Value: want, PartCount: 3}) {
		t.Fatalf("checksum %+v, want the CRC64NVME of the whole object %s", head.Checksum, want)
	}
	got, data := f.get(t, ctx, "big.bin", nil)
	if string(data) != string(all) || got.Info.ETag != head.ETag || got.Info.LastModified.Equal(time.Unix(0, 0)) {
		t.Fatalf("read back %d bytes, info %+v", len(data), got.Info)
	}
	q := f.quota(t, ctx)
	if q.Used != int64(len(all)) || q.Reserved != 0 || q.Staged != 0 || q.ActiveUploads != 0 {
		t.Fatalf("quota %+v", q)
	}
	if s := f.state(t, ctx, u.ID); s.State != "Completed" {
		t.Fatal(s.State)
	}

	// A lost response is retried: the receipt answers, nothing is charged
	// again, and an overwrite of the key is never undone by the retry.
	again, err := f.complete(ctx, u.ID, in)
	if err != nil || again.Revision != 1 || again.ETag != head.ETag {
		t.Fatal("retry", again, err)
	}
	newer := random(t, 1<<10)
	f.put(t, ctx, "big.bin", newer)
	again, err = f.complete(ctx, u.ID, in)
	if err != nil || again.Revision != 1 {
		t.Fatal("retry after overwrite", again, err)
	}
	if _, data := f.get(t, ctx, "big.bin", nil); string(data) != string(newer) {
		t.Fatal("a retry republished the completed object")
	}
	if q := f.quota(t, ctx); q.Used != int64(len(newer)) || q.Reserved != 0 || q.ActiveUploads != 0 {
		t.Fatalf("quota after retries %+v", q)
	}
	if _, err := f.complete(ctx, u.ID, input(u, res, false, 1, 2)); !errors.Is(err, storage.ErrConflict) {
		t.Fatal("a retry with other parts must not get the receipt", err)
	}

	// The receipt outlives the 24-hour upload expiry and ends after 7 days.
	f.advance(48 * time.Hour)
	if _, err := f.complete(ctx, u.ID, in); err != nil {
		t.Fatal("receipt after Active expiry", err)
	}
	f.advance(5 * 24 * time.Hour)
	if _, err := f.complete(ctx, u.ID, in); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("receipt after 7 days", err)
	}
}

func TestCompleteChecksumModes(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, alg, kind string
		parts           []uint32 // selected; parts 1..3 are uploaded
		checksums       bool
	}{
		{"CRC32C FULL_OBJECT over a gap", "CRC32C", "FULL_OBJECT", []uint32{1, 3}, false},
		{"CRC32 FULL_OBJECT", "CRC32", "FULL_OBJECT", []uint32{1, 2, 3}, true},
		{"SHA256 COMPOSITE", "SHA256", "COMPOSITE", []uint32{1, 2, 3}, true},
		{"CRC32 COMPOSITE", "CRC32", "COMPOSITE", []uint32{1, 2, 3}, true},
		{"SHA1 COMPOSITE", "SHA1", "COMPOSITE", []uint32{1, 2, 3}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newServiceFixtureQuota(t, nil, 64*mib)
			data := map[uint32][]byte{1: random(t, 5*mib), 2: random(t, 5*mib), 3: random(t, 7<<10)}
			u, res := f.stage(t, ctx, "k", tc.alg, tc.kind, data)
			head, err := f.complete(ctx, u.ID, input(u, res, tc.checksums, tc.parts...))
			if err != nil {
				t.Fatal(err)
			}
			var selected [][]byte
			composite := checksumHashOf(tc.alg)
			for _, n := range tc.parts {
				selected = append(selected, data[n])
				composite.Write(checksumOf(tc.alg, data[n]))
			}
			all := concat(selected...)
			want := base64.StdEncoding.EncodeToString(checksumOf(tc.alg, all))
			if tc.kind == "COMPOSITE" {
				want = base64.StdEncoding.EncodeToString(composite.Sum(nil)) + "-3"
			}
			if head.Checksum.Value != want || head.Checksum.Type != tc.kind || head.ETag != multipartETag(selected...) {
				t.Fatalf("checksum %+v ETag %s, want %s", head.Checksum, head.ETag, want)
			}
			if _, got := f.get(t, ctx, "k", nil); string(got) != string(all) {
				t.Fatal("read back differs")
			}
			// An unselected part is staging the upload consumes, not usage.
			if q := f.quota(t, ctx); q.Used != int64(len(all)) || q.Staged != 0 || q.Reserved != 0 {
				t.Fatalf("quota %+v", q)
			}
		})
	}
}

func checksumHashOf(alg string) hash.Hash {
	switch alg {
	case "SHA1":
		return sha1.New()
	case "SHA256":
		return sha256.New()
	case "CRC32":
		return crc32.NewIEEE()
	}
	return crc32.New(crc32.MakeTable(crc32.Castagnoli))
}

func TestCompleteValidationReopensActive(t *testing.T) {
	ctx := context.Background()
	f := newServiceFixtureQuota(t, nil, 64*mib)
	data := map[uint32][]byte{1: random(t, 5*mib), 2: random(t, 1<<10), 3: random(t, 1<<10)}
	u, res := f.stage(t, ctx, "k", "CRC64NVME", "FULL_OBJECT", data)
	before := f.quota(t, ctx)
	valid := input(u, res, false, 1, 2)
	wrongETag := input(u, res, false, 1, 2)
	wrongETag.Parts[0].ETag = strings.Repeat("0", 32)
	wrongSum := input(u, res, true, 1, 2)
	wrongSum.Parts[0].Checksum = new(base64.StdEncoding.EncodeToString(make([]byte, 8)))
	wrongSize := input(u, res, false, 1, 2)
	wrongSize.ObjectSize = new(int64(1))
	wrongObjectSum := input(u, res, false, 1, 2)
	wrongObjectSum.ObjectChecksum = new(base64.StdEncoding.EncodeToString(make([]byte, 8)))
	missing := input(u, res, false, 1, 2)
	missing.Parts = append(missing.Parts, core.Part{PartNumber: 4, ETag: strings.Repeat("a", 32)})
	var op string
	for _, tc := range []struct {
		name string
		in   core.Input
		code string
	}{
		{"part never uploaded", missing, "InvalidPart"},
		{"ETag differs", wrongETag, "InvalidPart"},
		{"part checksum differs", wrongSum, "BadDigest"},
		{"small part not last", input(u, res, false, 2, 3), "EntityTooSmall"},
		{"object size differs", wrongSize, "InvalidRequest"},
		{"object checksum differs", wrongObjectSum, "BadDigest"},
	} {
		_, err := f.complete(ctx, u.ID, tc.in)
		var v *core.ValidationError
		if !errors.As(err, &v) || v.Code != tc.code {
			t.Fatalf("%s: error %v, want %s", tc.name, err, tc.code)
		}
		s := f.state(t, ctx, u.ID)
		if s.State != "Active" || s.Publishers != 0 {
			t.Fatalf("%s: upload %s with %d publishers, want Active", tc.name, s.State, s.Publishers)
		}
		if op == "" {
			op = s.CompletionOp
		}
		if s.CompletionOp != op || op == "" {
			t.Fatalf("%s: completion op %q, want the first one %q", tc.name, s.CompletionOp, op)
		}
		if q := f.quota(t, ctx); q != before {
			t.Fatalf("%s: quota %+v, want %+v", tc.name, q, before)
		}
	}
	head, err := f.complete(ctx, u.ID, valid)
	if err != nil || head.OpID != op {
		t.Fatal("complete after reopen", head, err)
	}
}

func TestCompleteQuotaExceededKeepsReservation(t *testing.T) {
	ctx := context.Background()
	f := newServiceFixtureQuota(t, nil, 16*mib)
	f.put(t, ctx, "k", random(t, 6*mib))
	data := map[uint32][]byte{1: random(t, 5*mib), 2: random(t, 1*mib)}
	// Created over the 6 MiB head, the upload reserves nothing beyond it.
	u, res := f.stage(t, ctx, "k", "CRC64NVME", "FULL_OBJECT", data)
	if err := f.service.DeleteObject(ctx, DeleteRequest{Bucket: f.bucket, Key: "k", UserID: "owner", CredentialID: "owner-key"}); err != nil {
		t.Fatal(err)
	}
	f.put(t, ctx, "other", random(t, 12*mib))
	before := f.quota(t, ctx)
	in := input(u, res, false, 1, 2)
	if _, err := f.complete(ctx, u.ID, in); !errors.Is(err, storage.ErrQuotaExceeded) {
		t.Fatal("complete over quota", err)
	}
	if s := f.state(t, ctx, u.ID); s.State != "Active" {
		t.Fatal("upload", s.State)
	}
	if q := f.quota(t, ctx); q != before {
		t.Fatalf("quota %+v, want the upload's reservation kept %+v", q, before)
	}
	if err := f.service.DeleteObject(ctx, DeleteRequest{Bucket: f.bucket, Key: "other", UserID: "owner", CredentialID: "owner-key"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.complete(ctx, u.ID, in); err != nil {
		t.Fatal("retry after freeing quota", err)
	}
	if q := f.quota(t, ctx); q.Used != 6*mib || q.Reserved != 0 || q.Staged != 0 || q.ActiveUploads != 0 {
		t.Fatalf("quota %+v", q)
	}
}

func TestCompleteCancelsOpenPublisher(t *testing.T) {
	ctx := context.Background()
	f := newServiceFixtureQuota(t, nil, 64*mib)
	data := map[uint32][]byte{1: random(t, 5*mib), 2: random(t, 1<<10)}
	u, res := f.stage(t, ctx, "k", "CRC64NVME", "FULL_OBJECT", data)
	late := random(t, 2<<10)
	gate := &gatedBody{testBody: body(late), started: make(chan struct{}), release: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		_, err := f.service.UploadPart(ctx, PartRequest{Upload: u, Number: 3, Body: gate, ContentLength: int64(len(late)), CredentialID: "writer-key"})
		done <- err
	}()
	<-gate.started
	if n := f.openPublishers(t, ctx, u.ID); n != 1 {
		t.Fatal("open publishers", n)
	}
	head, err := f.complete(ctx, u.ID, input(u, res, false, 1, 2))
	if err != nil {
		t.Fatal(err)
	}
	close(gate.release)
	if err := <-done; err == nil {
		t.Fatal("a part received before the freeze published after it")
	}
	pubs, err := f.metadata.ListPublishers(ctx, u.ID, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pubs.Publishers {
		if p.PartNumber == 3 && p.State != "Cancelled" {
			t.Fatalf("late publisher %s, want Cancelled", p.State)
		}
	}
	if q := f.quota(t, ctx); q.Used != head.Size || q.Reserved != 0 || q.Staged != 0 {
		t.Fatalf("quota %+v", q)
	}
}

// grace is past a completion lease (60 s) and its grace (5 s).
const grace = 66 * time.Second

// TestCompleteKillPointsTakeOverOnce stops the completing worker at each kill
// point of API v2 4.3: the upload stays Completing with its reservation, a
// retry in the lease is OperationAborted, and after lease and grace a retry
// takes over the same completion op and commits exactly once.
func TestCompleteKillPointsTakeOverOnce(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name          string
		refuse, after string // operation prefix the kill hits
	}{
		{name: "after freeze", refuse: "prepare/"},
		{name: "after prepare", after: "prepare/"},
		{name: "before the transaction", refuse: "commit/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newServiceFixtureQuota(t, nil, 64*mib)
			data := map[uint32][]byte{1: random(t, 5*mib), 2: random(t, 3<<10)}
			u, res := f.stage(t, ctx, "k", "CRC64NVME", "FULL_OBJECT", data)
			in := input(u, res, false, 1, 2)
			staged := f.quota(t, ctx)
			killed := true
			hit := func(prefix string) func(string) error {
				return func(op string) error {
					if killed && prefix != "" && strings.HasPrefix(op, prefix) {
						return storage.ErrUnavailable
					}
					return nil
				}
			}
			f.refuse, f.after = hit(tc.refuse), hit(tc.after)
			if _, err := f.complete(ctx, u.ID, in); !errors.Is(err, storage.ErrUnavailable) {
				t.Fatal("killed attempt", err)
			}
			killed = false
			s := f.state(t, ctx, u.ID)
			if s.State != "Completing" || f.quota(t, ctx) != staged {
				t.Fatalf("after the kill: %s, quota %+v, want Completing with %+v", s.State, f.quota(t, ctx), staged)
			}
			if _, err := f.complete(ctx, u.ID, in); !errors.Is(err, storage.ErrConflict) {
				t.Fatal("retry inside the lease", err)
			}
			f.advance(grace)
			head, err := f.complete(ctx, u.ID, in)
			if err != nil || head.OpID != s.CompletionOp {
				t.Fatal("takeover", head, err)
			}
			if _, got := f.get(t, ctx, "k", nil); string(got) != string(concat(data[1], data[2])) {
				t.Fatal("read back differs")
			}
			if q := f.quota(t, ctx); q.Used != head.Size || q.Reserved != 0 || q.Staged != 0 || q.ActiveUploads != 0 {
				t.Fatalf("quota %+v", q)
			}
		})
	}
}

func TestCompleteLostCommitResponse(t *testing.T) {
	ctx := context.Background()
	f := newServiceFixtureQuota(t, nil, 64*mib)
	data := map[uint32][]byte{1: random(t, 5*mib), 2: random(t, 3<<10)}
	u, res := f.stage(t, ctx, "k", "CRC64NVME", "FULL_OBJECT", data)
	in := input(u, res, false, 1, 2)
	lost := true
	f.after = func(op string) error {
		if lost && strings.HasPrefix(op, "commit/") {
			return storage.ErrUnavailable
		}
		return nil
	}
	head, err := f.complete(ctx, u.ID, in)
	if err != nil || head.Revision != 1 {
		t.Fatal("the committed outcome must be reconciled, not retried", head, err)
	}
	lost = false
	again, err := f.complete(ctx, u.ID, in)
	if err != nil || again.Revision != 1 {
		t.Fatal("retry", again, err)
	}
	if q := f.quota(t, ctx); q.Used != head.Size || q.Reserved != 0 || q.ActiveUploads != 0 {
		t.Fatalf("quota %+v", q)
	}
}

func TestRecoverUploadResumesFrozenInput(t *testing.T) {
	ctx := context.Background()
	f := newServiceFixtureQuota(t, nil, 64*mib)
	data := map[uint32][]byte{1: random(t, 5*mib), 2: random(t, 3<<10)}
	u, res := f.stage(t, ctx, "k", "CRC64NVME", "FULL_OBJECT", data)
	killed := true
	f.after = func(op string) error {
		if killed && strings.HasPrefix(op, "prepare/") {
			return storage.ErrUnavailable
		}
		return nil
	}
	if _, err := f.complete(ctx, u.ID, input(u, res, false, 1, 2)); err == nil {
		t.Fatal("killed attempt completed")
	}
	killed = false
	f.advance(62 * time.Second)
	if _, err := f.service.RecoverUpload(ctx, u.ID); !errors.Is(err, storage.ErrConflict) {
		t.Fatal("recovery inside the grace", err)
	}
	f.advance(4 * time.Second)
	head, err := f.service.RecoverUpload(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, got := f.get(t, ctx, "k", nil); head.Size != int64(len(data[1])+len(data[2])) || string(got) != string(concat(data[1], data[2])) {
		t.Fatal("recovered object differs")
	}
	if _, err := f.service.RecoverUpload(ctx, u.ID); !errors.Is(err, storage.ErrConflict) {
		t.Fatal("recovery of a Completed upload", err)
	}
}

// A Completing upload is not removed by time: past expires_at a takeover
// still resumes its frozen input and commits once (API v2 4.3.1).
func TestRecoverExpiredCompletingResumes(t *testing.T) {
	ctx := context.Background()
	f := newServiceFixtureQuota(t, nil, 64*mib)
	data := map[uint32][]byte{1: random(t, 5*mib), 2: random(t, 3<<10)}
	u, res := f.stage(t, ctx, "k", "CRC64NVME", "FULL_OBJECT", data)
	f.refuse = func(op string) error {
		if strings.HasPrefix(op, "prepare/") {
			return storage.ErrUnavailable
		}
		return nil
	}
	if _, err := f.complete(ctx, u.ID, input(u, res, false, 1, 2)); err == nil {
		t.Fatal("killed attempt completed")
	}
	f.refuse = nil
	f.advance(25 * time.Hour)
	if s := f.state(t, ctx, u.ID); s.State != "Completing" || f.quota(t, ctx).Staged == 0 {
		t.Fatal("expiry alone changed a Completing upload", s.State)
	}
	head, err := f.service.RecoverUpload(ctx, u.ID)
	if err != nil {
		t.Fatal("takeover past expires_at", err)
	}
	if _, got := f.get(t, ctx, "k", nil); string(got) != string(concat(data[1], data[2])) {
		t.Fatal("read back differs")
	}
	if q := f.quota(t, ctx); q.Used != head.Size || q.Reserved != 0 || q.Staged != 0 || q.ActiveUploads != 0 {
		t.Fatalf("quota %+v", q)
	}
}

// A takeover past expires_at that fails a known check cannot reopen an
// expired upload: it aborts and releases the reservation once.
func TestExpiredTakeoverFailureAborts(t *testing.T) {
	ctx := context.Background()
	f := newServiceFixtureQuota(t, nil, 64*mib)
	data := map[uint32][]byte{1: random(t, 5*mib), 2: random(t, 3<<10)}
	u, res := f.stage(t, ctx, "k", "CRC64NVME", "FULL_OBJECT", data)
	in := input(u, res, false, 1, 2)
	refuse := func(prefix string, err error) func(string) error {
		return func(op string) error {
			if strings.HasPrefix(op, prefix) {
				return err
			}
			return nil
		}
	}
	f.refuse = refuse("prepare/", storage.ErrUnavailable)
	if _, err := f.complete(ctx, u.ID, in); err == nil {
		t.Fatal("killed attempt completed")
	}
	f.advance(25 * time.Hour)
	f.refuse = refuse("commit/", storage.ErrQuotaExceeded)
	if _, err := f.complete(ctx, u.ID, in); !errors.Is(err, storage.ErrQuotaExceeded) {
		t.Fatal(err)
	}
	if s := f.state(t, ctx, u.ID); s.State != "Aborted" {
		t.Fatal("upload", s.State)
	}
	q := f.quota(t, ctx)
	if q.Used != 0 || q.Reserved != 0 || q.Staged != 0 || q.ActiveUploads != 0 {
		t.Fatalf("quota %+v", q)
	}
	f.refuse = nil
	if _, err := f.service.RecoverUpload(ctx, u.ID); !errors.Is(err, storage.ErrConflict) || f.quota(t, ctx) != q {
		t.Fatal("recovery after abort", err)
	}
}

// A prepare longer than the 60 s completion lease keeps its fence: the
// heartbeat renews the lease while it runs, and the first attempt commits.
func TestCompleteRenewsLeaseDuringLongPrepare(t *testing.T) {
	ctx := context.Background()
	f := newServiceFixtureQuota(t, nil, 64*mib)
	// The writer's own deadline must outlast the slow prepare.
	o := f.options
	o.RequestTimeout = 30 * time.Minute
	service, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	f.service = service
	v := o.Clock.(*clock.Virtual)
	data := map[uint32][]byte{1: random(t, 5*mib), 2: random(t, 3<<10)}
	u, res := f.stage(t, ctx, "k", "CRC64NVME", "FULL_OBJECT", data)
	renewals := 0
	f.before = func(op string) {
		if !strings.HasPrefix(op, "prepare/") {
			return
		}
		// 75 s of metadata time pass in 15 s steps; each step waits until
		// the heartbeat, driven by the service clock, renewed the lease.
		for range 5 {
			lease := f.state(t, ctx, u.ID).LeaseUntil
			f.advance(15 * time.Second)
			for i := 0; i < 400 && !f.state(t, ctx, u.ID).LeaseUntil.After(lease); i++ {
				if err := v.Advance(time.Second); err != nil {
					t.Error(err)
					return
				}
				time.Sleep(2 * time.Millisecond)
			}
			if f.state(t, ctx, u.ID).LeaseUntil.After(lease) {
				renewals++
			}
		}
	}
	head, err := f.complete(ctx, u.ID, input(u, res, false, 1, 2))
	if err != nil {
		t.Fatalf("complete with a 75 s prepare after %d renewals: %v", renewals, err)
	}
	if renewals != 5 {
		t.Fatal("renewals", renewals)
	}
	if _, got := f.get(t, ctx, "k", nil); string(got) != string(concat(data[1], data[2])) {
		t.Fatal("read back differs")
	}
	if q := f.quota(t, ctx); q.Used != head.Size || q.Reserved != 0 || q.Staged != 0 || q.ActiveUploads != 0 {
		t.Fatalf("quota %+v", q)
	}
}

// A commit refused for the initiator's right (row 12) reopens a client's
// attempt Active; on a takeover it aborts the upload and releases it once.
func TestCompleteDeniedCommit(t *testing.T) {
	ctx := context.Background()
	f := newServiceFixtureQuota(t, nil, 64*mib)
	data := map[uint32][]byte{1: random(t, 5*mib), 2: random(t, 3<<10)}
	u, res := f.stage(t, ctx, "k", "CRC64NVME", "FULL_OBJECT", data)
	in := input(u, res, false, 1, 2)
	staged := f.quota(t, ctx)
	refuse := func(prefix string, err error) func(string) error {
		return func(op string) error {
			if strings.HasPrefix(op, prefix) {
				return err
			}
			return nil
		}
	}
	f.refuse = refuse("commit/", storage.ErrAccessDenied)
	if _, err := f.complete(ctx, u.ID, in); !errors.Is(err, storage.ErrAccessDenied) {
		t.Fatal(err)
	}
	if s := f.state(t, ctx, u.ID); s.State != "Active" || f.quota(t, ctx) != staged {
		t.Fatal("client attempt", s.State, f.quota(t, ctx))
	}
	f.refuse = refuse("prepare/", storage.ErrUnavailable)
	if _, err := f.complete(ctx, u.ID, in); !errors.Is(err, storage.ErrUnavailable) {
		t.Fatal(err)
	}
	f.advance(grace)
	f.refuse = refuse("commit/", storage.ErrAccessDenied)
	if _, err := f.complete(ctx, u.ID, in); !errors.Is(err, storage.ErrAccessDenied) {
		t.Fatal(err)
	}
	if s := f.state(t, ctx, u.ID); s.State != "Aborted" {
		t.Fatal("takeover", s.State)
	}
	if q := f.quota(t, ctx); q.Reserved != 0 || q.Staged != 0 || q.ActiveUploads != 0 {
		t.Fatalf("quota %+v", q)
	}
}
