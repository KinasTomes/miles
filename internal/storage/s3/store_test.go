package s3

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/KinasTomes/miles/internal/pack"
	"github.com/KinasTomes/miles/internal/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

type object struct {
	body    []byte
	class   types.StorageClass
	restore string
}

type fakeAPI struct {
	mu              sync.Mutex
	objects         map[string]object
	failed          error
	lostAck         bool
	puts            int
	restoreCalls    int
	lastRestoreDays int32
}

func newFakeAPI() *fakeAPI { return &fakeAPI{objects: make(map[string]object)} }

func (f *fakeAPI) PutObject(ctx context.Context, in *awss3.PutObjectInput, _ ...func(*awss3.Options)) (*awss3.PutObjectOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(in.Body)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failed != nil {
		return nil, f.failed
	}
	if _, ok := f.objects[*in.Key]; ok {
		return nil, &smithy.GenericAPIError{Code: "PreconditionFailed"}
	}
	if aws.ToString(in.IfNoneMatch) != "*" || int64(len(data)) != aws.ToInt64(in.ContentLength) {
		return nil, errors.New("non-conditional or truncated PUT")
	}
	f.objects[*in.Key] = object{body: data, class: in.StorageClass}
	f.puts++
	if f.lostAck {
		f.lostAck = false
		return nil, errors.New("response lost after durable PUT")
	}
	return &awss3.PutObjectOutput{}, nil
}

func (f *fakeAPI) HeadObject(ctx context.Context, in *awss3.HeadObjectInput, _ ...func(*awss3.Options)) (*awss3.HeadObjectOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.objects[*in.Key]
	if !ok {
		return nil, &smithy.GenericAPIError{Code: "NotFound"}
	}
	out := &awss3.HeadObjectOutput{ContentLength: aws.Int64(int64(len(o.body))), StorageClass: o.class}
	if o.restore != "" {
		out.Restore = aws.String(o.restore)
	}
	return out, nil
}

func (f *fakeAPI) GetObject(ctx context.Context, in *awss3.GetObjectInput, _ ...func(*awss3.Options)) (*awss3.GetObjectOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.objects[*in.Key]
	if !ok {
		return nil, &smithy.GenericAPIError{Code: "NoSuchKey"}
	}
	data := o.body
	out := &awss3.GetObjectOutput{}
	if in.Range != nil {
		var first, last int64
		if _, err := fmt.Sscanf(*in.Range, "bytes=%d-%d", &first, &last); err != nil || first < 0 || last < first || last >= int64(len(data)) {
			return nil, &smithy.GenericAPIError{Code: "InvalidRange"}
		}
		out.ContentRange = aws.String(RangeHeader(first, last-first+1, int64(len(data))))
		data = data[first : last+1]
	}
	out.ContentLength = aws.Int64(int64(len(data)))
	out.Body = io.NopCloser(bytes.NewReader(data))
	return out, nil
}

func (f *fakeAPI) CopyObject(ctx context.Context, in *awss3.CopyObjectInput, _ ...func(*awss3.Options)) (*awss3.CopyObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.objects[*in.Key]
	if !ok {
		return nil, &smithy.GenericAPIError{Code: "NoSuchKey"}
	}
	source, err := url.PathUnescape(aws.ToString(in.CopySource))
	if err != nil || in.MetadataDirective != types.MetadataDirectiveCopy || !strings.HasSuffix(source, "/"+*in.Key) {
		return nil, errors.New("copy lost source or metadata")
	}
	o.class = in.StorageClass
	f.objects[*in.Key] = o
	return &awss3.CopyObjectOutput{}, nil
}

func (f *fakeAPI) RestoreObject(ctx context.Context, in *awss3.RestoreObjectInput, _ ...func(*awss3.Options)) (*awss3.RestoreObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.objects[*in.Key]
	if !ok {
		return nil, &smithy.GenericAPIError{Code: "NoSuchKey"}
	}
	if in.RestoreRequest == nil || in.RestoreRequest.Days == nil || *in.RestoreRequest.Days <= 0 {
		return nil, errors.New("invalid restore")
	}
	f.restoreCalls++
	f.lastRestoreDays = *in.RestoreRequest.Days
	o.restore = `ongoing-request="true"`
	f.objects[*in.Key] = o
	return &awss3.RestoreObjectOutput{}, nil
}

func (f *fakeAPI) DeleteObject(ctx context.Context, in *awss3.DeleteObjectInput, _ ...func(*awss3.Options)) (*awss3.DeleteObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.objects, *in.Key)
	return &awss3.DeleteObjectOutput{}, nil
}

