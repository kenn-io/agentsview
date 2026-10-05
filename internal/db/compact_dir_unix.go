//go:build !windows

package db

import "go.kenn.io/kit/atomicfile"

// replaceInstalledFile atomically installs source over target on Unix. Both
// paths are required to be on the same filesystem by the caller.
func replaceInstalledFile(source, target string) error {
	return atomicfile.Replace(source, target)
}
