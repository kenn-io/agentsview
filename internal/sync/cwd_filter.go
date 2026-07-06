package sync

import "strings"

// cwdPrefixFilter gates session ingestion on the session working
// directory. An empty filter allows everything. A non-empty filter
// allows only sessions whose cwd equals a configured prefix or lives
// underneath one; sessions without a recorded cwd are rejected
// because they cannot be attributed to any workspace.
type cwdPrefixFilter struct {
	prefixes []string
}

// newCwdPrefixFilter normalizes the configured prefixes: entries are
// trimmed, blank entries are dropped, and trailing path separators
// are stripped so "/a/b/" and "/a/b" behave identically.
func newCwdPrefixFilter(prefixes []string) cwdPrefixFilter {
	normalized := make([]string, 0, len(prefixes))
	for _, p := range prefixes {
		p = strings.TrimSpace(p)
		p = strings.TrimRight(p, "/\\")
		if p == "" {
			continue
		}
		normalized = append(normalized, p)
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
	for _, p := range f.prefixes {
		if cwd == p {
			return true
		}
		if len(cwd) > len(p) && strings.HasPrefix(cwd, p) {
			if sep := cwd[len(p)]; sep == '/' || sep == '\\' {
				return true
			}
		}
	}
	return false
}
