//go:build windows

package requestsign

// SQLite's Windows VFS flushes database and journal files but ignores requests
// to sync their containing directory, so a directory sync is not available here.
func syncReplayDirectory(string) error { return nil }
