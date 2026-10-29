package storage

import "testing"

func TestBucketPolicyRecoveryIdentity(t *testing.T) {
	for _, id := range []string{"bucket-id", "bucket/other", "hải"} {
		got := BucketPolicyEntityID(id)
		if !validPolicyEntityID(got) || got == "global-policy" {
			t.Fatal(id, got)
		}
	}
	for _, id := range []string{"bucket-cache/", "bucket-cache/ff", "bucket-cache/6A", "storage/op/id", "bucket-cache/xyz", "bucket-cache/61/extra"} {
		if validPolicyEntityID(id) {
			t.Fatal("accepted invalid identity", id)
		}
	}
}
