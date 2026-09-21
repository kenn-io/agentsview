//go:build linux && cgo

package main

/*
#include <stdlib.h>
#ifdef __GLIBC__
#include <malloc.h>
#endif

static void set_malloc_arena_max(int value) {
#ifdef __GLIBC__
	mallopt(M_ARENA_MAX, value);
#endif
}

static void set_malloc_trim_threshold(int value) {
#ifdef __GLIBC__
	mallopt(M_TRIM_THRESHOLD, value);
#endif
}
*/
import "C"

import "os"

// The Go soft memory limit only bounds the Go heap. The serve daemon's
// other large allocator is glibc malloc, which the cgo SQLite driver
// calls on every query. glibc gives each thread its own arena, so with
// the daemon's ~20 threads a transient allocation can land in any of
// them; freed chunks stay resident, and fragmentation across arenas
// keeps the high-water mark of every burst. That is what makes the
// daemon's RSS ratchet from a couple of hundred megabytes at startup
// past a gigabyte after a day of use.
//
// The trim threshold is pinned rather than lowered. glibc's static
// default is 128 KiB, but by default the threshold is dynamic: freeing
// a block larger than the current mmap threshold raises the mmap
// threshold to that size (up to 32 MiB) and the trim threshold to twice
// it, so a daemon that has freed one large query result or parse stops
// trimming until 64 MiB of free memory sits at the top of the heap.
// Setting M_TRIM_THRESHOLD explicitly disables that adjustment: the
// mmap threshold stays at 128 KiB so large transients are mmapped and
// returned to the OS on free, and the top of the heap is trimmed once
// 1 MiB is free. 1 MiB is the measured value that held RSS flat under
// the usage and session load in #1585 with no measurable query latency
// cost; the static 128 KiB default would trim more often for no
// observed RSS gain.
const (
	mallocArenaMax      = 2
	mallocTrimThreshold = 1 << 20
)

// applyServeMallocTuning caps the glibc arena count and trim threshold
// for the long-running serve daemon. Other Linux C libraries get a no-op.
// Only the daemon is tuned:
// one-shot CLI commands exit before fragmentation can accumulate, and
// the resync worker child returns everything to the OS when it exits,
// so restricting its arenas would only slow the rebuild down.
func applyServeMallocTuning() {
	plan := planMallocTuning(os.Getenv)
	if plan.SetArenaMax {
		C.set_malloc_arena_max(C.int(mallocArenaMax))
	}
	if plan.SetTrimThreshold {
		C.set_malloc_trim_threshold(C.int(mallocTrimThreshold))
	}
}
