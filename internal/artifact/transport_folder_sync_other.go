//go:build !aix && !darwin && !dragonfly && !freebsd && !illumos && !linux && !netbsd && !openbsd && !solaris && !windows

package artifact

func isFolderDirectorySyncUnsupported(error) bool {
	return false
}
