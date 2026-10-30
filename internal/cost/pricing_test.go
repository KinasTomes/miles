package cost

import (
	"math"
	"os"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/pricing"
	"github.com/KinasTomes/miles/internal/storage"
)

func catalog(t *testing.T) *pricing.Catalog {
	t.Helper()
	b, err := os.ReadFile("../../configs/pricing/pricing.ap-southeast-1.example.json")
	if err != nil {
		t.Fatal(err)
	}
	c, err := pricing.Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestExactPricingAggregatesBeforeTiersAndKeepsMissingPrices(t *testing.T) {
	c := catalog(t)
	r, err := Evaluate(c, []Usage{{"s3.storage.standard", "GB-Mo", "51200"}, {"s3.storage.standard", "GB-Mo", "20"}, {"s3.restore_bulk_gb", "GB", "10"}, {"ec2.compute", "Hrs", "1"}, {"absent.rate", "Requests", "1"}})
	if err != nil {
		t.Fatal(err)
	}
	if r.Subtotal != "32012/25" || len(r.Lines) != 2 || len(r.Unpriced) != 2 || r.CompleteTotalUSD != nil || r.AsOf != c.AsOf || r.Sources["api"].URL != c.Sources["api"].URL {
		t.Fatalf("wrong subtotal/provenance/coverage: %+v", r)
	}
	for _, u := range []Usage{{"s3.put_standard", "1000 Requests", "1"}, {"s3.put_standard", "Requests", "-1"}, {"s3.put_standard", "Requests", "1e3"}, {"s3.put_standard", "Requests", "1/0"}} {
		if _, err := Evaluate(c, []Usage{u}); err == nil {
			t.Fatal("invalid usage priced", u)
		}
	}
	r, err = Evaluate(c, []Usage{{"ddb.storage", "GB-Mo", "1"}, {"s3.put_standard", "Requests", "3"}})
	if err != nil || r.Subtotal != "57003/200000" {
		t.Fatal("gross normalization or per-request price changed", r, err)
	}
}

func TestStorageIntegratesByteTimeWithExplicitUnits(t *testing.T) {
	u, err := StorageUsage(storage.Standard, 1<<30, 15*24*time.Hour, 1<<30, 30*24*time.Hour)
	if err != nil || u.Quantity != "1/2" {
		t.Fatal(u, err)
	}
	r, err := Evaluate(catalog(t), []Usage{u})
	if err != nil || r.Subtotal != "1/80" {
		t.Fatal(r, err)
	}
	if _, err = StorageUsage(storage.Standard, 1, time.Second, 0, time.Hour); err == nil {
		t.Fatal("missing units accepted")
	}
}

func TestCounterUnitsRatiosAndRouteIsolation(t *testing.T) {
	s := Snapshot{Logical: 100, UniqueRaw: 20, UniqueStored: 10, Physical: 40}
	if s.Validate() != nil || *s.Ratios().Dedup != 5 || *s.Ratios().Compression != 2 || *s.Ratios().TotalReduction != 2.5 {
		t.Fatal(s)
	}
	s.Physical = 80 // Revival can add dead Cold bytes without changing unique identities.
	if *s.Ratios().Dedup != 5 || *s.Ratios().TotalReduction != 1.25 {
		t.Fatal(s)
	}
	if (Snapshot{}).Ratios().Dedup != nil {
		t.Fatal("zero denominator produced a ratio")
	}
	c := WriteCounters{}
	if err := c.Add(WriteCounters{ChunkOccurrences: 5, DupOccurrences: 4, RevivedDistinct: 3, RevivedBytes: 10}); err != nil {
		t.Fatal("independent counters treated as a partition", err)
	}
	traffic := map[Route]uint64{GatewayClient: 100, S3Gateway: 0}
	if err := AddTraffic(traffic, map[Route]uint64{GatewayClient: 100}); err != nil || traffic[GatewayClient] != 200 || traffic[S3Gateway] != 0 {
		t.Fatal(traffic, err)
	}
	if AddTraffic(traffic, map[Route]uint64{"unknown": 1}) == nil {
		t.Fatal("unknown network route accepted")
	}
	if AddTraffic(nil, map[Route]uint64{GatewayClient: 1}) == nil {
		t.Fatal("nil accumulator accepted")
	}
	traffic[GatewayClient] = math.MaxUint64
	if AddTraffic(traffic, map[Route]uint64{GatewayClient: 1}) == nil {
		t.Fatal("byte counter overflow")
	}
}
