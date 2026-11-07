package app

import (
	"github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/coordination"
	"github.com/KinasTomes/miles/internal/object"
	"github.com/KinasTomes/miles/internal/readpath"
	"github.com/KinasTomes/miles/internal/storage"
	"time"
)

// objectRuntime uses the same engine/read admission in all profiles. Only
// metadata, authority, policy snapshots and AWS providers differ by deployment.
func objectReadAdmission() (*readpath.Admission, error) {
	node := readpath.Budget{
		Capacity: readpath.Cost{ChunkSlots: 64, StoredBytes: 256 << 20, IOSlots: 64, IOBytes: 256 << 20, RawBytes: 256 << 20, WorkspaceBytes: 512 << 20, CopyBytes: 512 << 20},
		Prefetch: readpath.Cost{ChunkSlots: 32, StoredBytes: 128 << 20, IOSlots: 32, IOBytes: 128 << 20, RawBytes: 128 << 20, WorkspaceBytes: 256 << 20, CopyBytes: 256 << 20},
	}
	return readpath.NewAdmission(readpath.Limits{Node: readpath.ScopeLimits{ActiveRequests: 64, Budget: node}, Bucket: readpath.ScopeLimits{ActiveRequests: 64, Budget: node}})
}

func objectRuntime(o object.Options, metadata storage.MetadataStore, packs storage.PackStore, gate *coordination.WarmGate, global config.GlobalSnapshot, admission *readpath.Admission, metrics *readpath.WarmMetrics) (*object.Service, error) {
	request := readpath.Budget{
		Capacity: readpath.Cost{ChunkSlots: 8, StoredBytes: 32 << 20, IOSlots: 8, IOBytes: 32 << 20, RawBytes: 32 << 20, WorkspaceBytes: 128 << 20, CopyBytes: 128 << 20},
		Prefetch: readpath.Cost{ChunkSlots: 4, StoredBytes: 16 << 20, IOSlots: 4, IOBytes: 16 << 20, RawBytes: 16 << 20, WorkspaceBytes: 64 << 20, CopyBytes: 64 << 20},
	}
	if admission == nil {
		var err error
		admission, err = objectReadAdmission()
		if err != nil {
			return nil, err
		}
	}
	if metrics == nil {
		metrics = new(readpath.WarmMetrics)
	}
	reader, err := readpath.NewWarmReader(readpath.WarmOptions{
		Metadata: metadata, Packs: packs, Cache: o.Cache, Admission: admission, Gate: gate, Clock: o.Clock, Global: global, Metrics: metrics,
		// net/http read/write, TLS record and SDK stream buffers; no custom buffering.
		// Resident connection pools and runtime/RSS headroom are outside this budget.
		TransportBytes: 128 << 10, MaxManifestPages: 1024, MaxManifestChunks: 65536,
	})
	if err != nil {
		return nil, err
	}
	o.Reader = reader
	o.Budget = request
	o.MaxOutstanding = 4
	o.RequestTimeout = 30 * time.Minute
	return object.New(o)
}