func TestPackStoreImmutableRangeAndSidecar(t *testing.T) {
	api := newFakeAPI()
	store, err := New(api, "dev-bucket", "")
	if err != nil {
		t.Fatal(err)
	}
	ref := storage.PackRef{ID: "ab000000000000000000000000000001", Generation: "cd000000000000000000000000000001"}
	key, sidecarKey, err := keys(ref)
	if err != nil || key != "packs/ab/"+ref.ID+"/"+ref.Generation || sidecarKey != "packs-index/ab/"+ref.ID+"/"+ref.Generation+".idx" {
		t.Fatalf("keys %q %q: %v", key, sidecarKey, err)
	}
	info := storage.PackInfo{Version: storage.ModelVersion, Ref: ref, Size: 8, Class: storage.Standard, State: "Warm"}
	ctx := context.Background()
	if err := store.PutPack(ctx, info, strings.NewReader("abcdefgh")); err != nil {
		t.Fatal(err)
	}
	if err := store.PutPack(ctx, info, strings.NewReader("abcdefgh")); err != nil || api.puts != 1 {
		t.Fatalf("identical retry: %v; puts=%d", err, api.puts)
	}
	if err := store.PutPack(ctx, info, strings.NewReader("abcdxxxx")); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("different content: %v", err)
	}
	if err := store.PutSidecar(ctx, ref, 5, strings.NewReader("index")); err != nil {
		t.Fatal(err)
	}
	if api.objects[sidecarKey].class != types.StorageClassStandard {
		t.Fatal("sidecar was not stored as STANDARD")
	}
	if err := store.PutSidecar(ctx, ref, 5, strings.NewReader("index")); err != nil {
		t.Fatal(err)
	}
	if got, err := store.ReadSidecar(ctx, ref); err != nil || string(got) != "index" {
		t.Fatalf("sidecar %q: %v", got, err)
	}
	for _, span := range []storage.ByteRange{{Offset: 2, Length: 3}, {Offset: 0, Length: 8}} {
		r, err := store.RangeGet(ctx, ref, span)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(r)
		r.Close()
		if err != nil || string(got) != "abcdefgh"[span.Offset:span.Offset+span.Length] {
			t.Fatalf("range %v: %q %v", span, got, err)
		}
	}
	for _, span := range []storage.ByteRange{{Length: 0}, {Offset: -1, Length: 1}, {Offset: 7, Length: 2}} {
		if _, err := store.RangeGet(ctx, ref, span); !errors.Is(err, storage.ErrInvalid) {
			t.Fatalf("invalid range %v: %v", span, err)
		}
	}
	other := ref
	other.Generation = "cd000000000000000000000000000002"
	if err := store.PutPack(ctx, storage.PackInfo{Version: 1, Ref: other, Size: 4, Class: storage.Standard, State: "Warm"}, strings.NewReader("wxyz")); err != nil {
		t.Fatal(err)
	}
	if err := store.DeletePack(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if _, err := store.HeadPack(ctx, other); err != nil {
		t.Fatalf("different generation deleted: %v", err)
	}
	if got, err := store.ReadSidecar(ctx, ref); err != nil || string(got) != "index" {
		t.Fatalf("sidecar unexpectedly deleted: %v", err)
	}
}

