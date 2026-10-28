package multipart

import "github.com/KinasTomes/miles/internal/multipart/core"

type crc struct{ algorithm string }

var crcs = map[string]crc{"CRC32": {"CRC32"}, "CRC32C": {"CRC32C"}, "CRC64NVME": {"CRC64NVME"}}

func (c crc) combine(a, b uint64, n int64) uint64 { return core.CombineCRC(c.algorithm, a, b, n) }
