package parser

import (
	"encoding/binary"
	"hash/fnv"
	"os"
)

// fileStatTupleDigest computes an FNV-1a 64 digest over (size, mtime,
// ctime, inode) tuples for the given files, prefixed with a per-provider
// domain separator so digests from different providers never collide.
// Missing or unreadable files are encoded as all zeros. An existing file
// without a reliable change-time or inode makes the whole digest unverified
// (0), because the remaining fields cannot rule out a same-size,
// mtime-preserving rewrite. The inode catches an atomic replacement whose
// timestamps land in the same clock tick as the original's, which coarse
// filesystem clocks allow. The device number is left out: a rename cannot
// cross filesystems, and some filesystems renumber devices across reboots
// or remounts, which would force a content check of every source.
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
	var buf [32]byte
	for _, path := range paths {
		var size, mtime, ctime int64
		var inode uint64
		if path != "" {
			if info, err := os.Stat(path); err == nil {
				size = info.Size()
				mtime = info.ModTime().UnixNano()
				var verified bool
				ctime, verified = changeTime(path, info)
				if !verified {
					return 0
				}
				inode, _ = sourceFileIdentityForPath(path, info)
				if inode == 0 {
					return 0
				}
			}
		}
		binary.LittleEndian.PutUint64(buf[:8], uint64(size))
		binary.LittleEndian.PutUint64(buf[8:16], uint64(mtime))
		binary.LittleEndian.PutUint64(buf[16:24], uint64(ctime))
		binary.LittleEndian.PutUint64(buf[24:32], inode)
		_, _ = h.Write(buf[:])
	}
	return h.Sum64()
}
