//go:build linux && cgo

package main

/*
#define _GNU_SOURCE
#include <stdlib.h>
#include <string.h>
#ifdef __GLIBC__
#include <malloc.h>
#include <stdio.h>

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
//
// The constructor runs before main because Go's runtime threads already own
// arenas by then. glibc latches its arena limit before a later mallopt can cap it.

enum {
	mallocArenaMax = 2,
	mallocTrimThreshold = 1 << 20
};

static int env_unset(const char *name) {
	const char *value = getenv(name);
	return value == NULL || value[0] == '\0';
}

// One-shot commands and sync-worker exit before fragmentation accumulates.
__attribute__((constructor)) static void tune_serve_malloc(int argc, char **argv, char **envp) {
	if (!(argc > 1 && strcmp(argv[1], "serve") == 0) &&
		!(!env_unset("AGENTSVIEW_MODE") && (argc < 2 || argv[1][0] == '-'))) return;
	if (env_unset("MALLOC_ARENA_MAX")) mallopt(M_ARENA_MAX, mallocArenaMax);
	if (env_unset("MALLOC_TRIM_THRESHOLD_")) mallopt(M_TRIM_THRESHOLD, mallocTrimThreshold);
}
#endif

static int malloc_arena_count(void) {
#ifdef __GLIBC__
	char *buf = NULL;
	size_t size = 0;
	FILE *stream = open_memstream(&buf, &size);
	if (stream == NULL) return -1;
	int result = malloc_info(0, stream);
	int closed = fclose(stream);
	int count = -1;
	if (result == 0 && closed == 0) {
		count = 0;
		for (char *p = buf; (p = strstr(p, "<heap nr=")) != NULL; p++) count++;
	}
	free(buf);
	return count;
#else
	return -1;
#endif
}
*/
import "C"

func mallocArenaCount() int { return int(C.malloc_arena_count()) }
