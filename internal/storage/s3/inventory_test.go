package s3

import (
	"context"
	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"testing"
)

type inventoryPages struct{ calls int }

func (m *inventoryPages) ListObjectVersions(_ context.Context, in *awss3.ListObjectVersionsInput, _ ...func(*awss3.Options)) (*awss3.ListObjectVersionsOutput, error) {
	m.calls++
	if m.calls == 1 {
		return &awss3.ListObjectVersionsOutput{IsTruncated: aws.Bool(true), NextKeyMarker: aws.String("marker-only")}, nil
	}
	if aws.ToString(in.KeyMarker) != "marker-only" {
		panic("lost pagination")
	}
	return &awss3.ListObjectVersionsOutput{Versions: []types.ObjectVersion{{Key: aws.String("packs/aa/aa/g"), VersionId: aws.String("new"), Size: aws.Int64(80), IsLatest: aws.Bool(true)}, {Key: aws.String("packs/aa/aa/g"), VersionId: aws.String("old"), Size: aws.Int64(70), IsLatest: aws.Bool(false)}, {Key: aws.String("orphan"), Size: aws.Int64(5), IsLatest: aws.Bool(true)}}}, nil
}
func TestInventoryRetainsVersionsAndMarkerPagination(t *testing.T) {
	m := &inventoryPages{}
	v := VersionInventory{Client: m, Bucket: "physical"}
	out, err := v.CollectInventory(context.Background(), 10)
	if err != nil || !out.Complete || len(out.Objects) != 3 || out.Objects[0].Category != "pack" || out.Objects[1].Current {
		t.Fatal(out, err)
	}
	m.calls = 0
	if _, err = v.CollectInventory(context.Background(), 2); err == nil {
		t.Fatal("budget exceeded but inventory marked complete")
	}
}
