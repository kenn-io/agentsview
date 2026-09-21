//go:build !linux || !cgo

package main

// applyServeMallocTuning is a no-op away from glibc: mallopt is a
// glibc extension, and a non-cgo build has no C allocator to tune.
func applyServeMallocTuning() {}
