package docbank

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"uuid"

	"go.kenn.io/agentsview/internal/config"
)

const (
	maxPageBytes = 1 << 20
	// cacheTTL bounds how long a successful answer is reused. An open live
	// session refreshes its lookups as the transcript grows, and most of its
	// digests do not change between refreshes.
	cacheTTL        = time.Minute
	maxCacheEntries = 4096
)

type Client struct {
	http *http.Client
	cfg  config.DocbankIntegration
	now  func() time.Time

	mu    sync.Mutex
	cache map[string]cachedResult
}

type cachedResult struct {
	result  Result
	expires time.Time
}

// New builds a client from configuration validated by the local backend.
func New(cfg config.DocbankIntegration) *Client {
	return &Client{cfg: cfg, now: time.Now, cache: map[string]cachedResult{}, http: &http.Client{Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

type Match struct {
	State   string `json:"state" enum:"current,prior,trash"`
	Name    string `json:"name"`
	Path    string `json:"path"`
	OpenURL string `json:"open_url,omitempty"`
}

type Result struct {
	Matches []Match `json:"matches"`
	Total   int     `json:"total"`
}

type remotePage struct {
	Items *[]struct {
		Version struct {
			ID uuid.UUID `json:"id"`
		} `json:"version"`
		Node struct {
			ID      int64  `json:"id"`
			Name    string `json:"name"`
			Trashed string `json:"trashed_at"`
		} `json:"node"`
		Path    string `json:"path"`
		Current bool   `json:"is_current"`
	} `json:"items"`
	Total *int `json:"total"`
}

// Lookup reads current references for a digest from Docbank. Successful
// answers are reused for cacheTTL; failures are never cached.
func (c *Client) Lookup(ctx context.Context, digest string) (Result, error) {
	if result, ok := c.cached(digest); ok {
		return result, nil
	}
	result, err := c.fetch(ctx, digest)
	if err == nil {
		c.store(digest, result)
	}
	return result, err
}

func (c *Client) cached(digest string) (Result, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.cache[digest]
	if !ok || !c.now().Before(entry.expires) {
		return Result{}, false
	}
	return entry.result, true
}

func (c *Client) store(digest string, result Result) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if len(c.cache) >= maxCacheEntries {
		for key, entry := range c.cache {
			if !now.Before(entry.expires) {
				delete(c.cache, key)
			}
		}
		if len(c.cache) >= maxCacheEntries {
			clear(c.cache)
		}
	}
	c.cache[digest] = cachedResult{result: result, expires: now.Add(cacheTTL)}
}

func (c *Client) fetch(ctx context.Context, digest string) (Result, error) {
	unavailable := errors.New("docbank unavailable")
	key := c.cfg.APIKey()
	if key == "" {
		return Result{}, lookupUnavailable("missing API key")
	}
	params := url.Values{"sha256": {digest}, "limit": {"10"}, "offset": {"0"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(strings.TrimSpace(c.cfg.URL), "/")+"/api/v1/content-references?"+params.Encode(), nil)
	if err != nil {
		return Result{}, unavailable
	}
	req.Header.Set("X-Api-Key", key)
	resp, err := c.http.Do(req)
	if err != nil {
		return Result{}, lookupUnavailable("server unreachable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Result{}, lookupStatusUnavailable(resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxPageBytes+1))
	if err != nil || len(data) > maxPageBytes {
		return Result{}, unavailable
	}
	var raw remotePage
	if json.Unmarshal(data, &raw) != nil || raw.Items == nil || raw.Total == nil || *raw.Total < len(*raw.Items) || len(*raw.Items) > 10 {
		return Result{}, unavailable
	}
	out := Result{Total: *raw.Total, Matches: []Match{}}
	for _, item := range *raw.Items {
		state, path := "prior", item.Path
		if item.Current {
			state = "current"
		}
		if item.Node.Trashed != "" {
			state, path = "trash", ""
		}
		open := ""
		if c.cfg.WebURL != "" {
			open = strings.TrimRight(strings.TrimSpace(c.cfg.WebURL), "/") + "/#node=" + strconv.FormatInt(item.Node.ID, 10) + "&version=" + item.Version.ID.String()
		}
		out.Matches = append(out.Matches, Match{State: state, Name: item.Node.Name, Path: path, OpenURL: open})
	}
	return out, nil
}

func lookupUnavailable(cause string) error {
	log.Printf("Docbank lookup unavailable: %s", cause)
	return errors.New("docbank unavailable")
}

func lookupStatusUnavailable(status int) error {
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return lookupUnavailable("API key rejected")
	}
	return lookupUnavailable(fmt.Sprintf("server returned HTTP %d", status))
}
