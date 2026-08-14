package parser

import (
	"encoding/binary"
	"hash/fnv"
	"os"
)

// fileStatTupleDigest computes an FNV-1a 64 digest over (size, mtime,
// ctime) tuples for the given files, prefixed with a per-provider domain
// separator so digests from different providers never collide. Missing or
// unreadable files are encoded as (0, 0, 0). An existing file without a
// reliable change-time makes the whole digest unverified (0), because its
// size and mtime alone cannot rule out a same-size, mtime-preserving rewrite.
func fileStatTupleDigest(sep byte, paths ...string) uint64 {
	return fileStatTupleDigestWithChangeTime(
		codexIndexChangeTime, sep, paths...,
	)
}

func fileStatTupleDigestWithChangeTime(
	changeTime func(string, os.FileInfo) (int64, bool),
	sep byte,
	paths ...string,
) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte{sep})
	var buf [24]byte
	for _, path := range paths {
		var size, mtime, ctime int64
		if path != "" {
			if info, err := os.Stat(path); err == nil {
				size = info.Size()
				mtime = info.ModTime().UnixNano()
				var verified bool
				ctime, verified = changeTime(path, info)
				if !verified {
					return 0
				}
			}
		}
		binary.LittleEndian.PutUint64(buf[:8], uint64(size))
		binary.LittleEndian.PutUint64(buf[8:16], uint64(mtime))
		binary.LittleEndian.PutUint64(buf[16:24], uint64(ctime))
		_, _ = h.Write(buf[:])
	}
	return h.Sum64()
}
