package parser

import (
	"context"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
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

// CombinePRLinks combines links to the same pull request for display and search.
func CombinePRLinks(lists ...[]PRLink) []PRLink {
	seen := make(map[prLinkIdentity]struct{})
	var result []PRLink
	for _, list := range lists {
		for _, link := range list {
			key := prLinkIdentity{strings.ToLower(link.Host), strings.ToLower(link.Repository), link.Number}
			if _, ok := seen[key]; !ok {
				seen[key] = struct{}{}
				result = append(result, link)
			}
		}
	}
	return result
}

type prLinkIdentity struct {
	host, repository string
	number           int
}

// PromptPRLinks extracts pull and merge request URLs from text.
func PromptPRLinks(text string) []PRLink {
	links, _ := promptPRLinks(context.Background(), text)
	return links
}

// PromptPRLinksContext permits callers holding locks to interrupt scanning.
func PromptPRLinksContext(ctx context.Context, text string) ([]PRLink, error) {
	return promptPRLinks(ctx, text)
}

func promptPRLinks(ctx context.Context, text string) ([]PRLink, error) {
	var links prLinkCollector
	for pos := 0; pos < len(text); {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		start := pos
		checkAt := pos + 4096
		for pos < len(text) {
			r, n := utf8.DecodeRuneInString(text[pos:])
			if unicode.IsSpace(r) || strings.ContainsRune("<>\"'()[]", r) {
				break
			}
			pos += n
			if pos >= checkAt {
				checkAt = pos + 4096
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
		}
		token := text[start:pos]
		var starts []int
		query := false
		for i := range len(token) {
			if i&4095 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			if token[i] == '?' || token[i] == '#' {
				query = true
			}
			if (len(token)-i >= 7 && strings.EqualFold(token[i:i+7], "http://")) || (len(token)-i >= 8 && strings.EqualFold(token[i:i+8], "https://")) {
				if len(starts) == 0 || (!query && token[i-1] != '/' && token[i-1] != '=') {
					starts = append(starts, i)
					query = false
				}
			}
		}
		for i, start := range starts {
			if i&255 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			end := len(token)
			if i+1 < len(starts) {
				end = starts[i+1]
			}
			candidate := token[start:end]
			if i+1 < len(starts) {
				candidate = strings.TrimRightFunc(candidate, func(r rune) bool { return unicode.IsPunct(r) || unicode.IsSymbol(r) })
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
		if pos < len(text) {
			_, n := utf8.DecodeRuneInString(text[pos:])
			pos += n
		}
	}
	return CombinePRLinks(links.result()), nil
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
