package write

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"os"

	"github.com/KinasTomes/miles/internal/storage"
)

// orderedRefs holds refs behind an unpublished pack without retaining the
// object body. Large queues spill to a temporary file and are replayed only
// after their canonical locations have been published.
type orderedRefs struct {
	domain string
	emit   func(storage.ChunkRef) error
	result *Result
	memory []waitingRef
	file   *os.File
	read   int64
	write  int64
}

type waitingRef struct {
	ref     storage.ChunkRef
	waitFor uint64
}

const maxMemoryRefs = 8192
const refRecordBytes = 52

func (q *orderedRefs) Add(ref storage.ChunkRef, waitFor uint64) error {
	item := waitingRef{ref: ref, waitFor: waitFor}
	if q.file == nil && len(q.memory) < maxMemoryRefs {
		q.memory = append(q.memory, item)
		return nil
	}
	if q.file == nil {
		file, err := os.CreateTemp("", "miles-write-refs-*")
		if err != nil {
			return err
		}
		q.file = file
		for _, old := range q.memory {
			if err := q.writeRef(old); err != nil {
				return err
			}
		}
		q.memory = nil
	}
	return q.writeRef(item)
}

func (q *orderedRefs) writeRef(item waitingRef) error {
	var record [refRecordBytes]byte
	binary.LittleEndian.PutUint64(record[:8], item.waitFor)
	binary.LittleEndian.PutUint64(record[8:16], uint64(item.ref.LogicalOffset))
	binary.LittleEndian.PutUint32(record[16:20], uint32(item.ref.RawBytes))
	if _, err := hex.Decode(record[20:], []byte(item.ref.Key.Hash)); err != nil {
		return fmt.Errorf("%w: queued chunk hash", storage.ErrInvalid)
	}
	n, err := q.file.WriteAt(record[:], q.write)
	if err != nil {
		return err
	}
	if n != len(record) {
		return io.ErrShortWrite
	}
	q.write += int64(n)
	return nil
}

func (q *orderedRefs) Drain(published uint64) error {
	if q.file == nil {
		count := 0
		for count < len(q.memory) && q.memory[count].waitFor <= published {
			if err := q.emitRef(q.memory[count].ref); err != nil {
				return err
			}
			count++
		}
		q.memory = q.memory[count:]
		return nil
	}
	for q.read < q.write {
		var record [refRecordBytes]byte
		if _, err := q.file.ReadAt(record[:], q.read); err != nil {
			return err
		}
		if binary.LittleEndian.Uint64(record[:8]) > published {
			return nil
		}
		ref := storage.ChunkRef{Key: storage.ChunkKey{Domain: q.domain, Hash: hex.EncodeToString(record[20:])},
			LogicalOffset: int64(binary.LittleEndian.Uint64(record[8:16])),
			RawBytes:      int64(binary.LittleEndian.Uint32(record[16:20]))}
		if err := q.emitRef(ref); err != nil {
			return err
		}
		q.read += refRecordBytes
	}
	if err := q.file.Truncate(0); err != nil {
		return err
	}
	q.read, q.write = 0, 0
	return nil
}

func (q *orderedRefs) emitRef(ref storage.ChunkRef) error {
	if err := q.emit(ref); err != nil {
		return err
	}
	q.result.Size += ref.RawBytes
	q.result.ChunkCount++
	return nil
}

func (q *orderedRefs) Close() {
	if q.file != nil {
		name := q.file.Name()
		_ = q.file.Close()
		_ = os.Remove(name)
	}
}

func (q *orderedRefs) Empty() bool {
	return len(q.memory) == 0 && q.read == q.write
}
