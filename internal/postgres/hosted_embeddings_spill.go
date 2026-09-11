package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// occurrenceCounter keeps exact UUID identity. Hashes route records, never
// substitute for key equality. Spill uses one open file and one record buffer;
// repeated UUIDs update the existing fixed-width count instead of appending.
type occurrenceCounter struct {
	memory                                             map[string]uint64
	memoryBytes, maxMemory, maxBytes, bytes, readBytes int64
	dir                                                string
}

func newOccurrenceCounter(memory, spill int64) *occurrenceCounter {
	return &occurrenceCounter{memory: map[string]uint64{}, maxMemory: memory, maxBytes: spill}
}
func (c *occurrenceCounter) close() error {
	if c.dir != "" {
		return os.RemoveAll(c.dir)
	}
	return nil
}
func (c *occurrenceCounter) next(ctx context.Context, key string) (int, error) {
	if e := ctx.Err(); e != nil {
		return 0, e
	}
	if key == "" {
		return 0, nil
	}
	if len(key) > 65536 {
		return 0, ErrHostedEmbeddingWorkLimit
	}
	if c.dir == "" {
		if n, ok := c.memory[key]; ok {
			c.memory[key] = n + 1
			return int(n + 1), nil
		}
		if c.memoryBytes+int64(len(key)+64) <= c.maxMemory {
			c.memory[key] = 1
			c.memoryBytes += int64(len(key) + 64)
			return 1, nil
		}
		dir, e := os.MkdirTemp("", "agentsview-embedding-")
		if e != nil {
			return 0, e
		}
		c.dir = dir
		for k, n := range c.memory {
			if _, e = c.disk(ctx, k, n); e != nil {
				return 0, e
			}
		}
		c.memory = nil
		c.memoryBytes = 0
	}
	return c.disk(ctx, key, 1)
}
func (c *occurrenceCounter) disk(ctx context.Context, key string, increment uint64) (int, error) {
	hash := sha256.Sum256([]byte(key))
	path := filepath.Join(c.dir, fmt.Sprintf("%02x", hash[0]))
	f, e := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0600)
	if e != nil {
		return 0, e
	}
	defer f.Close()
	var pos int64
	var header [12]byte
	for {
		if e = ctx.Err(); e != nil {
			return 0, e
		}
		_, e = io.ReadFull(f, header[:])
		if e == io.EOF {
			break
		}
		if e != nil {
			return 0, e
		}
		length := binary.LittleEndian.Uint32(header[:4])
		if length > 65536 {
			return 0, ErrHostedEmbeddingWorkLimit
		}
		c.readBytes += int64(length) + 12
		if c.readBytes > 1<<30 {
			return 0, ErrHostedEmbeddingWorkLimit
		}
		b := make([]byte, length)
		if _, e = io.ReadFull(f, b); e != nil {
			return 0, e
		}
		if string(b) == key {
			n := binary.LittleEndian.Uint64(header[4:]) + increment
			binary.LittleEndian.PutUint64(header[4:], n)
			_, e = f.WriteAt(header[4:], pos+4)
			return int(n), e
		}
		pos += int64(length) + 12
	}
	size := int64(len(key)) + 12
	if c.bytes+size > c.maxBytes {
		return 0, ErrHostedEmbeddingWorkLimit
	}
	binary.LittleEndian.PutUint32(header[:4], uint32(len(key)))
	binary.LittleEndian.PutUint64(header[4:], increment)
	if _, e = f.Write(header[:]); e != nil {
		return 0, e
	}
	if _, e = f.WriteString(key); e != nil {
		return 0, e
	}
	c.bytes += size
	return int(increment), nil
}
