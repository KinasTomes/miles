package benchmarknative

import (
	s "github.com/KinasTomes/miles/internal/storage"
	"testing"
)

func TestInventoryRetainsNoncurrentVersionsAndRejectsPartialProof(t *testing.T) {
	v := s.PhysicalInventory{Complete: true, VersionScope: "all-retained", Objects: []s.PhysicalObject{
		{Key: "pack", VersionID: "new", Size: 30, Class: s.StorageClass("STANDARD"), Category: "pack", Current: true},
		{Key: "pack", VersionID: "old", Size: 70, Class: s.StorageClass("GLACIER"), Category: "pack"},
	}}
	got, err := Sum(Config{Bucket: "fixture"}, v)
	if err != nil || got.RetainedBytes != 100 || got.ByClass["GLACIER"] != 70 || got.SHA256 == "" {
		t.Fatal(got, err)
	}
	v.Complete = false
	if _, err := Sum(Config{}, v); err == nil {
		t.Fatal("partial inventory passed")
	}
	v.Complete = true
	v.Objects = append(v.Objects, v.Objects[0])
	if _, err := Sum(Config{}, v); err == nil {
		t.Fatal("duplicate version passed")
	}
}
