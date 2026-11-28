// Package worktree defines worktree mapping rules shared by TOML and the archive.
package worktree

import (
	"errors"
	"fmt"
	"path"
	"strings"

	"go.kenn.io/agentsview/internal/parser"
)

const (
	LayoutExplicit         = "explicit"
	LayoutRepoDotWorktrees = "repo_dot_worktrees"
	LayoutRepoDotSuffix    = "repo_dot_suffix"
)

// Rule maps a path prefix to a fixed or directory-derived project name.
type Rule struct {
	PathPrefix string `toml:"path_prefix"`
	Layout     string `toml:"layout"`
	Project    string `toml:"project"`
}

// Normalize validates a rule and returns its canonical prefix, layout and
// project. Archive writes discard unused project values before calling it for
// compatibility with clients that send the old project when switching layouts.
func (r Rule) Normalize() (Rule, error) {
	r.PathPrefix = NormalizePath(r.PathPrefix)
	if r.PathPrefix == "" || r.PathPrefix == "." {
		return Rule{}, errors.New("path_prefix is required")
	}
	r.Layout = strings.TrimSpace(r.Layout)
	if r.Layout == "" {
		r.Layout = LayoutExplicit
	}
	r.Project = strings.TrimSpace(r.Project)
	switch r.Layout {
	case LayoutExplicit:
		if r.Project == "" {
			return Rule{}, errors.New("project is required")
		}
		r.Project = parser.NormalizeName(r.Project)
	case LayoutRepoDotWorktrees, LayoutRepoDotSuffix:
		if r.Project != "" {
			return Rule{}, fmt.Errorf("project must be empty for %s", r.Layout)
		}
	default:
		return Rule{}, fmt.Errorf("layout must be %s, %s, or %s",
			LayoutExplicit, LayoutRepoDotWorktrees, LayoutRepoDotSuffix)
	}
	return r, nil
}

// NormalizePath cleans recorded paths lexically, including drive and UNC roots
// from Windows sessions on other operating systems.
func NormalizePath(value string) string {
	value = strings.TrimSpace(strings.ReplaceAll(value, `\`, "/"))
	if value == "" {
		return ""
	}
	if strings.HasPrefix(value, "//") && !strings.HasPrefix(value, "///") {
		rest := path.Clean(strings.TrimPrefix(value, "//"))
		parts := strings.Split(rest, "/")
		if len(parts) >= 2 && parts[0] != "" && parts[0] != "." &&
			parts[0] != ".." && parts[1] != "" && parts[1] != "." &&
			parts[1] != ".." {
			normalized := "//" + rest
			if len(parts) == 2 {
				return normalized + "/"
			}
			return normalized
		}
	}
	driveAbsolute := len(value) >= 3 && value[1] == ':' && value[2] == '/'
	normalized := path.Clean(value)
	if driveAbsolute && normalized == value[:2] {
		return normalized + "/"
	}
	return normalized
}
