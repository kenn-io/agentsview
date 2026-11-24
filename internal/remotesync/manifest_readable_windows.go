//go:build windows

package remotesync

func archiveFileReadable(path string) (bool, error) {
	return probeOpenReadable(path)
}
