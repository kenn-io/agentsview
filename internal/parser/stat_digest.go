package parser

import (
	"encoding/binary"
	"hash/fnv"
	"os"
)

// fileStatTupleDigest computes an FNV-1a 64 digest over (size, mtime,
// ctime) tuples for the given files, prefixed with a per-provider domain
// separator so digests from different providers never collide. Missing or
// unreadable files are encoded as (0, 0, 0), and the change-time term
// degrades to 0 on platforms without a reliable ctime, keeping the tuple
// stable there — the same conventions as the Codebuff companion digest.
// The ctime term is what lets a matching digest rule out a same-size,
// same-mtime in-place rewrite: any content write bumps ctime even when
// mtime is carried over or falls in the same coarse granule.
func fileStatTupleDigest(sep byte, paths ...string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte{sep})
	var buf [24]byte
	for _, path := range paths {
		var size, mtime, ctime int64
		if path != "" {
			if info, err := os.Stat(path); err == nil {
				size = info.Size()
				mtime = info.ModTime().UnixNano()
				ctime, _ = codexIndexChangeTime(path, info)
			}
		}
		binary.LittleEndian.PutUint64(buf[:8], uint64(size))
		binary.LittleEndian.PutUint64(buf[8:16], uint64(mtime))
		binary.LittleEndian.PutUint64(buf[16:24], uint64(ctime))
		_, _ = h.Write(buf[:])
	}
	return h.Sum64()
}
