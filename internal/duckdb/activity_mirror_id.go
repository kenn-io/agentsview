package duckdb

import "os"

// activityMirrorID binds the persisted cache to the file the store opened.
// A replacement during open makes the identity unavailable until the watcher
// adopts a verified handle.
func activityMirrorID(path string, expected os.FileInfo) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !os.SameFile(expected, info) {
		return ""
	}
	return activityFileID(file, info)
}
