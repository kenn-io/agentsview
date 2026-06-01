package git

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	gitcmd "go.kenn.io/kit/git/cmd"
)

// LogResult aggregates author-filtered counts from `git log --numstat` output.
type LogResult struct {
	Commits      int
	LOCAdded     int
	LOCRemoved   int
	FilesChanged int
}

// AggregateLog runs `git log --numstat` filtered by author and window and
// returns total commits, lines added, lines removed, and files changed.
//
// The since/until timestamps should be RFC3339 strings; git accepts them
// directly via `--since`/`--until`. LOC counts from binary files (which
// numstat represents as `-\t-\t<path>`) are skipped, but the file still
// counts toward FilesChanged.
//
// If the window is empty or the author matches nothing, a zero-valued
// LogResult is returned with no error. Exec failures (bad repo path, git
// missing from PATH) are surfaced as errors.
func AggregateLog(
	ctx context.Context, repo, authorEmail, since, until string,
) (LogResult, error) {
	runner := gitcmd.New()
	runner.NullGlobalConfig = false
	out, stderr, err := runner.Run(
		ctx, repo, nil,
		"log",
		"--numstat",
		"--format=%H",
		"--since="+since,
		"--until="+until,
		"--author="+authorEmailPattern(authorEmail),
	)
	if err != nil {
		msg := strings.TrimSpace(string(stderr))
		// An empty repo (initialized but no commits, or a worktree
		// pointed at an unborn branch) is a normal state, not an
		// error — there is simply no log to aggregate. Treat as a
		// zero result so callers don't spam the user with errors
		// for every checkout that hasn't been used yet.
		if isEmptyRepoErr(msg) {
			return LogResult{}, nil
		}
		if msg == "" {
			return LogResult{}, fmt.Errorf("git log in %s: %w", repo, err)
		}
		return LogResult{}, fmt.Errorf(
			"git log in %s: %w: %s", repo, err, msg,
		)
	}
	return parseNumstat(out), nil
}

// isEmptyRepoErr reports whether a `git log` stderr message indicates
// the repo has no commits on the current branch — i.e., not a real
// failure, just nothing to aggregate. Both phrasings below have been
// stable across modern git versions; the second one shows up when
// HEAD points at a ref that doesn't yet exist (a freshly-created
// worktree on an unborn branch).
func isEmptyRepoErr(stderr string) bool {
	return strings.Contains(stderr, "does not have any commits yet") ||
		strings.Contains(stderr, "bad default revision 'HEAD'")
}

// parseNumstat walks `git log --numstat --format=%H` output and aggregates
// commit/LOC/file totals. The format emits a SHA line, a blank line, then
// zero or more numstat lines per commit:
//
//	<40-hex-sha>
//
//	<added>\t<removed>\t<path>
//	-\t-\t<binary-path>
func parseNumstat(out []byte) LogResult {
	var result LogResult
	scanner := bufio.NewScanner(bytes.NewReader(out))
	// Allow long paths; 1 MiB is generous but keeps a hard ceiling.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		if isSHALine(line) {
			result.Commits++
			continue
		}
		added, removed, ok := parseNumstatLine(line)
		if !ok {
			continue
		}
		result.FilesChanged++
		result.LOCAdded += added
		result.LOCRemoved += removed
	}
	return result
}

// isSHALine reports whether s is a 40-character lowercase hex SHA, as emitted
// by `--format=%H`.
func isSHALine(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}

// parseNumstatLine parses a single `added\tremoved\tpath` numstat row.
// Binary files use "-" for both counts; we treat those as zero LOC but
// still return ok=true so the file is counted.
func parseNumstatLine(line string) (added, removed int, ok bool) {
	parts := strings.SplitN(line, "\t", 3)
	if len(parts) < 3 {
		return 0, 0, false
	}
	added, err := parseNumstatCount(parts[0])
	if err != nil {
		return 0, 0, false
	}
	removed, err = parseNumstatCount(parts[1])
	if err != nil {
		return 0, 0, false
	}
	return added, removed, true
}

// parseNumstatCount returns 0 for the binary marker "-" and parses digits
// otherwise. Anything else is a parse error so the caller can reject the line.
func parseNumstatCount(s string) (int, error) {
	if s == "-" {
		return 0, nil
	}
	return strconv.Atoi(s)
}

// authorEmailPattern returns a regex that matches authorEmail literally.
// `git log --author` interprets its value as a regex, so emails containing
// metacharacters like "." or "+" (e.g. "first.last+dev@example.com") match
// many unrelated authors. Anchoring an escaped pattern with `<...>` keeps
// the match scoped to the author header's "<email>" portion — git formats
// the author line as "Name <email>", so the angle brackets bound the email
// without needing a full ^...$ on the whole header.
func authorEmailPattern(email string) string {
	return "<" + regexp.QuoteMeta(email) + ">"
}

// AuthorEmail returns `git config user.email` run from inside the repo,
// falling back to the global config. Returns "" if neither is set or git
// is not available.
func AuthorEmail(ctx context.Context, repo string) string {
	localRunner := gitcmd.New()
	localRunner.NullGlobalConfig = false
	out, err := localRunner.Output(ctx, repo, "config", "user.email")
	if err == nil {
		if v := strings.TrimSpace(string(out)); v != "" {
			return v
		}
	}

	globalRunner := gitcmd.New()
	globalRunner.NullGlobalConfig = false
	out, err = globalRunner.Output(ctx, "", "config", "--global", "user.email")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
