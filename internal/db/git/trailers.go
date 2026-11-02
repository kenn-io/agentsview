package git

import (
	"context"
	"crypto/sha256"
	"fmt"
	"regexp"
	"strings"
	"time"

	gitcmd "go.kenn.io/kit/git/cmd"
)

// TrailerCommit holds Git evidence independently of transcript tool output.
type TrailerCommit struct {
	SHA, Branch, Subject, CommittedAt string
	Sessions                          []string
}

var trailerValueRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}:[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
var trailerSHARE = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

// RefsFingerprint returns both the fingerprint and its reachable tips for the
// next incremental scan. An unborn repository has neither.
func RefsFingerprint(ctx context.Context, repo string) (string, []string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	runner := gitcmd.New()
	runner.StdoutLimit = 32 << 20
	out, stderr, err := runner.Run(ctx, repo, nil, "show-ref", "--head")
	if err != nil {
		if gitcmd.IsExitCode(err, 1) && len(out) == 0 && len(stderr) == 0 && ctx.Err() == nil {
			return "", nil, nil
		}
		return "", nil, fmt.Errorf("git show-ref: %w: %s", err, stderr)
	}
	tips := []string{}
	seen := map[string]bool{}
	for line := range strings.Lines(string(out)) {
		sha, _, _ := strings.Cut(line, " ")
		if trailerSHARE.MatchString(sha) && !seen[sha] {
			tips = append(tips, sha)
			seen[sha] = true
		}
	}
	return fmt.Sprintf("%x", sha256.Sum256(out)), tips, nil
}

// ScanCommitTrailers reads new reachable commits. Missing saved tips cause one
// full scan, as garbage collection may remove history after a rebase.
func ScanCommitTrailers(ctx context.Context, repo string, previous []string) ([]TrailerCommit, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	runner := gitcmd.New()
	runner.StdoutLimit = 32 << 20
	revisions := ""
	for _, tip := range previous {
		revisions += "^" + tip + "\n"
	}
	args := []string{"log", "--all", "--stdin", "--source", "-E", "--grep=^Agent-Session:", "--format=%H%x1f%cI%x1f%s%x1f%S%x1f%(trailers:key=Agent-Session,valueonly,separator=%x1e)%x1d"}
	out, stderr, err := runner.Run(ctx, repo, strings.NewReader(revisions), args...)
	if err != nil && len(previous) > 0 && strings.Contains(string(stderr), "bad object") {
		out, stderr, err = runner.Run(ctx, repo, nil, args...)
	}
	if err != nil {
		return nil, fmt.Errorf("git trailer log: %w: %s", err, stderr)
	}
	commits := []TrailerCommit{}
	for _, record := range strings.Split(string(out), "\x1d") {
		fields := strings.Split(strings.TrimSpace(record), "\x1f")
		if len(fields) != 5 || !trailerSHARE.MatchString(fields[0]) {
			continue
		}
		c := TrailerCommit{SHA: fields[0], CommittedAt: fields[1], Subject: fields[2]}
		for _, prefix := range []string{"refs/heads/", "refs/remotes/"} {
			if branch, ok := strings.CutPrefix(fields[3], prefix); ok {
				c.Branch = branch
			}
		}
		for _, value := range strings.Split(fields[4], "\x1e") {
			value = strings.TrimSpace(value)
			if trailerValueRE.MatchString(value) {
				c.Sessions = append(c.Sessions, value)
			}
		}
		if len(c.Sessions) > 0 {
			commits = append(commits, c)
		}
	}
	return commits, nil
}
