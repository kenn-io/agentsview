package parser

import (
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// PRLink is a pull or merge request associated with a session. The shape
// is provider-neutral so any parser can populate it.
type PRLink struct {
	// URL is the normalized web URL: lowercase host, no query, fragment,
	// or trailing slash.
	URL string `json:"url"`
	// Host is the lowercase forge host, for example "github.com".
	Host string `json:"host"`
	// Repository is the repository path on the host, for example
	// "owner/repo" or "group/subgroup/repo".
	Repository string `json:"repository"`
	// Number is the pull or merge request number.
	Number int `json:"number"`
}

// NewPRLink validates a web URL and the repository and number supplied by the source.
func NewPRLink(
	rawURL, repository string, number int,
) (PRLink, bool) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") ||
		u.Host == "" || u.User != nil {
		return PRLink{}, false
	}
	host := strings.ToLower(u.Host)
	repository = strings.Trim(strings.TrimSpace(repository), "/")
	if repository == "" || number <= 0 {
		return PRLink{}, false
	}
	urlRepository, rawNumber, remainder, _, routeFound := prLinkRoute(strings.TrimRight(u.Path, "/"))
	if routeFound {
		urlNumber, err := strconv.Atoi(rawNumber)
		if err != nil || remainder != "" || urlNumber != number || !strings.EqualFold(strings.TrimPrefix(urlRepository, "/"), repository) {
			return PRLink{}, false
		}
	}
	normalized := url.URL{Scheme: u.Scheme, Host: host, Path: u.Path}
	normalized.RawPath = ""
	normalizedURL := strings.TrimRight(normalized.String(), "/")
	return PRLink{
		URL:        normalizedURL,
		Host:       host,
		Repository: repository,
		Number:     number,
	}, true
}

// prLinkRoute chooses the last recognized route, including when a repository
// path itself contains a route-shaped segment.
func prLinkRoute(path string) (repository, number, remainder, routePath string, found bool) {
	for _, separator := range []string{"/pull/", "/pulls/", "/-/merge_requests/"} {
		prefix, suffix, ok := strings.CutLast(path, separator)
		if !ok || (found && len(prefix) <= len(repository)) {
			continue
		}
		repository, found = prefix, true
		number, remainder, _ = strings.Cut(suffix, "/")
		routePath = prefix + separator + number
	}
	return
}

// prLinkCollector deduplicates links by PR identity, keeping first-appearance
// order.
type prLinkCollector struct {
	links []PRLink
	seen  map[prLinkIdentity]struct{}
}

type prLinkIdentity struct {
	host, repository string
	number           int
}

func (c *prLinkCollector) add(link PRLink) {
	key := prLinkIdentity{strings.ToLower(link.Host), strings.ToLower(link.Repository), link.Number}
	if _, ok := c.seen[key]; ok {
		return
	}
	if c.seen == nil {
		c.seen = make(map[prLinkIdentity]struct{})
	}
	c.seen[key] = struct{}{}
	c.links = append(c.links, link)
}

func (c *prLinkCollector) result() []PRLink {
	if len(c.links) == 0 {
		return nil
	}
	return c.links
}

// MergePRLinks combines link lists with the collector's deduplication.
func MergePRLinks(lists ...[]PRLink) []PRLink {
	var c prLinkCollector
	for _, list := range lists {
		for _, link := range list {
			c.add(link)
		}
	}
	return c.result()
}

var (
	promptURL      = regexp.MustCompile(`(?i)https?://[^\s<>"'()\[\]]+`)
	promptURLStart = regexp.MustCompile(`(?i)https?://`)
)

// PromptPRLinks extracts pull and merge request URLs from text.
func PromptPRLinks(text string) []PRLink {
	var links prLinkCollector
	starts := standalonePromptURLStarts(text)
	for i, start := range starts {
		end := len(text)
		if i+1 < len(starts) {
			end = starts[i+1]
		}
		part := text[start:end]
		candidate := promptURLToken(part)
		if i+1 < len(starts) && len(candidate) == len(part) {
			candidate = strings.TrimRightFunc(candidate, func(r rune) bool {
				return unicode.IsPunct(r) || unicode.IsSymbol(r)
			})
		}
		candidate = strings.TrimRightFunc(candidate, func(r rune) bool {
			return strings.ContainsRune(".,;:!?]`*_", r) || r > unicode.MaxASCII && unicode.IsPunct(r)
		})
		u, err := url.Parse(candidate)
		if err != nil {
			continue
		}
		repository, num, routePath, found := promptPRRoute(u.Path)
		if !found || strings.Trim(num, "0123456789") != "" {
			continue
		}
		n, err := strconv.Atoi(num)
		if err != nil {
			continue
		}
		u.Path = routePath
		u.RawPath, u.RawQuery, u.Fragment = "", "", ""
		if link, ok := NewPRLink(u.String(), repository, n); ok {
			links.add(link)
		}
	}
	return links.result()
}

// promptURLToken returns the URL that starts text, ending at the first
// character a URL cannot contain or at any Unicode whitespace.
func promptURLToken(text string) string {
	token := promptURL.FindString(text)
	if n := strings.IndexFunc(token, unicode.IsSpace); n >= 0 {
		token = token[:n]
	}
	return token
}

// standalonePromptURLStarts returns the offsets of URLs that do not start
// inside another URL. A URL inside an earlier URL's query or fragment, or
// directly after a "/" or "=" in its path, is part of that URL. Any other
// URL glued to the previous one, such as "pull/1https://" or
// "pull/1,https://", starts a new link.
func standalonePromptURLStarts(text string) []int {
	var starts []int
	enclosingStart, enclosingEnd := 0, 0
	for _, match := range promptURLStart.FindAllStringIndex(text, -1) {
		start := match[0]
		if start < enclosingEnd {
			enclosing := text[enclosingStart:start]
			if strings.ContainsAny(enclosing, "?#") || strings.HasSuffix(enclosing, "/") || strings.HasSuffix(enclosing, "=") {
				continue
			}
		}
		starts = append(starts, start)
		enclosingStart, enclosingEnd = start, start+len(promptURLToken(text[start:]))
	}
	return starts
}

// promptPRRoute finds the pull or merge request route in a prompt URL path.
// "/pull/" and "/pulls/" must follow exactly owner/repo, as on GitHub, Gitea,
// and Forgejo. Only GitLab's "/-/merge_requests/" may follow a nested group
// path; GitLab reserves "-" as a path segment, so the first "-" ends the
// project path. Any segments after the number, such as "/files", are a
// subpage of the request.
func promptPRRoute(path string) (repository, number, routePath string, found bool) {
	segments := strings.Split(strings.TrimPrefix(path, "/"), "/")
	repoLen := slices.Index(segments, "-")
	var numberAt int
	switch {
	case repoLen >= 0 && repoLen+2 < len(segments) && segments[repoLen+1] == "merge_requests":
		numberAt = repoLen + 2
	case len(segments) > 3 && (segments[2] == "pull" || segments[2] == "pulls"):
		repoLen, numberAt = 2, 3
	default:
		return "", "", "", false
	}
	if repoLen < 2 || slices.Contains(segments[:repoLen], "") {
		return "", "", "", false
	}
	return strings.Join(segments[:repoLen], "/"), segments[numberAt],
		"/" + strings.Join(segments[:numberAt+1], "/"), true
}
