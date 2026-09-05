//go:build linux && cgo

package main

/*
#include <malloc.h>
*/
import "C"

import "os"

// applyServeMallocTuning caps the glibc arena count and trim threshold
// for the long-running serve daemon. Only the daemon is tuned:
// one-shot CLI commands exit before fragmentation can accumulate, and
// the resync worker child returns everything to the OS when it exits,
// so restricting its arenas would only slow the rebuild down.
func applyServeMallocTuning() {
	plan := planMallocTuning(os.Getenv)
	if plan.SetArenaMax {
		C.mallopt(C.M_ARENA_MAX, C.int(mallocArenaMax))
	}
	if plan.SetTrimThreshold {
		C.mallopt(C.M_TRIM_THRESHOLD, C.int(mallocTrimThreshold))
	}
}
