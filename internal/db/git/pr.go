package git

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// PRResult holds pull-request counts for a window. Opened counts PRs created
// in [since, until]; Merged counts PRs merged in [since, until] regardless of
// when they were created.
type PRResult struct {
	Opened int
	Merged int
}

// AggregatePRs queries the `gh` CLI twice per repo — once for PRs created in
// the window, once for PRs merged in the window — and returns the counts.
//
// since/until are formatted into gh's `--search=created:SINCE..UNTIL` (and
// `merged:...`) range expressions. GitHub search treats `>=A..B` as malformed
// — the colon-and-double-dot range syntax already implies an inclusive
// closed window, so the bounds must be plain dates or RFC3339 timestamps.
// The repo argument sets the working directory for `gh` so it picks up the
// correct remote.
//
// When ghToken is empty this returns (nil, nil): the caller distinguishes
// "unknown — gh not configured" from a legitimate zero count. GH_TOKEN is
// injected via the exec environment so it never appears in argv or logs.
//
// Any failure to invoke `gh` or parse its output is surfaced as an error;
// callers should log and continue rather than fail the whole aggregation.
func AggregatePRs(
	ctx context.Context, repo, since, until, ghToken string,
) (*PRResult, error) {
	if ghToken == "" {
		return nil, nil
	}
	window := since + ".." + until
	opened, err := countPRs(ctx, repo, ghToken, []string{
		"pr", "list",
		"--state=all",
		"--author=@me",
		"--search=created:" + window,
		"--json", "state",
		"--limit", "500",
	})
	if err != nil {
		return nil, fmt.Errorf("gh pr list (opened) in %s: %w", repo, err)
	}
	merged, err := countPRs(ctx, repo, ghToken, []string{
		"pr", "list",
		"--state=merged",
		"--author=@me",
		"--search=merged:" + window,
		"--json", "state",
		"--limit", "500",
	})
	if err != nil {
		return nil, fmt.Errorf("gh pr list (merged) in %s: %w", repo, err)
	}
	return &PRResult{Opened: opened, Merged: merged}, nil
}

// countPRs runs `gh` with the given args inside repo and returns the length of
// the resulting JSON array. GH_TOKEN is set in the exec env (not argv) so it
// doesn't leak into process listings.
func countPRs(
	ctx context.Context, repo, ghToken string, args []string,
) (int, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Dir = repo
	cmd.Env = ghEnv(ghToken)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return 0, err
		}
		return 0, fmt.Errorf("%w: %s", err, msg)
	}
	var rows []struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &rows); err != nil {
		return 0, fmt.Errorf("parse gh json: %w", err)
	}
	return len(rows), nil
}

// ghEnv returns a copy of the current environment with GH_TOKEN set/overridden
// to the caller-provided token. Any existing GH_TOKEN is replaced so the
// parent process's credentials don't shadow the injected one.
func ghEnv(ghToken string) []string {
	base := os.Environ()
	env := make([]string, 0, len(base)+1)
	for _, kv := range base {
		if strings.HasPrefix(kv, "GH_TOKEN=") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "GH_TOKEN="+ghToken)
	return env
}
