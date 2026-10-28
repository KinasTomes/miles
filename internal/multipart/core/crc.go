package core

import "encoding/binary"

// crc is a reflected CRC of API v2 2.7 with init and xorout all ones, as
// hash/crc32 and hash/crc64 compute it. poly is the reflected polynomial.
type crc struct {
	poly  uint64
	width uint // bits: 32 or 64
}

// crcs are the algorithms with a FULL_OBJECT checksum.
var crcs = map[string]crc{
	"CRC32":     {0xedb88320, 32},
	"CRC32C":    {0x82f63b78, 32},
	"CRC64NVME": {0x9a6c9329ac4bc9b5, 64},
}

// value reads a big-endian digest of c.
func (c crc) value(d []byte) uint64 {
	if c.width == 32 {
		return uint64(binary.BigEndian.Uint32(d))
	}
	return binary.BigEndian.Uint64(d)
}

// bytes is the big-endian digest of v.
func (c crc) bytes(v uint64) []byte {
	if c.width == 32 {
		return binary.BigEndian.AppendUint32(nil, uint32(v))
	}
	return binary.BigEndian.AppendUint64(nil, v)
}

// combine is the CRC of A followed by B from crcA, crcB and len(B), as zlib's
// crc32_combine: crc(A||B) = crcA * x^(8*lenB) mod P xor crcB. It costs
// O(log lenB) polynomial products, not a pass over lenB zero bytes.
func (c crc) combine(crcA, crcB uint64, lenB int64) uint64 {
	return c.mult(c.xpow8n(uint64(lenB)), crcA) ^ crcB
}

// mult is a*b mod P with polynomials in reflected order: the top bit of
// width is x^0.
func (c crc) mult(a, b uint64) uint64 {
	var p uint64
	for m := uint64(1) << (c.width - 1); m != 0; m >>= 1 {
		if a&m != 0 {
			p ^= b
		}
		if b&1 != 0 {
			b = b>>1 ^ c.poly
		} else {
			b >>= 1
		}
	}
	return p
}

// xpow8n is x^(8n) mod P by squaring.
func (c crc) xpow8n(n uint64) uint64 {
	p := uint64(1) << (c.width - 1)  // x^0
	sq := uint64(1) << (c.width - 9) // x^8
	for ; n != 0; n >>= 1 {
		if n&1 != 0 {
			p = c.mult(sq, p)
		}
		sq = c.mult(sq, sq)
	}
	return p
}

func CombineCRC(algorithm string, a, b uint64, n int64) uint64 {
	return crcs[algorithm].combine(a, b, n)
}
