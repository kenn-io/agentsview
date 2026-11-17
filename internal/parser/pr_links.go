package parser

import (
	"net/url"
	"regexp"
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

// prLinkCollector deduplicates links by URL, keeping first-appearance
// order.
type prLinkCollector struct {
	links []PRLink
	seen  map[string]struct{}
}

func (c *prLinkCollector) add(link PRLink) {
	if _, ok := c.seen[link.URL]; ok {
		return
	}
	if c.seen == nil {
		c.seen = make(map[string]struct{})
	}
	c.seen[link.URL] = struct{}{}
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
	promptURL      = regexp.MustCompile(`https?://(?:\[[^\]\s]+\]|[^\s<>"'()/\[\]]+)[^\s<>"'()\[\]]*`)
	promptURLStart = regexp.MustCompile(`https?://`)
)

// PromptPRLinks extracts pull and merge request URLs from text.
func PromptPRLinks(text string) []PRLink {
	var links prLinkCollector
	starts := promptURLStart.FindAllStringIndex(text, -1)
	for i, start := range starts {
		end := len(text)
		if i+1 < len(starts) {
			end = starts[i+1][0]
		}
		part := text[start[0]:end]
		candidate := promptURL.FindString(part)
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
		repo, num, _, routePath, found := prLinkRoute(u.Path)
		repository := strings.TrimPrefix(repo, "/")
		if !found || strings.Count(repository, "/") < 1 || strings.Trim(num, "0123456789") != "" {
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
