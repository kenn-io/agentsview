package parser

import (
	"net/url"
	"strconv"
	"strings"
	"time"
)

// PRLink is a pull or merge request associated with a session. The shape
// is provider-neutral so any parser can populate it.
type PRLink struct {
	// URL is the normalized web URL: lowercase host, no query, fragment,
	// or trailing slash.
	URL string
	// Host is the lowercase forge host, for example "github.com".
	Host string
	// Repository is the repository path on the host, for example
	// "owner/repo" or "group/subgroup/repo".
	Repository string
	// Number is the pull or merge request number.
	Number int
	// FirstSeenAt is the earliest timestamp the source attached to the
	// link; zero when the source carried none.
	FirstSeenAt time.Time
}

// NewPRLink validates and normalizes a pull request reference. The URL is
// required. Repository and number fall back to values parsed from the URL
// path when the source omits them, and are trusted over the path when it
// supplies them. It reports false when the reference is unusable.
func NewPRLink(
	rawURL, repository string, number int,
	seenAt time.Time,
) (PRLink, bool) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") ||
		u.Host == "" || u.User != nil {
		return PRLink{}, false
	}
	host := strings.ToLower(u.Host)
	path := strings.TrimRight(u.EscapedPath(), "/")
	pathRepo, pathNumber := prLinkPathParts(path)
	repository = strings.Trim(strings.TrimSpace(repository), "/")
	if repository == "" {
		repository = pathRepo
	}
	if number <= 0 {
		number = pathNumber
	}
	if repository == "" || number <= 0 {
		return PRLink{}, false
	}
	normalized := url.URL{Scheme: u.Scheme, Host: host, Path: u.Path}
	normalized.RawPath = ""
	normalizedURL := strings.TrimRight(normalized.String(), "/")
	return PRLink{
		URL:         normalizedURL,
		Host:        host,
		Repository:  repository,
		Number:      number,
		FirstSeenAt: seenAt,
	}, true
}

// prLinkPathParts extracts the repository and number from a GitHub
// (/owner/repo/pull/N), Bitbucket (/workspace/repo/pull-requests/N), or
// GitLab (/group/repo/-/merge_requests/N) path.
func prLinkPathParts(path string) (string, int) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 4 {
		return "", 0
	}
	n, err := strconv.Atoi(parts[len(parts)-1])
	if err != nil || n <= 0 {
		return "", 0
	}
	switch kind := parts[len(parts)-2]; {
	case kind == "pull" || kind == "pulls" || kind == "pull-requests":
		return strings.Join(parts[:len(parts)-2], "/"), n
	case kind == "merge_requests" && len(parts) >= 5 &&
		parts[len(parts)-3] == "-":
		return strings.Join(parts[:len(parts)-3], "/"), n
	}
	return "", 0
}

// prLinkCollector deduplicates links by URL, keeping first-appearance
// order and the earliest timestamp.
type prLinkCollector struct {
	links []PRLink
	index map[string]int
}

func (c *prLinkCollector) add(link PRLink) {
	if i, ok := c.index[link.URL]; ok {
		seen := c.links[i].FirstSeenAt
		if !link.FirstSeenAt.IsZero() &&
			(seen.IsZero() || link.FirstSeenAt.Before(seen)) {
			c.links[i].FirstSeenAt = link.FirstSeenAt
		}
		return
	}
	if c.index == nil {
		c.index = make(map[string]int)
	}
	c.index[link.URL] = len(c.links)
	c.links = append(c.links, link)
}

func (c *prLinkCollector) result() []PRLink {
	if len(c.links) == 0 {
		return nil
	}
	return c.links
}

// mergePRLinks combines link lists with the collector's deduplication.
func mergePRLinks(lists ...[]PRLink) []PRLink {
	var c prLinkCollector
	for _, list := range lists {
		for _, link := range list {
			c.add(link)
		}
	}
	return c.result()
}

// prLinkChangesStored reports whether a full parse would store something
// different after seeing link, given the stored URLs and first-seen
// times: a new URL, or an earlier timestamp for a stored URL.
func prLinkChangesStored(stored map[string]time.Time, link PRLink) bool {
	seen, ok := stored[link.URL]
	if !ok {
		return true
	}
	if link.FirstSeenAt.IsZero() {
		return false
	}
	return seen.IsZero() || link.FirstSeenAt.Before(seen)
}
