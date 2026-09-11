package main

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
