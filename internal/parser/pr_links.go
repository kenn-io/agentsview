package parser

import (
	"net/url"
	"strings"
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

// prLinkCollector deduplicates links by URL, keeping first-appearance
// order.
type prLinkCollector struct {
	links []PRLink
	index map[string]int
}

func (c *prLinkCollector) add(link PRLink) {
	if _, ok := c.index[link.URL]; ok {
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
