package s3

import (
	"context"
	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"strings"
)

// VersionInventoryAPI is separate from PackStore: serving a chunk never needs
// listing permission. The configured bucket is the complete physical namespace.
type VersionInventoryAPI interface {
	ListObjectVersions(context.Context, *awss3.ListObjectVersionsInput, ...func(*awss3.Options)) (*awss3.ListObjectVersionsOutput, error)
}
type VersionInventory struct {
	Client VersionInventoryAPI
	Bucket string
}

var _ s.PhysicalInventorySource = VersionInventory{}

func (v VersionInventory) CollectInventory(ctx context.Context, limit int) (s.PhysicalInventory, error) {
	out := s.PhysicalInventory{VersionScope: "all-retained", Objects: []s.PhysicalObject{}}
	if v.Client == nil || v.Bucket == "" || limit < 1 || limit > 1000000 {
		return out, s.ErrInvalid
	}
	var key, version *string
	for pages := 0; pages < limit+16; pages++ {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		res, err := v.Client.ListObjectVersions(ctx, &awss3.ListObjectVersionsInput{Bucket: aws.String(v.Bucket), KeyMarker: key, VersionIdMarker: version, MaxKeys: aws.Int32(int32(min(1000, limit+1-len(out.Objects))))})
		if err != nil {
			return out, mapError(err)
		}
		if res == nil {
			return out, s.ErrUnavailable
		}
		for _, item := range res.Versions {
			if len(out.Objects) >= limit || item.Key == nil || item.Size == nil || *item.Size < 0 || item.IsLatest == nil {
				return out, s.ErrUnavailable
			}
			o := s.PhysicalObject{Key: *item.Key, VersionID: aws.ToString(item.VersionId), Size: *item.Size, Current: *item.IsLatest, Category: "other"}
			parts := strings.Split(o.Key, "/")
			if len(parts) == 4 && (parts[0] == "packs" || parts[0] == "packs-index") {
				o.Pack = s.PackRef{ID: parts[2], Generation: strings.TrimSuffix(parts[3], ".idx")}
				pk, sk, e := keys(o.Pack)
				if e == nil && o.Key == pk {
					o.Category = "pack"
				}
				if e == nil && o.Key == sk {
					o.Category = "sidecar"
				}
			} else if strings.HasPrefix(o.Key, "journal/") {
				o.Category = "journal"
			} else if strings.HasPrefix(o.Key, "checkpoint/") || strings.HasPrefix(o.Key, "checkpoints/") || strings.HasPrefix(o.Key, "recovery/") {
				o.Category = "recovery"
			}
			out.Objects = append(out.Objects, o)
		}
		// Delete markers have no payload; still advance through marker-only pages.
		if !aws.ToBool(res.IsTruncated) {
			out.Complete = true
			return out, nil
		}
		if res.NextKeyMarker == nil || aws.ToString(key) == aws.ToString(res.NextKeyMarker) && aws.ToString(version) == aws.ToString(res.NextVersionIdMarker) {
			return out, s.ErrUnavailable
		}
		key, version = res.NextKeyMarker, res.NextVersionIdMarker
	}
	return out, s.ErrUnavailable
}
