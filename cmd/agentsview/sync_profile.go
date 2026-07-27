// ABOUTME: Hidden profiling hooks for the sync command (CPU/mem
// ABOUTME: profiles and runtime trace) for performance analysis.
package main

import (
	"log"
	"os"
	"runtime"
	"runtime/pprof"
	"runtime/trace"
	"slices"

	"go.kenn.io/agentsview/internal/pathutil"
)

// startSyncProfile starts whichever of the hidden --cpuprofile,
// --memprofile, and --trace outputs were requested on the sync
// command, and returns a closer that should be deferred from
// runSync. All three are best-effort: a failure to create or start
// a profile is logged and that channel is silently disabled, so a
// profiling typo never aborts a real sync.
func startSyncProfile(cfg SyncConfig) func() {
	var stoppers []func()
	cfg.CPUProfile = expandSyncProfilePath("cpuprofile", cfg.CPUProfile)
	cfg.MemProfile = expandSyncProfilePath("memprofile", cfg.MemProfile)
	cfg.Trace = expandSyncProfilePath("trace", cfg.Trace)

	if cfg.CPUProfile != "" {
		f, err := os.Create(cfg.CPUProfile)
		if err != nil {
			log.Printf("cpuprofile: create %s: %v", cfg.CPUProfile, err)
		} else if err := pprof.StartCPUProfile(f); err != nil {
			log.Printf("cpuprofile: start: %v", err)
			f.Close()
		} else {
			log.Printf("cpuprofile: writing %s", cfg.CPUProfile)
			stoppers = append(stoppers, func() {
				pprof.StopCPUProfile()
				f.Close()
			})
		}
	}

	if cfg.Trace != "" {
		f, err := os.Create(cfg.Trace)
		if err != nil {
			log.Printf("trace: create %s: %v", cfg.Trace, err)
		} else if err := trace.Start(f); err != nil {
			log.Printf("trace: start: %v", err)
			f.Close()
		} else {
			log.Printf("trace: writing %s", cfg.Trace)
			stoppers = append(stoppers, func() {
				trace.Stop()
				f.Close()
			})
		}
	}

	// Memory profile is captured at end (heap snapshot at exit), not
	// streamed, so we just stash the path and write on shutdown.
	memPath := cfg.MemProfile
	stoppers = append(stoppers, func() {
		if memPath == "" {
			return
		}
		runtime.GC() // get up-to-date statistics
		f, err := os.Create(memPath)
		if err != nil {
			log.Printf("memprofile: create %s: %v", memPath, err)
			return
		}
		defer f.Close()
		if err := pprof.WriteHeapProfile(f); err != nil {
			log.Printf("memprofile: write: %v", err)
			return
		}
		log.Printf("memprofile: wrote %s", memPath)
	})

	return func() {
		// Stop in reverse order so trace.Stop runs before file
		// close.
		for _, stop := range slices.Backward(stoppers) {
			stop()
		}
	}
}

func expandSyncProfilePath(name, path string) string {
	expanded, err := pathutil.ExpandHome(path)
	if err != nil {
		log.Printf("%s: expand path: %v", name, err)
		return ""
	}
	return expanded
}
