package simulator

import (
	"context"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/cost"
)

func TestComparePoliciesDirectS3ExcludesGatewayTraffic(t *testing.T) {
	dayNS := int64(24 * time.Hour)
	baseTrace, catalog := fixture(t, "cloud-boundary")

	oneGiB := uint64(1024 * 1024 * 1024)
	trace := baseTrace
	trace.Name = "direct-traffic-isolation"
	trace.DurationNS = 30 * dayNS
	trace.Events = []Event{
		{
			AtNS: 0,
			Snapshot: &cost.Snapshot{
				Logical:  oneGiB,
				Physical: oneGiB,
			},
			Traffic: map[cost.Route]uint64{
				cost.ClientGateway: oneGiB,
				cost.GatewayClient: oneGiB,
				cost.GatewayS3:     oneGiB,
				cost.S3Gateway:     500,
				cost.InterNode:     100,
			},
		},
	}

	res, err := ComparePolicies(context.Background(), trace, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}

	directPol := res.Policies[0]
	if directPol.Breakdown == nil {
		t.Fatal("expected breakdown")
	}
	tb := directPol.Breakdown.TrafficBytes
	if tb["gateway_to_s3_upload"] != 0 {
		t.Fatalf("direct_s3 must not have gateway_to_s3_upload, got %d", tb["gateway_to_s3_upload"])
	}
	if tb["s3_to_gateway_download"] != 0 {
		t.Fatalf("direct_s3 must not have s3_to_gateway_download, got %d", tb["s3_to_gateway_download"])
	}
	if tb["inter_node"] != 0 {
		t.Fatalf("direct_s3 must not have inter_node, got %d", tb["inter_node"])
	}
	if tb["client_to_s3"] != oneGiB {
		t.Fatalf("expected client_to_s3 = 1 GiB, got %d", tb["client_to_s3"])
	}
	if tb["s3_to_client"] != oneGiB {
		t.Fatalf("expected s3_to_client = 1 GiB, got %d", tb["s3_to_client"])
	}
}
