package benchmarknative

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"time"

	s "github.com/KinasTomes/miles/internal/storage"
	packs3 "github.com/KinasTomes/miles/internal/storage/s3"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Inventory includes current and noncurrent payload versions in the whole
// installation bucket. It does not mistake logical GET bytes for physical size.
type Inventory struct {
	At            time.Time           `json:"at"`
	Bucket        string              `json:"bucket"`
	RunID         string              `json:"run_id"`
	Slot          string              `json:"slot"`
	Source        string              `json:"source"`
	RetainedBytes int64               `json:"retained_bytes"`
	ByClass       map[string]int64    `json:"bytes_by_class"`
	ByCategory    map[string]int64    `json:"bytes_by_category"`
	SHA256        string              `json:"sha256"`
	Native        s.PhysicalInventory `json:"native"`
}

func Sum(c Config, v s.PhysicalInventory) (Inventory, error) {
	out := Inventory{At: time.Now().UTC(), Bucket: c.Bucket, RunID: c.RunID, Slot: c.Slot,
		Source: c.Source, ByClass: map[string]int64{}, ByCategory: map[string]int64{}, Native: v}
	if !v.Complete || v.VersionScope != "all-retained" {
		return out, errors.New("incomplete physical version inventory")
	}
	seen := map[string]bool{}
	for _, o := range v.Objects {
		id := o.Key + "\x00" + o.VersionID
		if o.Key == "" || o.VersionID == "" || o.Size < 0 || seen[id] || out.RetainedBytes > math.MaxInt64-o.Size {
			return out, errors.New("invalid/duplicate physical version")
		}
		seen[id] = true
		out.RetainedBytes += o.Size
		class := string(o.Class)
		if class == "" {
			class = "STANDARD"
		}
		out.ByClass[class] += o.Size
		out.ByCategory[o.Category] += o.Size
	}
	sort.Slice(out.Native.Objects, func(i, j int) bool {
		a, b := out.Native.Objects[i], out.Native.Objects[j]
		if a.Key == b.Key {
			return a.VersionID < b.VersionID
		}
		return a.Key < b.Key
	})
	body, err := json.Marshal(out.Native)
	if err != nil {
		return out, err
	}
	h := sha256.Sum256(body)
	out.SHA256 = hex.EncodeToString(h[:])
	return out, nil
}

// expectedOwnerAPI pins every page to the reviewed account.
type expectedOwnerAPI struct {
	client  *s3.Client
	account string
}

func (a expectedOwnerAPI) ListObjectVersions(ctx context.Context, in *s3.ListObjectVersionsInput, opts ...func(*s3.Options)) (*s3.ListObjectVersionsOutput, error) {
	in.ExpectedBucketOwner = aws.String(a.account)
	return a.client.ListObjectVersions(ctx, in, opts...)
}

func CheckBucket(ctx context.Context, client *s3.Client, c Config) error {
	if _, err := client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: &c.Bucket, ExpectedBucketOwner: &c.Account}); err != nil {
		return err
	}
	tags, err := client.GetBucketTagging(ctx, &s3.GetBucketTaggingInput{Bucket: &c.Bucket, ExpectedBucketOwner: &c.Account})
	if err != nil {
		return err
	}
	actual := map[string]string{}
	for _, t := range tags.TagSet {
		actual[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	for k, v := range c.Tags {
		if actual[k] != v {
			return errors.New("native bucket ownership tag mismatch")
		}
	}
	return nil
}

func Collect(ctx context.Context, client *s3.Client, c Config) (Inventory, error) {
	var zero Inventory
	if err := CheckBucket(ctx, client, c); err != nil {
		return zero, err
	}
	versioning, err := client.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: &c.Bucket, ExpectedBucketOwner: &c.Account})
	if err != nil || string(versioning.Status) != "Enabled" {
		return zero, errors.New("physical bucket must be versioned")
	}
	mpu, err := client.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: &c.Bucket, ExpectedBucketOwner: &c.Account, MaxUploads: aws.Int32(1)})
	if err != nil {
		return zero, err
	}
	if len(mpu.Uploads) > 0 || aws.ToBool(mpu.IsTruncated) {
		return zero, errors.New("unaccounted physical multipart storage")
	}
	provider := packs3.VersionInventory{Client: expectedOwnerAPI{client, c.Account}, Bucket: c.Bucket}
	first, err := provider.CollectInventory(ctx, 100000)
	if err != nil {
		return zero, err
	}
	a, err := Sum(c, first)
	if err != nil {
		return zero, err
	}
	second, err := provider.CollectInventory(ctx, 100000)
	if err != nil {
		return zero, err
	}
	b, err := Sum(c, second)
	if err != nil {
		return zero, err
	}
	if a.SHA256 != b.SHA256 {
		return zero, errors.New("physical inventory changed during collection; quiesce and reconcile")
	}
	return b, nil
}
