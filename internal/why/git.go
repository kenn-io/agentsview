package why

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	gitcmd "go.kenn.io/kit/git/cmd"
	gitenv "go.kenn.io/kit/git/env"
)

const maxFileBytes = 4 << 20

// Inspection pins the repository tree used to validate named documents.
type Inspection struct {
	Question     Question
	Lines        []Line
	Commits      []Commit
	Older        int
	Milliseconds int64
	root, tree   string
	runner       gitcmd.Runner
}

func newGitRunner() gitcmd.Runner {
	// Preserve our no-fetch control after stripping all inherited Git bindings.
	r := gitcmd.New().WithEnv(append(gitenv.StripAll(os.Environ()), "GIT_NO_LAZY_FETCH=1"))
	r.StripEnv = false
	r.StdoutLimit = 4 << 20
	r.StderrLimit = 64 << 10
	return r.WithConfig("core.fsmonitor", "false").WithConfig("diff.external", "")
}
func runGit(ctx context.Context, r gitcmd.Runner, dir string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, _, err := r.Run(ctx, dir, nil, args...)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, gitcmd.ErrStdoutLimitExceeded) {
			return nil, errors.New("git output exceeds 4 MiB; supply a smaller line range")
		}
		return nil, fmt.Errorf("git %s: %w", args[0], err)
	}
	return out, nil
}

// Inspect reads one local question without executing configured content helpers.
func Inspect(ctx context.Context, cwd string, req Request) (*Inspection, error) {
	start := time.Now()
	// Git reports a physical root; compare relative paths in that same space.
	cwd, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return nil, fmt.Errorf("resolve checkout directory: %w", err)
	}
	cwd, err = filepath.Abs(cwd)
	if err != nil {
		return nil, err
	}
	r := newGitRunner()
	out, err := runGit(ctx, r, cwd, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("run why in a Git checkout: %w", err)
	}
	root := strings.TrimSuffix(string(out), "\n")
	var tree string
	if req.Commit != "" {
		tree, err = resolveCommitPrefix(ctx, r, root, req.Commit)
	} else {
		rev := req.Rev
		if rev == "" {
			rev = "HEAD"
		}
		out, err = runGit(ctx, r, root, "rev-parse", "--verify", "--end-of-options", rev+"^{commit}")
		if err == nil {
			tree = strings.TrimSpace(string(out))
		}
	}
	if err != nil {
		return nil, fmt.Errorf("resolve commit or revision: %w", err)
	}
	if len(tree) != 40 || !shaPattern.MatchString(tree) {
		return nil, errors.New("why requires a SHA-1 Git repository")
	}
	result := &Inspection{Lines: []Line{}, Commits: []Commit{}, root: root, tree: tree, runner: r}
	if req.Commit != "" {
		result.Question = Question{Commit: tree}
		result.Commits = append(result.Commits, Commit{SHA: tree})
	} else {
		target := req.Path
		if !filepath.IsAbs(target) {
			target = filepath.Join(cwd, target)
		}
		target, err = filepath.Abs(target)
		if err != nil {
			return nil, err
		}
		rel, err := filepath.Rel(root, target)
		if err == nil && (rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
			// Replace the outermost checkout alias, keeping historical path
			// components lexical even when they are symlinks in today's worktree.
			logical := target
			for parent := filepath.Dir(logical); parent != filepath.Dir(parent); parent = filepath.Dir(parent) {
				real, resolveErr := filepath.EvalSymlinks(parent)
				if resolveErr != nil {
					continue
				}
				if same, relErr := filepath.Rel(root, real); relErr == nil && same == "." {
					suffix, _ := filepath.Rel(parent, logical)
					target = filepath.Join(root, suffix)
				}
			}
			rel, err = filepath.Rel(root, target)
		}
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, errors.New("path escapes the Git checkout")
		}
		rel = filepath.ToSlash(rel)
		if req.Rev == "" {
			real, err := filepath.EvalSymlinks(target)
			if err != nil {
				return nil, fmt.Errorf("read worktree file (use --rev for a historical path): %w", err)
			}
			actualRel, err := filepath.Rel(root, real)
			if err != nil || actualRel == ".." || strings.HasPrefix(actualRel, ".."+string(filepath.Separator)) {
				return nil, errors.New("symlink path escapes the Git checkout")
			}
			st, err := os.Lstat(target)
			if err != nil {
				return nil, err
			}
			if !st.Mode().IsRegular() {
				return nil, errors.New("worktree path must be a regular file")
			}
			if st.Size() > maxFileBytes {
				return nil, errors.New("file exceeds 4 MiB; select a smaller file")
			}
			// Disable every effective filter driver before working-tree conversion.
			keys, cfgErr := runGit(ctx, r, root, "config", "--null", "--name-only", "--get-regexp", `^filter\..*\.(clean|process|required)$`)
			if cfgErr != nil && !gitcmd.IsExitCode(cfgErr, 1) {
				return nil, fmt.Errorf("inspect content filters: %w", cfgErr)
			}
			for _, key := range strings.Split(string(keys), "\x00") {
				if key == "" {
					continue
				}
				base := key[:strings.LastIndexByte(key, '.')]
				r = r.WithConfig(base+".clean", "").WithConfig(base+".process", "").WithConfig(base+".required", "false")
			}
		} else {
			size, err := runGit(ctx, r, root, "cat-file", "-s", tree+":"+rel)
			if err != nil {
				return nil, err
			}
			n, err := strconv.ParseInt(strings.TrimSpace(string(size)), 10, 64)
			if err != nil {
				return nil, err
			}
			if n > maxFileBytes {
				return nil, errors.New("file exceeds 4 MiB; select a smaller file")
			}
		}
		args := []string{"blame", "--line-porcelain", "--no-textconv", "--ignore-revs-file=", "-w"}
		if req.Start != 0 {
			if req.Start < 1 || req.End < req.Start || req.End-req.Start >= maxLines {
				return nil, errors.New("invalid or oversized line range")
			}
			args = append(args, "-L", fmt.Sprintf("%d,%d", req.Start, req.End))
		}
		if req.Rev != "" {
			args = append(args, tree)
		}
		args = append(args, "--", rel)
		raw, err := runGit(ctx, r, root, args...)
		if err != nil {
			return nil, fmt.Errorf("blame failed; check the path/range and local objects: %w", err)
		}
		result.Lines, err = parseBlame(raw)
		if err != nil {
			return nil, err
		}
		if req.Start != 0 && len(result.Lines) != req.End-req.Start+1 {
			return nil, errors.New("line range extends beyond the end of the file")
		}
		from, to := 0, 0
		if len(result.Lines) > 0 {
			from = result.Lines[0].N
			to = result.Lines[len(result.Lines)-1].N
		}
		questionRev := req.Rev
		if questionRev == "" {
			questionRev = "WORKTREE"
		}
		result.Question = Question{Path: rel, Start: &from, End: &to, Rev: questionRev}
		result.Commits, result.Older = selectCommits(result.Lines)
	}
	for i := range result.Commits {
		c := &result.Commits[i]
		out, err := runGit(ctx, r, root, "show", "--no-patch", "--no-notes", "--no-show-signature", "--format=%H%x00%cI%x00%an%x00%s%x00%B", c.SHA, "--")
		if err != nil {
			return nil, err
		}
		fields := strings.SplitN(string(out), "\x00", 5)
		if len(fields) != 5 {
			return nil, errors.New("malformed Git commit metadata")
		}
		c.At = fields[1]
		c.Author = strings.ToValidUTF8(fields[2], "\uFFFD")
		c.Subject = strings.ToValidUTF8(fields[3], "\uFFFD")
		c.Message = strings.ToValidUTF8(fields[4], "\uFFFD")
	}
	result.Milliseconds = time.Since(start).Milliseconds()
	return result, nil
}

