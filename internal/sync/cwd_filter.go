package sync

import (
	"os"
	"path/filepath"
	"strings"

	"go.kenn.io/agentsview/internal/parser"
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

// sourceAllowsParserExclusions reports whether a source's parser
// exclusions (including engine stale-row cleanup) may delete archived
// rows. When the cwd allow-list is active, a source proves it is
// inside the list by producing at least one allowed session or
// incremental update; a source with no allowed output is frozen — its
// exclusions would erase archived sessions whose replacement writes
// the filter vetoes, which the ingestion-only contract forbids.
// Zero-result exclusion carriers (e.g. a file that parses to no live
// session) have no cwd to judge, so they are frozen too.
func (e *Engine) sourceAllowsParserExclusions(res processResult) bool {
	if e.cwdFilter.empty() {
		return true
	}
	if res.incremental != nil && e.cwdFilter.allows(res.incremental.cwd) {
		return true
	}
	for _, pr := range res.results {
		if e.cwdFilter.allows(pr.Session.Cwd) {
			return true
		}
	}
	return false
}

// splitResultsByCwdFilter returns the parsed sessions the cwd
// allow-list admits and the number it vetoes. With no filter
// configured it returns the input untouched.
func (e *Engine) splitResultsByCwdFilter(
	results []parser.ParseResult,
) ([]parser.ParseResult, int) {
	if e.cwdFilter.empty() || len(results) == 0 {
		return results, 0
	}
	allowed := make([]parser.ParseResult, 0, len(results))
	for _, pr := range results {
		if e.cwdFilter.allows(pr.Session.Cwd) {
			allowed = append(allowed, pr)
		}
	}
	return allowed, len(results) - len(allowed)
}
