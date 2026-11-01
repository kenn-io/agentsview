package git

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/commitlinks"
	"go.kenn.io/agentsview/internal/secrets"
	gitcmd "go.kenn.io/kit/git/cmd"
	gitenv "go.kenn.io/kit/git/env"
)

// CommitObject contains bounded Git evidence. Raw commit bodies are never cached.
type CommitObject struct {
	SHA           string   `json:"sha"`
	At            string   `json:"at"`
	Subject       string   `json:"subject"`
	Branch        string   `json:"branch"`
	Sessions      []string `json:"sessions"`
	Paths         []string `json:"paths"`
	PathsComplete bool     `json:"paths_complete"`
}

func commitRunner() gitcmd.Runner {
	r := gitcmd.New()
	r.StdoutLimit = 8 << 20
	r.StderrLimit = 64 << 10
	r.Env = append(gitenv.StripAll(os.Environ()), "GIT_NO_LAZY_FETCH=1", "GIT_ALLOW_PROTOCOL=none")
	r.StripEnv = false
	// Sanitize inherited Git state first; preserve the explicit offline controls
	// rather than letting the runner strip them a second time.
	return r
}

func commitOutput(ctx context.Context, repo string, stdin string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	r := commitRunner()
	out, _, err := r.Run(ctx, repo, strings.NewReader(stdin), args...)
	return out, err
}

