package s3

import (
	"bytes"
	"context"
	"errors"
	"github.com/KinasTomes/miles/internal/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"io"
	"testing"
)

type lateHeadAPI struct {
	*fakeAPI
	afterHead     func()
	copies        int
	retryDisabled bool
}

func (a *lateHeadAPI) HeadObject(ctx context.Context, in *awss3.HeadObjectInput, opts ...func(*awss3.Options)) (*awss3.HeadObjectOutput, error) {
	out, e := a.fakeAPI.HeadObject(ctx, in, opts...)
	a.afterHead()
	return out, e
}
func (a *lateHeadAPI) CopyObject(ctx context.Context, in *awss3.CopyObjectInput, opts ...func(*awss3.Options)) (*awss3.CopyObjectOutput, error) {
	a.copies++
	var o awss3.Options
	for _, apply := range opts {
		apply(&o)
	}
	_, a.retryDisabled = o.Retryer.(aws.NopRetryer)
	return a.fakeAPI.CopyObject(ctx, in, opts...)
}
func TestPhysicalGuardAfterSlowHeadAndNoHiddenCopyRetry(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(map[bool]string{false: "valid", true: "expired"}[expired], func(t *testing.T) {
			api := &lateHeadAPI{fakeAPI: newFakeAPI()}
			ref := storage.PackRef{ID: "pack-a", Generation: "generation-1"}
			key, _, _ := keys(ref)
			api.objects[key] = object{body: []byte("data"), class: types.StorageClassStandard}
			lost := false
			api.afterHead = func() { lost = expired }
			store, e := New(api, "fixture-bucket", "")
			if e != nil {
				t.Fatal(e)
			}
			ctx := storage.WithPhysicalDispatch(t.Context(), func(context.Context) error {
				if lost {
					return storage.ErrFenced
				}
				return nil
			})
			e = store.ChangeStorageClass(ctx, ref, storage.GlacierIR)
			if expired {
				if !errors.Is(e, storage.ErrFenced) || api.copies != 0 {
					t.Fatal("new copy after expired read", e, api.copies)
				}
			} else if e != nil || api.copies != 1 || !api.retryDisabled {
				t.Fatal(e, api.copies, api.retryDisabled)
			}
		})
	}
}

type lateBody struct {
	io.Reader
	after func()
}

func (b lateBody) Read(p []byte) (int, error) { n, e := b.Reader.Read(p); b.after(); return n, e }
func TestPhysicalGuardRestorePutAndDelete(t *testing.T) {
	api := &lateHeadAPI{fakeAPI: newFakeAPI()}
	ref := storage.PackRef{ID: "pack-a", Generation: "generation-1"}
	key, _, _ := keys(ref)
	api.objects[key] = object{body: []byte("data"), class: types.StorageClassGlacier}
	expired := false
	api.afterHead = func() { expired = true }
	store, _ := New(api, "fixture-bucket", "")
	ctx := storage.WithPhysicalDispatch(t.Context(), func(context.Context) error {
		if expired {
			return storage.ErrFenced
		}
		return nil
	})
	if e := store.Restore(ctx, ref, storage.RestoreRequest{Days: 1, Tier: "Standard"}); !errors.Is(e, storage.ErrFenced) || api.restoreCalls != 0 {
		t.Fatal("restore dispatched after slow HEAD", e)
	}
	expired = false
	body := lateBody{Reader: bytes.NewReader([]byte("data")), after: func() { expired = true }}
	dest := storage.PackInfo{Version: 1, Ref: storage.PackRef{ID: "pack-b", Generation: "generation-1"}, Size: 4, State: "Warm", Class: storage.Standard}
	if e := store.PutPack(ctx, dest, body); !errors.Is(e, storage.ErrFenced) || api.puts != 0 {
		t.Fatal("PUT dispatched after slow body", e)
	}
	if e := store.DeletePack(ctx, ref); !errors.Is(e, storage.ErrFenced) {
		t.Fatal("delete ignored dispatch guard", e)
	}
	if _, ok := api.objects[key]; !ok {
		t.Fatal("denied delete removed durable bytes")
	}
}
