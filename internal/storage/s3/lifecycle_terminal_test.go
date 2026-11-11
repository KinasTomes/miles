package s3

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/jobs"
	"github.com/KinasTomes/miles/internal/lifecycle"
	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

type terminalAPI struct {
	*fakeAPI
	versions map[string][]types.ObjectVersion
	heads    map[string]*awss3.HeadObjectOutput
}

func (a *terminalAPI) ListObjectVersions(_ context.Context, in *awss3.ListObjectVersionsInput, _ ...func(*awss3.Options)) (*awss3.ListObjectVersionsOutput, error) {
	return &awss3.ListObjectVersionsOutput{Versions: a.versions[aws.ToString(in.Prefix)], IsTruncated: aws.Bool(false)}, nil
}

func (a *terminalAPI) HeadObject(ctx context.Context, in *awss3.HeadObjectInput, opts ...func(*awss3.Options)) (*awss3.HeadObjectOutput, error) {
	if in.VersionId != nil {
		if out := a.heads[aws.ToString(in.VersionId)]; out != nil {
			return out, nil
		}
		return nil, errors.New("version not found")
	}
	return a.fakeAPI.HeadObject(ctx, in, opts...)
}

func terminalAction(kind jobs.Type) lifecycle.Action {
	id := strings.Repeat("a", 32)
	return lifecycle.Action{Version: lifecycle.ActionVersionCurrent, ID: id, JobID: id, StepID: strings.Repeat("e", 32), Kind: kind, State: "Unknown", Revision: 2,
		Fence:  s.WriterFence{LeaseID: id, ServiceIncarnation: strings.Repeat("b", 32), Epoch: 1},
		Source: s.PackInfo{Version: 1, Ref: s.PackRef{ID: strings.Repeat("c", 32), Generation: strings.Repeat("d", 32)}, StateRevision: 3, Size: 64, Class: s.GlacierIR, State: "Cold"},
	}
}

func TestLifecycleTerminalRequiresActionBoundS3Version(t *testing.T) {
	a := terminalAction("tiering")
	a.Target = s.GlacierIR
	key, _, _ := keys(a.Source.Ref)
	api := &terminalAPI{fakeAPI: newFakeAPI(), versions: map[string][]types.ObjectVersion{}, heads: map[string]*awss3.HeadObjectOutput{}}
	api.versions[key] = []types.ObjectVersion{{Key: aws.String(key), VersionId: aws.String("version-1")}}
	api.heads["version-1"] = &awss3.HeadObjectOutput{ContentLength: aws.Int64(a.Source.Size), StorageClass: types.StorageClassGlacierIr, Metadata: map[string]string{}}
	store, _ := New(api, "physical", "")
	if terminal, err := store.VerifyLifecycleTerminal(t.Context(), a); !errors.Is(err, jobs.ErrReconciliation) || terminal {
		t.Fatal("unmarked HEAD/version was accepted", terminal, err)
	}
	api.heads["version-1"].Metadata[lifecycleActionMetadata] = a.ID
	if terminal, err := store.VerifyLifecycleTerminal(t.Context(), a); err != nil || !terminal {
		t.Fatal("exact tagged version was not accepted", terminal, err)
	}
	api.heads["version-1"].StorageClass = types.StorageClassStandard
	if terminal, err := store.VerifyLifecycleTerminal(t.Context(), a); !errors.Is(err, s.ErrConflict) || terminal {
		t.Fatal("mismatched physical result was accepted", terminal, err)
	}
}

func TestLifecycleTerminalRequiresEveryExactSweepVersionGone(t *testing.T) {
	a := terminalAction("gc")
	a.Source.Class = s.GlacierIR
	a.SweepVersions = []s.SweepVersion{{Category: "pack", VersionID: "version-1"}}
	key, _, _ := keys(a.Source.Ref)
	api := &terminalAPI{fakeAPI: newFakeAPI(), versions: map[string][]types.ObjectVersion{key: {{Key: aws.String(key), VersionId: aws.String("version-1")}}}, heads: map[string]*awss3.HeadObjectOutput{}}
	store, _ := New(api, "physical", "")
	if terminal, err := store.VerifyLifecycleTerminal(t.Context(), a); !errors.Is(err, jobs.ErrReconciliation) || terminal {
		t.Fatal("a still present exact version was accepted as deleted", terminal, err)
	}
	api.versions[key] = nil
	if terminal, err := store.VerifyLifecycleTerminal(t.Context(), a); err != nil || !terminal {
		t.Fatal("complete exact-version deletion was not accepted", terminal, err)
	}
}

func TestLifecycleRestoreTerminalRequiresPositiveRestoreState(t *testing.T) {
	a := terminalAction("pack_restore")
	a.Source.Class = s.Glacier
	a.RequiredUntil = time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	key, _, _ := keys(a.Source.Ref)
	api := &terminalAPI{fakeAPI: newFakeAPI(), versions: map[string][]types.ObjectVersion{}, heads: map[string]*awss3.HeadObjectOutput{}}
	api.objects[key] = object{body: make([]byte, a.Source.Size), class: types.StorageClassGlacier, restore: `ongoing-request="true"`}
	store, _ := New(api, "physical", "")
	if terminal, err := store.VerifyLifecycleTerminal(t.Context(), a); err != nil || !terminal {
		t.Fatal("positive ongoing restore was not accepted", terminal, err)
	}
	api.objects[key] = object{body: make([]byte, a.Source.Size), class: types.StorageClassGlacier, restore: `ongoing-request="false", expiry-date="Wed, 11 Nov 2026 00:00:00 GMT"`}
	if terminal, err := store.VerifyLifecycleTerminal(t.Context(), a); !errors.Is(err, jobs.ErrReconciliation) || terminal {
		t.Fatal("short restore window was accepted", terminal, err)
	}
}

func TestLifecycleCopyCarriesDurableActionMarker(t *testing.T) {
	api := newFakeAPI()
	store, _ := New(api, "physical", "")
	ref := s.PackRef{ID: strings.Repeat("a", 32), Generation: strings.Repeat("b", 32)}
	key, _, _ := keys(ref)
	expires := time.Date(2026, 12, 1, 1, 2, 3, 0, time.UTC)
	api.objects[key] = object{body: []byte("pack"), class: types.StorageClassStandard, metadata: map[string]string{"custom-integrity-id": "preserve-me", lifecycleActionMetadata: "old-action"},
		contentType: "application/x-miles-pack", cacheControl: "private, max-age=60", contentDisposition: "attachment; filename=pack.miles", contentEncoding: "gzip", contentLanguage: "en", expires: &expires, websiteRedirectLocation: "/packs/original"}
	ctx := s.WithLifecycleAction(t.Context(), strings.Repeat("c", 32))
	if err := store.ChangeStorageClass(ctx, ref, s.GlacierIR); err != nil {
		t.Fatal(err)
	}
	got := api.objects[key]
	if got.metadata["custom-integrity-id"] != "preserve-me" || got.metadata[lifecycleActionMetadata] != strings.Repeat("c", 32) || got.contentType != "application/x-miles-pack" || got.cacheControl != "private, max-age=60" || got.contentDisposition != "attachment; filename=pack.miles" || got.contentEncoding != "gzip" || got.contentLanguage != "en" || got.expires == nil || !got.expires.Equal(expires) || got.websiteRedirectLocation != "/packs/original" {
		t.Fatalf("copy did not preserve source metadata/headers and add the action marker: %+v", got)
	}
}
