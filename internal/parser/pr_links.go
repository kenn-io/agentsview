package parser

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"
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
	path := strings.TrimRight(u.Path, "/")
	var urlRepository, rawNumber string
	var routeFound bool
	for _, separator := range []string{"/pull/", "/pulls/", "/-/merge_requests/"} {
		if prefix, suffix, found := strings.CutLast(path, separator); found && (!routeFound || len(prefix) > len(urlRepository)) {
			urlRepository, rawNumber = prefix, suffix
			routeFound = true
		}
	}
	if routeFound {
		urlNumber, err := strconv.Atoi(rawNumber)
		if err != nil || urlNumber != number || !strings.EqualFold(strings.TrimPrefix(urlRepository, "/"), repository) {
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

var promptURL = regexp.MustCompile(`https?://[^\s<>"'()]+`)

// PromptPRLinks extracts pull and merge request URLs from text.
func PromptPRLinks(text string) []PRLink {
	var links prLinkCollector
	for _, candidate := range promptURL.FindAllString(text, -1) {
		candidate = strings.TrimRight(candidate, ".,;:!?")
		u, err := url.Parse(candidate)
		if err != nil || u.User != nil {
			continue
		}
		parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
		for i := 2; i+1 < len(parts); i++ {
			route := parts[i] == "pull" || parts[i] == "pulls"
			if parts[i] == "-" && i+2 < len(parts) && parts[i+1] == "merge_requests" {
				i++
				route = true
			}
			if !route {
				continue
			}
			num := parts[i+1]
			if num == "" || strings.Trim(num, "0123456789") != "" {
				continue
			}
			n, err := strconv.Atoi(num)
			if err != nil {
				continue
			}
			end := i
			if parts[i] == "merge_requests" {
				end--
			}
			repository := strings.Join(parts[:end], "/")
			u.Path = "/" + strings.Join(parts[:i+2], "/")
			u.RawPath, u.RawQuery, u.Fragment = "", "", ""
			if link, ok := NewPRLink(u.String(), repository, n); ok {
				links.add(link)
			}
			break
		}
	}
	return links.result()
}
