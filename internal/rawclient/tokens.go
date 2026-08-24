package rawclient

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// tokenScopes is the exact scope set the transport needs; the status scope
// has no server route yet and is not requested.
var tokenScopes = []string{"negotiate", "upload", "commit"}

type tokenResponse struct {
	Token     string    `json:"token"`
	DeviceID  string    `json:"device_id"`
	Scopes    []string  `json:"scopes"`
	ExpiresAt time.Time `json:"expires_at"`
}

// tokenProvider caches one live device token and refreshes it with
// single-flight semantics before the server-side expiry margin.
type tokenProvider struct {
	client     *Client
	deviceID   string
	credential string
	margin     time.Duration

	mu      sync.Mutex
	current string
	expires time.Time
	refresh chan struct{}
}

func newTokenProvider(
	client *Client,
	deviceID, credential string,
	margin time.Duration,
) *tokenProvider {
	return &tokenProvider{
		client: client, deviceID: deviceID, credential: credential,
		margin: margin, refresh: make(chan struct{}, 1),
	}
}

// token returns a bearer token valid beyond the configured margin. Concurrent
// callers share one in-flight exchange.
func (p *tokenProvider) token(ctx context.Context) (string, error) {
	token, ok := p.cached()
	if ok {
		return token, nil
	}

	select {
	case p.refresh <- struct{}{}:
		defer func() { <-p.refresh }()
	case <-ctx.Done():
		return "", ctx.Err()
	}

	// Re-check after winning the refresh slot: another caller may have
	// completed the exchange while this one waited.
	token, ok = p.cached()
	if ok {
		return token, nil
	}
	return p.exchange(ctx)
}

// invalidate drops the cached token after an unauthorized response.
func (p *tokenProvider) invalidate() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.current = ""
	p.expires = time.Time{}
}

// cached returns the current token when it is still valid beyond the margin.
func (p *tokenProvider) cached() (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.current == "" || !time.Now().Add(p.margin).Before(p.expires) {
		return "", false
	}
	return p.current, true
}

// exchange trades the device credential for a fresh scoped token and caches
// it. It goes through rawRequest, never do: do prefetches an avdt token and
// would recurse into this provider.
func (p *tokenProvider) exchange(ctx context.Context) (string, error) {
	body := struct {
		Scopes []string `json:"scopes"`
	}{Scopes: tokenScopes}
	resp, err := p.client.rawRequest(ctx, http.MethodPost, "/api/v1/raw-sync/tokens",
		http.Header{
			"Authorization":          []string{"Bearer " + p.credential},
			"X-AgentsView-Device-ID": []string{p.deviceID},
		}, body)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var issued tokenResponse
	if err := jsonDecode(resp.Body, &issued); err != nil {
		return "", fmt.Errorf("rawclient: decode token response: %w", err)
	}
	if issued.Token == "" || issued.DeviceID != p.deviceID {
		return "", fmt.Errorf("rawclient: token response identity mismatch")
	}
	p.mu.Lock()
	p.current = issued.Token
	p.expires = issued.ExpiresAt
	p.mu.Unlock()
	return issued.Token, nil
}
