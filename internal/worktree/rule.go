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
	// Keep the volume outside path.Clean: only its rooted suffix can
	// consume "..", so a parent reference can never leave that volume.
	drive := value[0] | 0x20 // Match ASCII drive letters in either case.
	if len(value) >= 3 && drive >= 'a' && drive <= 'z' && value[1] == ':' && value[2] == '/' {
		return value[:2] + path.Clean(value[2:])
	}
	if unc, ok := strings.CutPrefix(value, "//"); ok {
		host, rest, _ := strings.Cut(unc, "/")
		share, subpath, _ := strings.Cut(strings.TrimLeft(rest, "/"), "/")
		if validUNCComponent(host) && validUNCComponent(share) {
			return "//" + host + "/" + share + path.Clean("/"+subpath)
		}
	}
	return path.Clean(value)
}

func validUNCComponent(value string) bool {
	return value != "" && value != "." && value != ".."
}
