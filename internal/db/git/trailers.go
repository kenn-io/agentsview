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
	args := []string{"log", "--all", "--source", "-E", "--regexp-ignore-case", "--grep=^Agent-Session", "--format=%H%x00%aI%x00%s%x00%S%x00%(trailers:key=Agent-Session,valueonly,unfold=true,separator=%x0a)%x00"}
	out, stderr, err := runner.Run(ctx, repo, nil, args...)
	if err != nil {
		return nil, fmt.Errorf("git trailer log: %w: %s", err, stderr)
	}
	var commits []TrailerCommit
	// Git message fields cannot contain NUL. Consume five fields per commit
	// so empty fields and control bytes in subjects preserve record boundaries.
	fields := strings.Split(string(out), "\x00")
	for len(fields) >= 5 {
		record := fields[:5]
		fields = fields[5:]
		record[0] = strings.TrimSpace(record[0])
		if !trailerSHARE.MatchString(record[0]) {
			continue
		}
		committedAt, err := time.Parse(time.RFC3339, record[1])
		if err != nil {
			return nil, fmt.Errorf("git commit time: %w", err)
		}
		c := TrailerCommit{SHA: record[0], CommittedAt: timeutil.Format(committedAt), Subject: record[2]}
		for _, prefix := range []string{"refs/heads/", "refs/remotes/"} {
			if branch, ok := strings.CutPrefix(record[3], prefix); ok {
				c.Branch = branch
			}
		}
		// Unfolding joins continuation lines, so each line is a whole value.
		for value := range strings.SplitSeq(record[4], "\n") {
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