func resolveCommitPrefix(ctx context.Context, r gitcmd.Runner, root, prefix string) (string, error) {
	out, err := runGit(ctx, r, root, "rev-parse", "--disambiguate="+prefix)
	if err != nil {
		return "", err
	}
	objects := strings.Fields(string(out))
	if len(objects) == 0 {
		return "", errors.New("commit prefix does not match a local object")
	}
	if len(objects) != 1 {
		return "", errors.New("commit prefix is ambiguous")
	}
	kind, err := runGit(ctx, r, root, "cat-file", "-t", objects[0])
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(string(kind)) != "commit" {
		return "", errors.New("commit prefix does not identify a commit")
	}
	return objects[0], nil
}

// DocumentExists tests a literal path in the pinned tree without reading its body.
func (in *Inspection) DocumentExists(ctx context.Context, path string) (bool, error) {
	out, err := runGit(ctx, in.runner, in.root, "--literal-pathspecs", "ls-tree", "-z", in.tree, "--", strings.TrimPrefix(path, "./"))
	if err != nil {
		return false, err
	}
	for _, entry := range strings.Split(string(out), "\x00") {
		meta, name, ok := strings.Cut(entry, "\t")
		if !ok || name != strings.TrimPrefix(path, "./") {
			continue
		}
		if strings.HasPrefix(meta, "100644 blob ") || strings.HasPrefix(meta, "100755 blob ") {
			return true, nil
		}
	}
	return false, nil
}
