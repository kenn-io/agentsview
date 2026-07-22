//go:build !darwin

package sync

func newWatchBackend(excludes []string) (watchBackend, error) {
	return newFSNotifyBackend(excludes)
}
