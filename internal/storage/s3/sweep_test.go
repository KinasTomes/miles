package s3

import (
	"context"
	"errors"
	"testing"

	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
)

type sweepSDK struct {
	*fakeAPI
	requests []*awss3.DeleteObjectInput
	noRetry  bool
}

func (a *sweepSDK) DeleteObject(_ context.Context, in *awss3.DeleteObjectInput, opts ...func(*awss3.Options)) (*awss3.DeleteObjectOutput, error) {
	a.requests = append(a.requests, in)
	var o awss3.Options
	for _, opt := range opts {
		opt(&o)
	}
	_, a.noRetry = o.Retryer.(aws.NopRetryer)
	return &awss3.DeleteObjectOutput{}, nil
}

func TestSweepDeletesOnlyFencedExactVersion(t *testing.T) {
	a := &sweepSDK{fakeAPI: newFakeAPI()}
	p, e := New(a, "fixture", "")
	if e != nil {
		t.Fatal(e)
	}
	ref := s.PackRef{ID: "pack-a", Generation: "old"}
	v := s.SweepVersion{Category: "sidecar", VersionID: "exact-version"}
	if e = p.DeleteSweepVersion(t.Context(), ref, v); !errors.Is(e, s.ErrFenced) {
		t.Fatal(e)
	}
	denied := s.WithPhysicalDispatch(t.Context(), func(context.Context) error { return s.ErrFenced })
	if e = p.DeleteSweepVersion(denied, ref, v); !errors.Is(e, s.ErrFenced) {
		t.Fatal(e)
	}
	if len(a.requests) != 0 {
		t.Fatal(a.requests)
	}
	allowed := s.WithPhysicalDispatch(t.Context(), func(context.Context) error { return nil })
	if e = p.DeleteSweepVersion(allowed, ref, v); e != nil {
		t.Fatal(e)
	}
	if len(a.requests) != 1 || !a.noRetry {
		t.Fatal(a.requests, a.noRetry)
	}
	_, key, _ := keys(ref)
	req := a.requests[0]
	if aws.ToString(req.Key) != key || aws.ToString(req.VersionId) != v.VersionID {
		t.Fatal(req)
	}
	v.VersionID = "null"
	if e = p.DeleteSweepVersion(allowed, ref, v); !errors.Is(e, s.ErrInvalid) || len(a.requests) != 1 {
		t.Fatal(e, a.requests)
	}
}
