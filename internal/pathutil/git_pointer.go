package pathutil

import (
	"path/filepath"
	"runtime"
	"strings"
)

// GitPointerPath resolves a pointer beside its file, or on cwd's drive for a Windows rooted path.
func GitPointerPath(pointer, base, cwd string) string {
	if pointer == "" {
		return ""
	}
	pointer = filepath.FromSlash(pointer)
	if runtime.GOOS == "windows" {
		volume := filepath.VolumeName(pointer)
		if volume != "" && !filepath.IsAbs(pointer) {
			return ""
		}
		if volume == "" && strings.HasPrefix(pointer, `\`) {
			volume = filepath.VolumeName(cwd)
			if len(volume) != 2 || volume[1] != ':' {
				return ""
			}
			return filepath.Clean(volume + pointer)
		}
	}
	if filepath.IsAbs(pointer) {
		return filepath.Clean(pointer)
	}
	return filepath.Join(base, pointer)
}
