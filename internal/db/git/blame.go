package git

import (
	"context"
	"fmt"
	"strings"

	gitcmd "go.kenn.io/kit/git/cmd"
)

// BlameCommit counts selected lines belonging to a commit.
type BlameCommit struct {
	SHA     string
	Subject string
	Lines   int
}

// BlameResult preserves commits in their first appearance in the file.
type BlameResult struct {
	Commits     []BlameCommit
	Uncommitted int
}

// BlameCommits reads local blame, including uncommitted working tree edits.
// dir is the file's directory and path is its base name.
func BlameCommits(ctx context.Context, dir, path string, start, end int) (BlameResult, error) {
	args := []string{"blame", "--porcelain"}
	if start > 0 {
		args = append(args, "-L", fmt.Sprintf("%d,%d", start, end))
	}
	args = append(args, "--", path)
	runner := gitcmd.New()
	runner.NullGlobalConfig = false
	out, stderr, err := runner.Run(ctx, dir, nil, args...)
	if err != nil {
		return BlameResult{}, fmt.Errorf("git blame: %w: %s", err, strings.TrimSpace(string(stderr)))
	}
	result := BlameResult{}
	indices := make(map[string]int)
	current := -1
	for line := range strings.SplitSeq(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && len(fields) <= 4 && isSHALine(fields[0]) && !strings.HasPrefix(line, "\t") {
			sha := fields[0]
			current = -1
			if sha == strings.Repeat("0", len(sha)) {
				result.Uncommitted++
				continue
			}
			index, ok := indices[sha]
			if !ok {
				index = len(result.Commits)
				indices[sha] = index
				result.Commits = append(result.Commits, BlameCommit{SHA: sha})
			}
			current = index
			result.Commits[index].Lines++
		} else if current >= 0 && strings.HasPrefix(line, "summary ") {
			result.Commits[current].Subject = strings.TrimPrefix(line, "summary ")
		}
	}
	return result, nil
}
