//go:build !darwin && !linux

package trash

// resolveBackend returns the fallback backend on platforms where the
// system trash is not reachable with stdlib only (Windows).
func resolveBackend(dataDir string) backend {
	return fallbackBackend{dataDir: dataDir}
}
