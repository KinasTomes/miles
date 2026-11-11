package s3

import (
	"context"
	"strings"
	"time"

	"github.com/KinasTomes/miles/internal/jobs"
	"github.com/KinasTomes/miles/internal/lifecycle"
	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
)

const lifecycleActionMetadata = "miles-lifecycle-action"

// VerifyLifecycleTerminal proves a single-shot lifecycle request from an
// immutable S3 version or an exact version deletion. It never treats absence
// from HEAD, elapsed time, or a mutable current-object class as completion.
// The caller must also prove the R1 action owner is isolated before releasing
// its retained protection.
func (store *Store) VerifyLifecycleTerminal(ctx context.Context, a lifecycle.Action) (bool, error) {
	if store == nil || store.client == nil || a.Validate() != nil {
		return false, s.ErrInvalid
	}
	switch a.Kind {
	case "tiering":
		if a.Target == "" || a.Source.Size <= 0 {
			return false, s.ErrInvalid
		}
		key, _, err := keys(a.Source.Ref)
		if err != nil {
			return false, err
		}
		versions, err := store.objectVersionIDs(ctx, key)
		if err != nil {
			return false, err
		}
		matched := 0
		for versionID := range versions {
			if err := ctx.Err(); err != nil {
				return false, err
			}
			out, err := store.client.HeadObject(ctx, &awss3.HeadObjectInput{Bucket: aws.String(store.bucket), Key: aws.String(key), VersionId: aws.String(versionID)})
			if err != nil {
				return false, mapError(err)
			}
			if out == nil || out.Metadata[lifecycleActionMetadata] != a.ID {
				continue
			}
			matched++
			class := s.StorageClass(out.StorageClass)
			if out.ContentLength == nil || *out.ContentLength != a.Source.Size || class != a.Target {
				return false, s.ErrConflict
			}
		}
		if matched != 1 {
			return false, jobs.ErrReconciliation
		}
		return true, nil
	case "gc":
		if !s.ValidSweepVersions(a.SweepVersions) {
			return false, s.ErrInvalid
		}
		for _, target := range a.SweepVersions {
			key, err := lifecycleSweepKey(a.Source.Ref, target.Category)
			if err != nil {
				return false, err
			}
			versions, err := store.objectVersionIDs(ctx, key)
			if err != nil {
				return false, err
			}
			if versions[target.VersionID] {
				return false, jobs.ErrReconciliation
			}
		}
		return true, nil
	case "pack_restore":
		if a.Source.Class != s.Glacier || a.RequiredUntil.IsZero() {
			return false, s.ErrInvalid
		}
		key, _, err := keys(a.Source.Ref)
		if err != nil {
			return false, err
		}
		out, err := store.client.HeadObject(ctx, &awss3.HeadObjectInput{Bucket: aws.String(store.bucket), Key: aws.String(key)})
		if err != nil {
			return false, mapError(err)
		}
		if out == nil || s.StorageClass(out.StorageClass) != s.Glacier || out.Restore == nil {
			return false, jobs.ErrReconciliation
		}
		if strings.Contains(*out.Restore, `ongoing-request="true"`) {
			return true, nil // accepted asynchronous restore; the pack stays protected.
		}
		if !strings.Contains(*out.Restore, `ongoing-request="false"`) {
			return false, jobs.ErrReconciliation
		}
		until, ok := restoreExpiry(*out.Restore)
		if !ok || until.Before(a.RequiredUntil) {
			return false, jobs.ErrReconciliation
		}
		return true, nil
	default:
		return false, s.ErrInvalid
	}
}

func (store *Store) objectVersionIDs(ctx context.Context, key string) (map[string]bool, error) {
	client, ok := store.client.(VersionInventoryAPI)
	if !ok || store.bucket == "" || key == "" {
		return nil, s.ErrUnavailable
	}
	versions := map[string]bool{}
	var keyMarker, versionMarker *string
	for pageNo := range 256 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		page, err := client.ListObjectVersions(ctx, &awss3.ListObjectVersionsInput{
			Bucket: aws.String(store.bucket), Prefix: aws.String(key), KeyMarker: keyMarker,
			VersionIdMarker: versionMarker, MaxKeys: aws.Int32(1000),
		})
		if err != nil {
			return nil, mapError(err)
		}
		if page == nil {
			return nil, s.ErrUnavailable
		}
		for _, version := range page.Versions {
			if version.Key == nil || version.VersionId == nil || *version.VersionId == "" {
				return nil, s.ErrUnavailable
			}
			if *version.Key == key {
				versions[*version.VersionId] = true
			}
		}
		if !aws.ToBool(page.IsTruncated) {
			return versions, nil
		}
		if page.NextKeyMarker == nil || page.NextVersionIdMarker == nil ||
			aws.ToString(page.NextKeyMarker) == aws.ToString(keyMarker) && aws.ToString(page.NextVersionIdMarker) == aws.ToString(versionMarker) {
			return nil, s.ErrUnavailable
		}
		keyMarker, versionMarker = page.NextKeyMarker, page.NextVersionIdMarker
		if pageNo == 255 {
			return nil, s.ErrUnavailable
		}
	}
	return nil, s.ErrUnavailable
}

func lifecycleSweepKey(ref s.PackRef, category string) (string, error) {
	packKey, sidecarKey, err := keys(ref)
	if err != nil {
		return "", err
	}
	switch category {
	case "pack":
		return packKey, nil
	case "sidecar":
		return sidecarKey, nil
	default:
		return "", s.ErrInvalid
	}
}

func restoreExpiry(value string) (time.Time, bool) {
	const token = `expiry-date="`
	start := strings.Index(value, token)
	if start < 0 {
		return time.Time{}, false
	}
	start += len(token)
	end := strings.IndexByte(value[start:], '"')
	if end < 0 {
		return time.Time{}, false
	}
	expiry, err := time.Parse(time.RFC1123, value[start:start+end])
	return expiry.UTC(), err == nil
}
