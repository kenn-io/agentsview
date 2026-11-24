package parser

import (
	"fmt"
	"strings"

	"go.kenn.io/agentsview/internal/export"
)

// RetainedPRRemote stands in for a session's checkout remote while a command is
// analyzed before that remote is known. Links that name its host or repository
// are resolved against the real remote by ResolveRetainedPRLinks.
const RetainedPRRemote = "https://" + retainedPRHost + "/" + retainedPRRepo + ".git"

const (
	retainedPRHost = "remote.agentsview.invalid"
	retainedPRRepo = "agentsview.invalid/session-remote"
)

// ResolveRetainedPRLinks applies a session's context to links analyzed against
// RetainedPRRemote. A link on the placeholder host names a repository without
// a host; one on the placeholder repository names only a number. Both depend
// on the remote and are returned a second time as usesRemote, so callers can
// tell when later context would change them. hostUnknown drops both, and
// allowBare drops bare numbers.
func ResolveRetainedPRLinks(links []PRLink, remote string, allowBare, hostUnknown bool) (resolved, usesRemote []PRLink) {
	remoteHost, remoteRepo := "", ""
	if normalized, ok := export.NormalizeGitRemote(remote); ok {
		remoteHost, remoteRepo, _ = strings.Cut(normalized, "/")
	}
	for _, link := range links {
		if link.Host != retainedPRHost {
			resolved = append(resolved, link)
			continue
		}
		host, repo := remoteHost, link.Repository
		if repo == retainedPRRepo {
			repo = remoteRepo
			if !allowBare || host == "" {
				continue
			}
		}
		if host == "" {
			host = "github.com"
		}
		if hostUnknown {
			continue
		}
		resolvedLink, ok := NewPRLink(fmt.Sprintf("https://%s/%s/pull/%d", host, repo, link.Number), repo, link.Number)
		if ok {
			resolved = append(resolved, resolvedLink)
			usesRemote = append(usesRemote, resolvedLink)
		}
	}
	return CombinePRLinks(resolved), usesRemote
}