func TestPackStoreFaultAndClassBoundaries(t *testing.T) {
	api := newFakeAPI()
	store, _ := New(api, "dev-bucket", "kms-key")
	ref := storage.PackRef{ID: "ab", Generation: "g1"}
	info := storage.PackInfo{Version: 1, Ref: ref, Size: 4, Class: storage.Standard, State: "Warm"}
	ctx := context.Background()
	for _, body := range []string{"abc", "abcde"} {
		if err := store.PutPack(ctx, info, strings.NewReader(body)); !errors.Is(err, storage.ErrInvalid) {
			t.Fatal(err)
		}
	}
	if len(api.objects) != 0 {
		t.Fatal("failed body published an object")
	}
	api.failed = &smithy.GenericAPIError{Code: "SlowDown"}
	if err := store.PutPack(ctx, info, strings.NewReader("data")); !errors.Is(err, storage.ErrUnavailable) {
		t.Fatal(err)
	}
	api.failed = nil
	api.lostAck = true
	if err := store.PutPack(ctx, info, strings.NewReader("data")); !errors.Is(err, storage.ErrUnavailable) {
		t.Fatalf("lost acknowledgement must be unknown: %v", err)
	}
	if err := store.PutPack(ctx, info, strings.NewReader("data")); err != nil {
		t.Fatal(err)
	}
	if err := store.PutSidecar(ctx, ref, 5, strings.NewReader("index")); err != nil {
		t.Fatal(err)
	}
	if err := store.ChangeStorageClass(ctx, ref, storage.GlacierIR); err != nil {
		t.Fatal(err)
	}
	r, err := store.RangeGet(ctx, ref, storage.ByteRange{Length: 4})
	if err != nil {
		t.Fatal("Glacier Instant Retrieval must remain readable", err)
	}
	r.Close()
	if err := store.ChangeStorageClass(ctx, ref, storage.Glacier); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RangeGet(ctx, ref, storage.ByteRange{Length: 4}); !errors.Is(err, storage.ErrCold) {
		t.Fatalf("archive not cold: %v", err)
	}
	tooManyDays := int64(math.MaxInt32) + 1
	if err := store.Restore(ctx, ref, storage.RestoreRequest{Days: int(tooManyDays), Tier: "Standard"}); !errors.Is(err, storage.ErrInvalid) || api.restoreCalls != 0 {
		t.Fatalf("out-of-range restore reached backend: %v calls=%d", err, api.restoreCalls)
	}
	if strconv.IntSize > 32 {
		wrapsPositive := (int64(1) << 32) + 1
		if err := store.Restore(ctx, ref, storage.RestoreRequest{Days: int(wrapsPositive), Tier: "Standard"}); !errors.Is(err, storage.ErrInvalid) || api.restoreCalls != 0 {
			t.Fatalf("positive wrap reached backend: %v calls=%d", err, api.restoreCalls)
		}
	}
	if err := store.Restore(ctx, ref, storage.RestoreRequest{Days: 1, Tier: "Standard"}); err != nil {
		t.Fatal(err)
	}
	if api.restoreCalls != 1 || api.lastRestoreDays != 1 {
		t.Fatalf("valid restore changed request: calls=%d days=%d", api.restoreCalls, api.lastRestoreDays)
	}
	if _, err := store.RangeGet(ctx, ref, storage.ByteRange{Length: 4}); !errors.Is(err, storage.ErrCold) {
		t.Fatalf("restore acceptance wrongly implies readability: %v", err)
	}
	if err := store.ChangeStorageClass(ctx, ref, storage.Standard); !errors.Is(err, storage.ErrInvalid) {
		t.Fatalf("cold-to-warm in place: %v", err)
	}
	if got, err := store.ReadSidecar(ctx, ref); err != nil || string(got) != "index" {
		t.Fatalf("sidecar followed pack into archive: %v", err)
	}
	key, _, _ := keys(ref)
	if api.objects[key].class != types.StorageClassGlacier {
		t.Fatal("pack class not changed")
	}
}

func TestPackRoundTripThroughAdapter(t *testing.T) {
	api := newFakeAPI()
	store, _ := New(api, "dev-bucket", "")
	ref := storage.PackRef{ID: "ab000000000000000000000000000001", Generation: "cd000000000000000000000000000001"}
	raw := []byte("one logical chunk")
	sum := sha256.Sum256(raw)
	builder, err := pack.NewBuilder(ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := builder.Add(pack.Blob{Key: storage.ChunkKey{Domain: "default", Hash: hex.EncodeToString(sum[:])},
		Codec: "raw", RawLength: uint32(len(raw)), Stored: raw}); err != nil {
		t.Fatal(err)
	}
	data, index, entries, err := builder.Finish()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.PutPack(ctx, storage.PackInfo{Version: 1, Ref: ref, Size: int64(len(data)),
		Class: storage.Standard, State: "Warm"}, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if err := store.PutSidecar(ctx, ref, int64(len(index)), bytes.NewReader(index)); err != nil {
		t.Fatal(err)
	}
	if got, err := store.ReadSidecar(ctx, ref); err != nil || !bytes.Equal(got, index) {
		t.Fatalf("sidecar: %v", err)
	}
	verified, err := store.ReadVerifiedSidecar(ctx, ref)
	if err != nil || len(verified) != 1 || verified[0] != entries[0] {
		t.Fatalf("verified sidecar: %v", err)
	}
	fromFooter, _, err := pack.ReadFooter(ctx, store, ref)
	if err != nil || len(fromFooter) != 1 || fromFooter[0] != entries[0] {
		t.Fatalf("footer: %v", err)
	}
	chunk := storage.Chunk{Version: 1, Key: entries[0].Key, RawBytes: int64(len(raw)),
		LocationRevision: 1, Location: entries[0].Location(ref)}
	got, err := pack.ReadCanonicalChunk(ctx, store, chunk)
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("range decode: %q %v", got, err)
	}
}
