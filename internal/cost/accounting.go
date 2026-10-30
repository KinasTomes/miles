// Package cost evaluates supplied usage, never user billing or execution authority.
package cost

import (
	"errors"
	"math"
)

// Snapshot counts live heads and each live domain/hash once. Physical includes
// retained/dead/control bytes; upload traffic is a separate cumulative counter.
type Snapshot struct {
	Logical      uint64 `json:"bytes_logical"`
	UniqueRaw    uint64 `json:"bytes_unique_raw"`
	UniqueStored uint64 `json:"bytes_unique_stored"`
	Physical     uint64 `json:"bytes_physical"`
}

func (s Snapshot) Validate() error {
	if s.UniqueRaw > s.Logical || s.UniqueStored > s.UniqueRaw || s.UniqueStored > s.Physical {
		return errors.New("inconsistent live/physical byte snapshot")
	}
	return nil
}

type Ratios struct {
	Dedup          *float64 `json:"dedup_ratio"`
	Compression    *float64 `json:"compression_ratio"`
	TotalReduction *float64 `json:"total_reduction"`
}

func (s Snapshot) Ratios() Ratios {
	ratio := func(n, d uint64) *float64 {
		if d == 0 {
			return nil
		}
		v := float64(n) / float64(d)
		return &v
	}
	return Ratios{ratio(s.Logical, s.UniqueRaw), ratio(s.UniqueRaw, s.UniqueStored), ratio(s.Logical, s.Physical)}
}

// Dup counts occurrences; revival counts distinct identities PER request.
// They are independent observations, not a partition of ChunkOccurrences.
type WriteCounters struct {
	ChunkOccurrences uint64 `json:"chunk_occurrences"`
	DupOccurrences   uint64 `json:"dup_occurrences"`
	RevivedDistinct  uint64 `json:"revived_distinct_chunks"`
	RevivedBytes     uint64 `json:"revived_bytes_stored"`
}

func (c *WriteCounters) Add(delta WriteCounters) error {
	if delta.DupOccurrences > delta.ChunkOccurrences || delta.RevivedDistinct > delta.ChunkOccurrences {
		return errors.New("invalid write counter units")
	}
	next := *c
	for _, p := range []struct{ dst, src *uint64 }{{&next.ChunkOccurrences, &delta.ChunkOccurrences}, {&next.DupOccurrences, &delta.DupOccurrences}, {&next.RevivedDistinct, &delta.RevivedDistinct}, {&next.RevivedBytes, &delta.RevivedBytes}} {
		if *p.src > math.MaxUint64-*p.dst {
			return errors.New("write counter overflow")
		}
		*p.dst += *p.src
	}
	*c = next
	return nil
}

type Route string

const (
	ClientGateway Route = "client_to_gateway"
	GatewayClient Route = "gateway_to_client"
	GatewayS3     Route = "gateway_to_s3_upload"
	S3Gateway     Route = "s3_to_gateway_download"
	ControlPlane  Route = "metadata_control_plane"
	InterNode     Route = "inter_node"
	ClientS3      Route = "client_to_s3"
	S3Client      Route = "s3_to_client"
)

func (r Route) Valid() bool {
	switch r {
	case ClientGateway, GatewayClient, GatewayS3, S3Gateway, ControlPlane, InterNode, ClientS3, S3Client:
		return true
	}
	return false
}

// AddTraffic never infers a route from cache hits or logical payload size.
func AddTraffic(total map[Route]uint64, delta map[Route]uint64) error {
	if total == nil {
		return errors.New("traffic accumulator is nil")
	}
	for route, n := range delta {
		if !route.Valid() || n > math.MaxUint64-total[route] {
			return errors.New("invalid traffic route or byte overflow")
		}
	}
	for route, n := range delta {
		total[route] += n
	}
	return nil
}
