package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"go.kenn.io/agentsview/internal/config"
)

// fallbackTempDirName is the data-dir subdirectory used for temporary files
// when the inherited temp directory is unusable.
const fallbackTempDirName = "tmp"

// ensureUsableTempDir redirects temporary files into the data directory when
// the inherited temp directory cannot be written. SQLite writes statement
// journals and sort spills there, and on Windows it uses TMP/TEMP without
// checking them, so a sandbox that hands child processes an unusable temp
// directory makes every session write fail with "unable to open database
// file". The redirect also covers os.CreateTemp spools and is inherited by
// the sync worker, daemon, and other child processes.
func ensureUsableTempDir(stderr io.Writer) {
	current := os.TempDir()
	probeErr := probeWritableDir(current)
	if probeErr == nil {
		return
	}
	dataDir, err := config.ResolveDataDir()
	if err != nil {
		fmt.Fprintf(stderr,
			"warning: temp directory %s is not writable (%v), "+
				"and the data directory could not be resolved: %v\n",
			current, probeErr, err)
		return
	}
	// Child processes such as the raw parser sandbox require absolute temp
	// paths, and a relative one would move with the working directory.
	fallback, err := filepath.Abs(filepath.Join(dataDir, fallbackTempDirName))
	if err != nil {
		fmt.Fprintf(stderr,
			"warning: temp directory %s is not writable (%v), "+
				"and the fallback path could not be resolved: %v\n",
			current, probeErr, err)
		return
	}
	err = os.MkdirAll(fallback, 0o700)
	if err == nil {
		err = probeWritableDir(fallback)
	}
	if err != nil {
		fmt.Fprintf(stderr,
			"warning: temp directory %s is not writable (%v), "+
				"and the fallback %s is not writable either: %v\n",
			current, probeErr, fallback, err)
		return
	}
	for _, key := range tempDirEnvVars() {
		if err := os.Setenv(key, fallback); err != nil {
			fmt.Fprintf(stderr, "warning: setting %s: %v\n", key, err)
			return
		}
	}
	fmt.Fprintf(stderr,
		"warning: temp directory %s is not writable (%v); using %s\n",
		current, probeErr, fallback)
}

// tempDirEnvVars lists the variables os.TempDir and SQLite read for the
// temp directory on this platform. On Windows, os.TempDir in a process
// running as SYSTEM reads SystemTemp instead of TMP and TEMP.
func tempDirEnvVars() []string {
	if runtime.GOOS == "windows" {
		return []string{"TMP", "TEMP", "SystemTemp"}
	}
	return []string{"TMPDIR"}
}

func probeWritableDir(dir string) error {
	f, err := os.CreateTemp(dir, ".agentsview-probe-*")
	if err != nil {
		return err
	}
	name := f.Name()
	closeErr := f.Close()
	removeErr := os.Remove(name)
	if closeErr != nil {
		return closeErr
	}
	return removeErr
}
