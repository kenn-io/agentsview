package sync

import (
	"os"
	"path/filepath"
	"strings"
)

// cwdPrefixFilter gates session ingestion on the session working
// directory. An empty filter allows everything. A non-empty filter
// allows only sessions whose cwd equals a configured prefix or lives
// underneath one; sessions without a recorded cwd are rejected
// because they cannot be attributed to any workspace.
//
// Prefixes and cwds are lexically cleaned before matching and the
// path boundary is the local OS separator. The filter only ever sees
// local paths (remote sync does not apply it), so local filesystem
// semantics are the correct ones: on POSIX a backslash is an ordinary
// filename character, not a boundary, and a cwd like "/a/b/../c" is
// resolved to "/a/c" rather than prefix-matching "/a/b".
type cwdPrefixFilter struct {
	prefixes []string
}

// newCwdPrefixFilter normalizes the configured prefixes: entries are
// trimmed, blank entries are dropped, and each remaining entry is
// cleaned with filepath.Clean so "/a/b/" and "/a/b" behave
// identically and ".." components cannot linger in a prefix.
func newCwdPrefixFilter(prefixes []string) cwdPrefixFilter {
	normalized := make([]string, 0, len(prefixes))
	for _, p := range prefixes {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		normalized = append(normalized, filepath.Clean(p))
	}
	return cwdPrefixFilter{prefixes: normalized}
}

func (f cwdPrefixFilter) empty() bool {
	return len(f.prefixes) == 0
}

// allows reports whether a session with the given cwd may be
// ingested. Matching is path-boundary aware: prefix "/a/b" matches
// "/a/b" and "/a/b/c" but not "/a/bc".
func (f cwdPrefixFilter) allows(cwd string) bool {
	if f.empty() {
		return true
	}
	if cwd == "" {
		return false
	}
	cwd = filepath.Clean(cwd)
	sep := string(os.PathSeparator)
	for _, p := range f.prefixes {
		if cwd == p {
			return true
		}
		prefix := p
		if !strings.HasSuffix(prefix, sep) {
			prefix += sep
		}
		if strings.HasPrefix(cwd, prefix) {
			return true
		}
	}
	return false
}
