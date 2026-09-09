// ABOUTME: Hidden profiling hooks for the sync command (CPU/mem
// ABOUTME: profiles and runtime trace) for performance analysis.
package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"runtime/trace"
	"slices"
	"strconv"
	"strings"

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

// startSyncWorkerProfile captures the process doing the sync work. The parent
// CLI's profiling flags cannot profile a daemon's spawned worker.
func startSyncWorkerProfile(mode string) func() {
	disabled := func() {}
	root := strings.TrimSpace(os.Getenv("AGENTSVIEW_SYNC_PROFILE_DIR"))
	if root == "" {
		return disabled
	}
	// Only known dispatch modes may become part of the output directory name.
	switch mode {
	case "startup", "sync", "resync-build", "audit":
	default:
		return disabled
	}
	var captureTrace bool
	if value := strings.TrimSpace(os.Getenv("AGENTSVIEW_SYNC_PROFILE_TRACE")); value != "" {
		var err error
		captureTrace, err = strconv.ParseBool(value)
		if err != nil {
			log.Printf("sync-worker profiling disabled: invalid AGENTSVIEW_SYNC_PROFILE_TRACE: %v", err)
			return disabled
		}
	}
	root = expandSyncProfilePath("AGENTSVIEW_SYNC_PROFILE_DIR", root)
	if root == "" {
		return disabled
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		log.Printf("sync-worker profiling disabled: create directory: %v", err)
		return disabled
	}
	// Each worker owns a private directory, including when the configured root
	// already exists with broader permissions. The random suffix avoids reuse.
	dir, err := os.MkdirTemp(root, fmt.Sprintf("sync-worker-%s-%d-", mode, os.Getpid()))
	if err != nil {
		log.Printf("sync-worker profiling disabled: create worker directory: %v", err)
		return disabled
	}
	cfg := SyncConfig{
		CPUProfile: filepath.Join(dir, "cpu.pprof"),
		MemProfile: filepath.Join(dir, "memory.pprof"),
	}
	if captureTrace {
		cfg.Trace = filepath.Join(dir, "runtime.trace")
	}
	return startSyncProfile(cfg)
}
