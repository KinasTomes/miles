package coldrunner

import (
	"context"
	"errors"
	"github.com/KinasTomes/miles/internal/benchmarkdata"
	"github.com/KinasTomes/miles/internal/benchmarknative"
	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/coordination"
	"github.com/KinasTomes/miles/internal/metadata/dynamodb"
	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"time"
)

// The logical response's StorageClass is not proof that its backing packs have
// transitioned. Resolve exact manifest -> chunk -> pack generations natively.
func nativeInventory(ctx context.Context, cfg aws.Config, c benchmarknative.Config, logicalBucket string, manifest *benchmarkdata.Manifest, expected string) (func(context.Context) (benchmarknative.Inventory, error), error) {
	if c.LogicalBucket != logicalBucket {
		return nil, errors.New("native logical bucket binding differs")
	}
	m, err := dynamodb.New(dynamodb.Options{API: ddb.NewFromConfig(cfg), Tables: c.Tables, MaxAttempts: 1,
		Authority: &dynamodb.SharedAuthority{Time: coordination.Chrony{Clock: clock.NewReal(), Reference: "169.254.169.123", RestartID: c.RunID}, Lifetime: time.Minute}})
	if err != nil {
		return nil, err
	}
	physical := s3.NewFromConfig(cfg)
	return func(ctx context.Context) (benchmarknative.Inventory, error) {
		out, err := benchmarknative.Collect(ctx, physical, c)
		if err != nil {
			return out, err
		}
		bucket, err := m.GetBucket(ctx, logicalBucket)
		if err != nil {
			return out, err
		}
		refs := map[s.PackRef]bool{}
		for _, file := range manifest.Files {
			head, err := m.GetObject(ctx, s.ObjectKey{Bucket: logicalBucket, BucketID: bucket.ID, Key: file.Path})
			if err != nil {
				return out, err
			}
			if head.Deleted || head.Size != int64(file.Bytes) {
				return out, s.ErrConflict
			}
			pages, err := m.GetManifest(ctx, head.ManifestID)
			if err != nil {
				return out, err
			}
			for _, page := range pages {
				for start := 0; start < len(page.Chunks); start += 100 {
					keys := []s.ChunkKey{}
					for _, chunk := range page.Chunks[start:min(start+100, len(page.Chunks))] {
						keys = append(keys, chunk.Key)
					}
					chunks, err := m.GetChunks(ctx, keys, true)
					if err != nil {
						return out, err
					}
					for _, key := range keys {
						chunk, ok := chunks[key]
						if !ok {
							return out, s.ErrNotFound
						}
						refs[chunk.Location.Pack] = true
					}
				}
			}
		}
		if len(refs) == 0 {
			return out, errors.New("no physical Cold payload packs")
		}
		for ref := range refs {
			found := false
			for _, object := range out.Native.Objects {
				if object.Category == "pack" && object.Pack == ref && object.Current {
					if string(object.Class) != expected {
						return out, errors.New("logical Cold class is not yet present on physical pack")
					}
					found = true
				}
			}
			if !found {
				return out, errors.New("logical manifest pack generation missing from native inventory")
			}
		}
		return out, nil
	}, nil
}
