package testutil

import "os"

// IsolateCacheDirectory confines application caches to a disposable test directory.
func IsolateCacheDirectory() func() {
	dir, err := os.MkdirTemp("", "agentsview-test-cache-*")
	if err != nil {
		panic(err)
	}
	if err := os.Setenv("CACHE_DIRECTORY", dir); err != nil {
		_ = os.RemoveAll(dir)
		panic(err)
	}
	return func() { _ = os.RemoveAll(dir) }
}
