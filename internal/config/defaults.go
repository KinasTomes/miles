package config

import "github.com/KinasTomes/miles/internal/storage"

// DefaultGlobalPolicy materializes W1 Clock §4.2 / Architecture §19.1 only.
// Defaults are for bootstrap, never implicit repair of stored snapshots/patches.
// Call PolicyValidator before admitting any configuration for execution.
func DefaultGlobalPolicy() GlobalPolicy {
	return GlobalPolicy{
		Tiering: TieringPolicy{
			Enabled: true, WarmStorageClass: storage.Standard, ColdStorageClass: storage.GlacierIR,
			ColdAfterDays: 30, MinPackLiveRatio: Decimal{digits: "8", exponent: -1},
			AutoRestore: true, AutoRestoreDays: 1, AutoRestoreTier: "Standard",
		},
		Compression: CompressionPolicy{Enabled: true, Codec: "zstd", Level: 3, MinSavingRatio: Decimal{digits: "1", exponent: -1}},
		Chunking:    ChunkingPolicy{MinBytes: 262144, AvgBytes: 1048576, MaxBytes: 4194304},
	}
}

func DefaultCachePolicy() CachePolicy { return CachePolicy{AdmitOnWrite: true, ReadaheadChunks: 4} }
