package parser

import (
	"path"
	"regexp"
	"strings"

	"github.com/google/shlex"
)

var shellWindowsDriveRE = regexp.MustCompile(`^[A-Za-z]:`)

// ShellWorkdir resolves a transcript path independently of the host OS.
func ShellWorkdir(base, dir string) string {
	if strings.HasPrefix(dir, "'") {
		if words, err := shlex.Split(dir); err == nil && len(words) == 1 {
			dir = words[0]
		}
	} else if len(dir) >= 2 && strings.HasPrefix(dir, `"`) && strings.HasSuffix(dir, `"`) {
		dir = dir[1 : len(dir)-1]
	}
	if path.IsAbs(dir) || strings.HasPrefix(dir, "~") || shellWindowsDriveRE.MatchString(dir) || strings.HasPrefix(dir, `\\`) {
		return dir
	}
	if path.IsAbs(base) && !strings.HasPrefix(base, "//") || !strings.ContainsAny(base+dir, `\`) && !shellWindowsDriveRE.MatchString(base) {
		return path.Join(base, dir)
	}
	// filepath follows the host OS; transcript paths can come from any OS.
	backslashes := strings.ContainsAny(base+dir, `\`)
	base = strings.ReplaceAll(base, `\`, "/")
	dir = strings.ReplaceAll(dir, `\`, "/")
	volume := ""
	if shellWindowsDriveRE.MatchString(base) {
		volume, base = base[:2], base[2:]
	} else if strings.HasPrefix(base, "//") {
		parts := strings.SplitN(base[2:], "/", 3)
		if len(parts) >= 2 {
			volume, base = "//"+parts[0]+"/"+parts[1], "/"
			if len(parts) == 3 {
				base += parts[2]
			}
		}
	}
	if path.IsAbs(dir) {
		base = ""
	}
	joined := volume + path.Join(base, dir)
	if backslashes {
		joined = strings.ReplaceAll(joined, "/", `\`)
	}
	return joined
}
