package dynamodb

import (
	"reflect"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/jobs"
)

func TestNodeCacheNativeRoundTripAndOldRecord(t *testing.T) {
	n := jobs.Node{ID: "node", Cache: jobs.CacheSample{SampledAt: time.Now().UTC(), Hits: 2, Misses: 3, Lookups: 6, Errors: 2, Puts: 1}}
	item, err := nativeItem(n)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeItem[jobs.Node](item)
	if err != nil || !reflect.DeepEqual(got, n) {
		t.Fatal(got, err)
	}
	delete(item, "cache")
	got, err = decodeItem[jobs.Node](item)
	if err != nil || !got.Cache.SampledAt.IsZero() {
		t.Fatal("old node must remain readable without sample", got, err)
	}
}
