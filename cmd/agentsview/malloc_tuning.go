package main

// The Go soft memory limit only bounds the Go heap. The serve daemon's
// other large allocator is glibc malloc, which the cgo SQLite driver
// calls on every query. glibc gives each thread its own arena, so with
// the daemon's ~20 threads a transient allocation can land in any of
// them; freed chunks stay resident, and fragmentation across arenas
// keeps the high-water mark of every burst. That is what makes the
// daemon's RSS ratchet from a couple of hundred megabytes at startup
// past a gigabyte after a day of use. Capping the arena count and
// lowering the trim threshold so freed top-of-heap memory is returned
// to the OS holds RSS flat with no measurable query latency cost.
const (
	mallocArenaMax      = 2
	mallocTrimThreshold = 1 << 20
)

// mallocTuningPlan records which glibc knobs the daemon should set.
type mallocTuningPlan struct {
	SetArenaMax      bool
	SetTrimThreshold bool
}

// planMallocTuning decides which knobs to install given an environment
// lookup. glibc reads MALLOC_ARENA_MAX and MALLOC_TRIM_THRESHOLD_ at
// startup and has already applied them by the time this runs, so an
// operator who set either one keeps that value; the knobs are
// independent, so setting one does not suppress the other.
func planMallocTuning(getenv func(string) string) mallocTuningPlan {
	return mallocTuningPlan{
		SetArenaMax:      getenv("MALLOC_ARENA_MAX") == "",
		SetTrimThreshold: getenv("MALLOC_TRIM_THRESHOLD_") == "",
	}
}
