package s3

import (
	"context"
	"sort"

	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
)

func (p *Store) ListSweepVersions(ctx context.Context, ref s.PackRef) ([]s.SweepVersion, error) {
	packKey, sidecarKey, e := keys(ref)
	if e != nil {
		return nil, e
	}
	client, ok := p.client.(VersionInventoryAPI)
	if !ok {
		return nil, s.ErrUnavailable
	}
	var out []s.SweepVersion
	// The entire exact-key version set has to fit the bounded intent.
	// A truncated page (including markers or same-prefix keys) cannot
	// authorize partial deletion.
	for _, target := range []struct{ category, key string }{{"pack", packKey}, {"sidecar", sidecarKey}} {
		page, err := client.ListObjectVersions(ctx, &awss3.ListObjectVersionsInput{Bucket: aws.String(p.bucket), Prefix: aws.String(target.key), MaxKeys: aws.Int32(128)})
		if err != nil {
			return nil, mapError(err)
		}
		if page == nil || aws.ToBool(page.IsTruncated) {
			return nil, s.ErrUnavailable
		}
		for _, version := range page.Versions {
			if version.Key == nil {
				return nil, s.ErrUnavailable
			}
			if *version.Key == target.key {
				out = append(out, s.SweepVersion{Category: target.category, VersionID: aws.ToString(version.VersionId)})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Category != out[j].Category {
			return out[i].Category < out[j].Category
		}
		return out[i].VersionID < out[j].VersionID
	})
	if !s.ValidSweepVersions(out) {
		return nil, s.ErrUnavailable
	}
	return out, nil
}

func (p *Store) DeleteSweepVersion(ctx context.Context, ref s.PackRef, v s.SweepVersion) error {
	if !s.ValidSweepVersions([]s.SweepVersion{v}) {
		return s.ErrInvalid
	}
	key, sidecar, e := keys(ref)
	if e != nil {
		return e
	}
	if v.Category == "sidecar" {
		key = sidecar
	}
	if e = s.RequirePhysicalDispatch(ctx); e != nil {
		return e
	}
	_, e = p.client.DeleteObject(ctx, &awss3.DeleteObjectInput{Bucket: aws.String(p.bucket), Key: aws.String(key), VersionId: aws.String(v.VersionID)}, func(o *awss3.Options) { o.Retryer = aws.NopRetryer{} })
	return mapError(e)
}