func validCommitSHA(sha string) bool {
	if len(sha) < 7 || len(sha) > 40 {
		return false
	}
	for _, c := range sha {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// ReadCommitObject resolves a literal SHA locally, with no lazy fetch, external
// diff, text conversion or prompting. Unavailable objects remain unresolved.
func ReadCommitObject(ctx context.Context, repo, sha string) (CommitObject, error) {
	if !validCommitSHA(sha) {
		return CommitObject{}, errors.New("invalid commit SHA")
	}
	out, err := commitOutput(ctx, repo, "", "show", "--no-patch", "--no-show-signature", "--format=%H%x00%cI%x00%s%x00%B%x00", "--end-of-options", sha+"^{commit}")
	if err != nil {
		return CommitObject{}, err
	}
	objects, err := parseCommitObjects(out)
	if err != nil {
		return CommitObject{}, err
	}
	if len(objects) != 1 {
		return CommitObject{}, errors.New("missing commit object")
	}
	object := objects[0]
	paths, complete, err := ReadCommitPaths(ctx, repo, object.SHA)
	if err == nil {
		object.Paths, object.PathsComplete = paths, complete
	}
	return object, nil
}

// SnapshotCommitTips captures commit tips from local branches, remote-tracking
// branches, tags and detached HEAD. Non-commit tags do not enter the traversal.
func SnapshotCommitTips(ctx context.Context, repo string) ([]string, error) {
	out, err := commitOutput(ctx, repo, "", "for-each-ref", "--format=%(objecttype) %(objectname) %(*objecttype) %(*objectname)", "refs/heads", "refs/remotes", "refs/tags")
	if err != nil {
		return nil, err
	}
	if len(out) > 1<<20 {
		return nil, errors.New("commit ref snapshot exceeds limits")
	}
	seen := map[string]bool{}
	for line := range strings.SplitSeq(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == "commit" && isSHALine(f[1]) {
			seen[f[1]] = true
		}
		if len(f) >= 4 && f[2] == "commit" && isSHALine(f[3]) {
			seen[f[3]] = true
		}
	}
	if head, err := commitOutput(ctx, repo, "", "rev-parse", "--verify", "HEAD^{commit}"); err == nil {
		sha := strings.TrimSpace(string(head))
		if isSHALine(sha) {
			seen[sha] = true
		}
	}
	if len(seen) > 4096 {
		return nil, errors.New("commit ref snapshot exceeds limits")
	}
	tips := make([]string, 0, len(seen))
	for tip := range seen {
		tips = append(tips, tip)
	}
	sort.Strings(tips)
	return tips, nil
}

// ErrCommitScanTipUnavailable means an immutable scan frontier was pruned.
var ErrCommitScanTipUnavailable = errors.New("commit scan tip unavailable")

// ScanSessionTrailersPage walks immutable captured tips, so moving refs cannot
// restart a historical page. It returns every scanned commit, including misses.
func ScanSessionTrailersPage(ctx context.Context, repo string, tips []string, offset, limit int) ([]CommitObject, error) {
	return ScanSessionTrailersPageExcluding(ctx, repo, tips, nil, offset, limit)
}

// ScanSessionTrailersPageExcluding walks only commits reachable from tips that
// are not already reachable from excludedTips. Both frontiers are immutable.
func ScanSessionTrailersPageExcluding(ctx context.Context, repo string, tips, excludedTips []string, offset, limit int) ([]CommitObject, error) {
	if len(tips) == 0 {
		return []CommitObject{}, nil
	}
	if len(tips) > 4096 || len(excludedTips) > 4096 || offset < 0 || limit < 1 || limit > 1000 {
		return nil, errors.New("invalid commit scan bounds")
	}
	for _, tip := range append(append([]string(nil), tips...), excludedTips...) {
		if !isSHALine(tip) {
			return nil, errors.New("invalid commit scan tip")
		}
	}
	for {
		input := commitScanRevisionInput(tips, excludedTips)
		out, err := commitOutput(ctx, repo, input, "log", "--stdin", "--no-merges", "--topo-order", "--no-show-signature", fmt.Sprintf("--skip=%d", offset), fmt.Sprintf("--max-count=%d", limit), "--format=%H%x00%cI%x00%s%x00%B%x00")
		if err == nil {
			return parseCommitObjects(out)
		}
		if !errors.Is(err, gitcmd.ErrStdoutLimitExceeded) {
			// Diagnose missing tips separately from timeouts and other Git
			// failures. Missing positive tips invalidate this pass; missing
			// exclusions can be dropped because they no longer hide history.
			// cat-file reports missing objects without fetching.
			revisions := append(append([]string(nil), tips...), excludedTips...)
			checked, checkErr := commitOutput(ctx, repo, strings.Join(revisions, "\n")+"\n", "cat-file", "--batch-check=%(objecttype)")
			if checkErr == nil {
				lines := strings.Split(strings.TrimSuffix(string(checked), "\n"), "\n")
				if len(lines) == len(revisions) {
					missingPositive := false
					remainingExclusions := make([]string, 0, len(excludedTips))
					for i, line := range lines {
						if !strings.HasSuffix(line, " missing") {
							if i >= len(tips) {
								remainingExclusions = append(remainingExclusions, excludedTips[i-len(tips)])
							}
							continue
						}
						if i < len(tips) {
							missingPositive = true
						}
					}
					if missingPositive {
						return nil, ErrCommitScanTipUnavailable
					}
					if len(remainingExclusions) != len(excludedTips) {
						excludedTips = remainingExclusions
						continue
					}
				}
			}
			return nil, err
		}
		if limit > 1 {
			limit = (limit + 1) / 2
			continue
		}
		// An oversized single commit is unsupported, but cannot stall all
		// older history. Read its literal identity without its unbounded body.
		raw, err := commitOutput(ctx, repo, input, "log", "--stdin", "--no-merges", "--topo-order", fmt.Sprintf("--skip=%d", offset), "--max-count=1", "--format=%H")
		if err != nil {
			return nil, err
		}
		sha := strings.TrimSpace(string(raw))
		if !isSHALine(sha) {
			return nil, errors.New("invalid oversized commit identity")
		}
		return []CommitObject{{SHA: sha}}, nil
	}
}

func commitScanRevisionInput(tips, excludedTips []string) string {
	var input strings.Builder
	for _, tip := range tips {
		input.WriteString(tip)
		input.WriteByte('\n')
	}
	for _, tip := range excludedTips {
		input.WriteByte('^')
		input.WriteString(tip)
		input.WriteByte('\n')
	}
	return input.String()
}

func parseCommitObjects(out []byte) ([]CommitObject, error) {
	fields := strings.Split(string(out), "\x00")
	objects := []CommitObject{}
	for len(fields) >= 5 {
		sha := strings.TrimSpace(fields[0])
		if !isSHALine(sha) {
			return nil, errors.New("invalid Git commit framing")
		}
		at, err := time.Parse(time.RFC3339, fields[1])
		if err != nil {
			return nil, err
		}
		object := CommitObject{SHA: sha, At: at.UTC().Format(time.RFC3339), Subject: secrets.Redact(fields[2])}
		if len(fields[3]) <= 256<<10 {
			object.Sessions = commitlinks.ParseTrailers(fields[3])
		}
		objects = append(objects, object)
		fields = fields[4:]
	}
	if len(fields) > 1 || len(fields) == 1 && strings.TrimSpace(fields[0]) != "" {
		return nil, errors.New("incomplete Git commit framing")
	}
	return objects, nil
}

func ReadCommitPaths(ctx context.Context, repo, sha string) ([]string, bool, error) {
	out, err := commitOutput(ctx, repo, "", "diff-tree", "--root", "--no-commit-id", "--name-only", "--no-renames", "--no-ext-diff", "--no-textconv", "-r", "-z", sha)
	if err != nil {
		return nil, false, err
	}
	if len(out) > 64<<10 {
		return nil, false, nil
	}
	paths := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	if len(paths) == 1 && paths[0] == "" {
		return []string{}, true, nil
	}
	if len(paths) > 256 {
		return paths[:256], false, nil
	}
	return paths, true, nil
}

// ScanSessionTrailersPageCached caches only parsed, bounded records. The key
// includes captured tips and page offset. The caller evicts expired scan keys.
func ScanSessionTrailersPageCached(ctx context.Context, cache *Cache, repo string, tips []string, offset int) ([]CommitObject, error) {
	return ScanSessionTrailersPageCachedExcluding(ctx, cache, repo, tips, nil, offset)
}

// ScanSessionTrailersPageCachedExcluding includes the immutable exclusion
// frontier in its key so pages never mix coverage from different snapshots.
func ScanSessionTrailersPageCachedExcluding(ctx context.Context, cache *Cache, repo string, tips, excludedTips []string, offset int) ([]CommitObject, error) {
	encodedFrontiers, _ := json.Marshal([][]string{tips, excludedTips})
	key := CacheKey("commit-scan-v2", repo, string(encodedFrontiers), fmt.Sprint(offset), "1000")
	payload, err := cache.GetOrCompute(ctx, key, "commit-scan-v2", time.Minute, func() ([]byte, error) {
		objects, err := ScanSessionTrailersPageExcluding(ctx, repo, tips, excludedTips, offset, 1000)
		if err != nil {
			return nil, err
		}
		return json.Marshal(objects)
	})
	if err != nil {
		return nil, err
	}
	var objects []CommitObject
	err = json.Unmarshal(payload, &objects)
	return objects, err
}
