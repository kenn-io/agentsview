package git

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/timeutil"
	gitcmd "go.kenn.io/kit/git/cmd"
)

// TrailerCommit holds Git evidence independently of transcript tool output.
type TrailerCommit struct {
	SHA, Branch, Subject, CommittedAt string
	Sessions                          []string
}

var (
	trailerAgentRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	trailerIDRE    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:._-]{0,127}$`)
	trailerSHARE   = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
)

// ScanCommitTrailers reads trailers from all reachable commits.
func ScanCommitTrailers(ctx context.Context, repo string) ([]TrailerCommit, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	runner := gitcmd.New()
	runner.StdoutLimit = 32 << 20
	// --all includes HEAD from every worktree, including detached worktrees.
	args := []string{"log", "--all", "--source", "-E", "--regexp-ignore-case", "--grep=^Agent-Session:", "--format=%H%x1f%aI%x1f%s%x1f%S%x1f%(trailers:key=Agent-Session,valueonly,separator=%x1e)%x1d"}
	out, stderr, err := runner.Run(ctx, repo, nil, args...)
	if err != nil {
		return nil, fmt.Errorf("git trailer log: %w: %s", err, stderr)
	}
	var commits []TrailerCommit
	for record := range strings.SplitSeq(string(out), "\x1d") {
		fields := strings.Split(strings.TrimSpace(record), "\x1f")
		if len(fields) != 5 || !trailerSHARE.MatchString(fields[0]) {
			continue
		}
		committedAt, err := time.Parse(time.RFC3339, fields[1])
		if err != nil {
			return nil, fmt.Errorf("git commit time: %w", err)
		}
		c := TrailerCommit{SHA: fields[0], CommittedAt: timeutil.Format(committedAt), Subject: fields[2]}
		for _, prefix := range []string{"refs/heads/", "refs/remotes/"} {
			if branch, ok := strings.CutPrefix(fields[3], prefix); ok {
				c.Branch = branch
			}
		}
		for value := range strings.SplitSeq(fields[4], "\x1e") {
			agent, id, ok := strings.Cut(value, ":")
			if ok && trailerAgentRE.MatchString(agent) && trailerIDRE.MatchString(id) {
				c.Sessions = append(c.Sessions, value)
			}
		}
		if len(c.Sessions) > 0 {
			commits = append(commits, c)
		}
	}
	return commits, nil
}
