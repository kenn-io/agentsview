package docbank

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/agentsview/internal/config"
)

const maxPageBytes = 1 << 20

var (
	digestPattern  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	versionPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	errRemote      = errors.New("docbank unavailable")
	errDeferred    = errors.New("docbank deferred")
)

type client struct {
	http          *http.Client
	endpoint, web string
}

func newClient(cfg config.DocbankIntegration) *client {
	u, _ := url.Parse(cfg.URL)
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	tr.MaxConnsPerHost = 4
	tr.MaxIdleConnsPerHost = 4
	tr.MaxIdleConns = 4
	dialer := &net.Dialer{Timeout: 2 * time.Second}
	tr.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if !config.DocbankLoopbackHost(u.Hostname()) {
			return dialer.DialContext(ctx, network, address)
		}
		ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, errRemote
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil || len(ips) == 0 {
			return nil, errRemote
		}
		return dialLoopback(ctx, network, port, ips, dialer.DialContext)
	}
	return &client{http: &http.Client{Transport: tr, Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, endpoint: strings.TrimRight(cfg.URL, "/") + "/api/v1/content-references", web: strings.TrimRight(cfg.WebURL, "/")}
}

// dialLoopback validates the full resolution before dialing pinned addresses.
func dialLoopback(ctx context.Context, network, port string, ips []net.IPAddr, dial func(context.Context, string, string) (net.Conn, error)) (net.Conn, error) {
	if len(ips) == 0 {
		return nil, errRemote
	}
	for _, ip := range ips {
		if !ip.IP.IsLoopback() {
			return nil, errRemote
		}
	}
	var err error
	for _, ip := range ips {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var conn net.Conn
		conn, err = dial(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return nil, err
}

type remotePage struct {
	Items []struct {
		Version struct {
			ID     string `json:"id"`
			NodeID int64  `json:"node_id"`
			Hash   string `json:"blob_hash"`
			Size   *int64 `json:"size"`
		} `json:"version"`
		Node struct {
			ID      int64  `json:"id"`
			Kind    string `json:"kind"`
			Current string `json:"current_version_id"`
			Name    string `json:"name"`
			Trashed string `json:"trashed_at"`
		} `json:"node"`
		Path    string `json:"path"`
		Current *bool  `json:"is_current"`
	} `json:"items"`
	Total  *int `json:"total"`
	Limit  *int `json:"limit"`
	Offset *int `json:"offset"`
}

// fetch never returns unsanitized transport errors or response text.
func (c *client) fetch(ctx context.Context, q Lookup, key string, now time.Time) (Page, time.Duration) {
	unavailable := Page{State: "unavailable", CheckedAt: &now}
	params := url.Values{"sha256": {q.SHA256}, "limit": {"100"}, "offset": {strconv.Itoa(q.ReferenceOffset)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint+"?"+params.Encode(), nil)
	if err != nil {
		return unavailable, 0
	}
	req.Header.Set("X-Api-Key", key)
	resp, err := c.http.Do(req)
	if err != nil {
		return unavailable, 0
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		return unavailable, retryDelay(resp.Header.Get("Retry-After"), now)
	}
	if resp.StatusCode != http.StatusOK {
		return unavailable, 0
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxPageBytes+1))
	if err != nil || len(data) > maxPageBytes || !utf8.Valid(data) {
		return unavailable, 0
	}
	var generic any
	if json.Unmarshal(data, &generic) != nil || !boundedStrings(generic) {
		return unavailable, 0
	}
	var raw remotePage
	if json.Unmarshal(data, &raw) != nil || raw.Total == nil || raw.Limit == nil || raw.Offset == nil || *raw.Total < 0 || *raw.Limit != 100 || *raw.Offset != q.ReferenceOffset || raw.Items == nil || len(raw.Items) > 100 {
		return unavailable, 0
	}
	count := len(raw.Items)
	if q.ReferenceOffset < *raw.Total {
		if count == 0 || count > *raw.Total-q.ReferenceOffset {
			return unavailable, 0
		}
	} else if count != 0 {
		return unavailable, 0
	}
	out := Page{State: "found", CheckedAt: &now, ReferenceTotal: raw.Total}
	if *raw.Total == 0 && q.ReferenceOffset == 0 {
		out.State = "not_found"
	}
	seen := make(map[string]bool, count)
	for i, item := range raw.Items {
		v, n := item.Version, item.Node
		if n.ID <= 0 || n.ID > 9007199254740991 || n.Kind != "file" || v.NodeID != n.ID || !versionPattern.MatchString(v.ID) || !versionPattern.MatchString(n.Current) || v.Hash != q.SHA256 || v.Size == nil || *v.Size < 0 || *v.Size != q.Size || item.Current == nil || *item.Current != (v.ID == n.Current) {
			return unavailable, 0
		}
		state, path := "prior", item.Path
		if *item.Current {
			state = "current"
		}
		if n.Trashed != "" {
			if _, err := time.Parse(time.RFC3339Nano, n.Trashed); err != nil {
				return unavailable, 0
			}
			state = "trash"
			path = ""
		}
		pair := strconv.FormatInt(n.ID, 10) + ":" + v.ID
		if seen[pair] {
			continue
		}
		seen[pair] = true
		open := ""
		if c.web != "" {
			open = c.web + "/#node=" + strconv.FormatInt(n.ID, 10) + "&version=" + v.ID
		}
		out.Matches = append(out.Matches, Match{NodeID: n.ID, VersionID: v.ID, SHA256: v.Hash, Size: *v.Size, State: state, Name: n.Name, Path: path, OpenURL: open})
		out.MatchOffsets = append(out.MatchOffsets, q.ReferenceOffset+i)
	}
	if q.ReferenceOffset < *raw.Total && count < *raw.Total-q.ReferenceOffset {
		next := q.ReferenceOffset + count
		out.NextReferenceOffset = &next
	}
	return out, 0
}

func boundedStrings(v any) bool {
	switch x := v.(type) {
	case string:
		return len(x) <= 4096
	case []any:
		for _, e := range x {
			if !boundedStrings(e) {
				return false
			}
		}
	case map[string]any:
		for k, e := range x {
			if len(k) > 4096 || !boundedStrings(e) {
				return false
			}
		}
	}
	return true
}

func retryDelay(s string, now time.Time) time.Duration {
	seconds, err := strconv.ParseInt(s, 10, 64)
	d := 60 * time.Second
	if err == nil {
		if seconds <= 0 {
			return time.Second
		}
		if seconds < 60 {
			d = time.Duration(seconds) * time.Second
		}
	} else if date, e := http.ParseTime(s); e == nil {
		d = date.Sub(now)
	}
	if d > 60*time.Second {
		d = 60 * time.Second
	}
	if d < time.Second {
		d = time.Second
	}
	return d
}
