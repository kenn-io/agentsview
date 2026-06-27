package artifact

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	artifactAPIPath        = "/api/v1/artifacts"
	httpTransportTimeout   = 120 * time.Second
	httpTransportMaxErrLen = 512
)

// IsHTTPTarget reports whether target is an HTTP(S) peer URL rather than a local
// folder target.
func IsHTTPTarget(target string) bool {
	return strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://")
}

// httpTransport exchanges artifacts with a remote agentsview peer over its
// authenticated HTTP artifact API: list origins, list an origin's artifact
// index, get an artifact by name, and post an artifact. Content addressing makes
// "does the peer have X" a name-set comparison, so exchange is a stateless,
// idempotent set-union in both directions.
type httpTransport struct {
	base   string
	origin string
	token  string
	client *http.Client
}

func newHTTPTransport(target, token string) (*httpTransport, error) {
	u, err := url.Parse(strings.TrimRight(target, "/"))
	if err != nil {
		return nil, fmt.Errorf("parsing peer URL %q: %w", target, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("peer target must be http:// or https://: %q", target)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("peer target is missing a host: %q", target)
	}
	base := strings.TrimRight(u.String(), "/")
	if !strings.HasSuffix(base, artifactAPIPath) {
		base += artifactAPIPath
	}
	peerOrigin := (&url.URL{Scheme: u.Scheme, Host: u.Host}).String()
	return &httpTransport{
		base:   base,
		origin: peerOrigin,
		token:  token,
		client: &http.Client{Timeout: httpTransportTimeout},
	}, nil
}

// Prepare validates that the peer is reachable and authenticated before the
// local export runs, so a wrong URL or token fails fast.
func (t *httpTransport) Prepare(_ string) error {
	if _, err := t.listOrigins(context.Background()); err != nil {
		return fmt.Errorf("connecting to artifact peer: %w", err)
	}
	return nil
}

func (t *httpTransport) Exchange(ctx context.Context, localRoot string) error {
	if err := t.pull(ctx, localRoot); err != nil {
		return fmt.Errorf("fetching artifacts from peer: %w", err)
	}
	if err := t.push(ctx, localRoot); err != nil {
		return fmt.Errorf("publishing artifacts to peer: %w", err)
	}
	return nil
}

// pull fetches every artifact the peer holds that is missing locally.
func (t *httpTransport) pull(ctx context.Context, localRoot string) error {
	origins, err := t.listOrigins(ctx)
	if err != nil {
		return err
	}
	for _, origin := range origins {
		remote, err := t.getIndex(ctx, origin)
		if err != nil {
			return err
		}
		local, err := ListArtifacts(localRoot, origin)
		if err != nil {
			return err
		}
		for _, item := range missingItems(remote, local) {
			data, err := t.getArtifact(ctx, origin, item.kind, item.name)
			if err != nil {
				return err
			}
			if _, err := WriteArtifact(localRoot, origin, item.kind, item.name, data); err != nil {
				return err
			}
		}
	}
	return nil
}

// push uploads every local artifact the peer is missing.
func (t *httpTransport) push(ctx context.Context, localRoot string) error {
	origins, err := ListOrigins(localRoot)
	if err != nil {
		return err
	}
	for _, origin := range origins {
		local, err := ListArtifacts(localRoot, origin)
		if err != nil {
			return err
		}
		remote, err := t.getIndex(ctx, origin)
		if err != nil {
			return err
		}
		for _, item := range missingItems(local, remote) {
			art, err := ReadArtifact(localRoot, origin, item.kind, item.name)
			if err != nil {
				return err
			}
			if err := t.postArtifact(ctx, origin, item.kind, item.name, art.Data); err != nil {
				return err
			}
		}
	}
	return nil
}

type artifactItem struct {
	kind string
	name string
}

func indexItems(idx OriginArtifactIndex) []artifactItem {
	items := make([]artifactItem, 0,
		len(idx.Checkpoints)+len(idx.Manifests)+len(idx.Segments)+len(idx.Meta)+len(idx.Raw))
	for _, group := range []struct {
		kind  string
		names []string
	}{
		{KindCheckpoints, idx.Checkpoints},
		{KindManifests, idx.Manifests},
		{KindSegments, idx.Segments},
		{KindMeta, idx.Meta},
		{KindRaw, idx.Raw},
	} {
		for _, name := range group.names {
			items = append(items, artifactItem{kind: group.kind, name: name})
		}
	}
	return items
}

// missingItems returns artifacts present in have but absent from other.
func missingItems(have, other OriginArtifactIndex) []artifactItem {
	present := make(map[artifactItem]struct{})
	for _, it := range indexItems(other) {
		present[it] = struct{}{}
	}
	var out []artifactItem
	for _, it := range indexItems(have) {
		if _, ok := present[it]; !ok {
			out = append(out, it)
		}
	}
	return out
}

func (t *httpTransport) listOrigins(ctx context.Context) ([]string, error) {
	var resp struct {
		Origins []string `json:"origins"`
	}
	if err := t.getJSON(ctx, t.base+"/origins", &resp); err != nil {
		return nil, err
	}
	return resp.Origins, nil
}

func (t *httpTransport) getIndex(ctx context.Context, origin string) (OriginArtifactIndex, error) {
	var idx OriginArtifactIndex
	if err := t.getJSON(ctx, t.base+"/"+url.PathEscape(origin)+"/index", &idx); err != nil {
		return OriginArtifactIndex{}, err
	}
	return idx, nil
}

func (t *httpTransport) getArtifact(ctx context.Context, origin, kind, name string) ([]byte, error) {
	u := t.base + "/" + url.PathEscape(origin) + "/" + url.PathEscape(kind) + "/" + url.PathEscape(name)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	t.authorize(req)
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, httpStatusError(resp)
	}
	return io.ReadAll(resp.Body)
}

func (t *httpTransport) postArtifact(ctx context.Context, origin, kind, name string, data []byte) error {
	u := t.base + "/" + url.PathEscape(origin) + "/" + url.PathEscape(kind) + "/" + url.PathEscape(name)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Origin", t.origin)
	t.authorize(req)
	resp, err := t.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return httpStatusError(resp)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

func (t *httpTransport) getJSON(ctx context.Context, u string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	t.authorize(req)
	resp, err := t.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return httpStatusError(resp)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (t *httpTransport) authorize(req *http.Request) {
	if t.token != "" {
		req.Header.Set("Authorization", "Bearer "+t.token)
	}
}

func httpStatusError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, httpTransportMaxErrLen))
	msg := strings.TrimSpace(string(body))
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("%w: peer rejected the bearer token (401)", errHTTPPeer)
	}
	if msg == "" {
		return fmt.Errorf("%w: %s", errHTTPPeer, resp.Status)
	}
	return fmt.Errorf("%w: %s: %s", errHTTPPeer, resp.Status, msg)
}

var errHTTPPeer = errors.New("artifact peer request failed")
