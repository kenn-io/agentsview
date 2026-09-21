package rawclient

import (
	"context"
	"errors"
	"sync"
	"time"

	"go.kenn.io/agentsview/internal/apiclient"
)

// tokenScopes covers this client's upload operations. Hosted status callers
// request the status scope separately through the token endpoint.
var tokenScopes = []string{"negotiate", "upload", "commit"}

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
// it. It goes through request, never do: do prefetches an avdt token and
// would recurse into this provider.
func (p *tokenProvider) exchange(ctx context.Context) (string, error) {
	response, err := p.client.request(func(api *apiclient.Client) (*apiclient.PostAPIV1RawSyncTokensResp, error) {
		return api.PostAPIV1RawSyncTokensWithResponse(ctx, &apiclient.PostAPIV1RawSyncTokensRequestOptions{
			Body:   &apiclient.RawSyncTokenInputBody{Scopes: tokenScopes},
			Header: &apiclient.PostAPIV1RawSyncTokensHeaders{Authorization: new("Bearer " + p.credential), XAgentsViewDeviceID: new(p.deviceID)},
		})
	}, "")
	if err != nil {
		return "", err
	}
	issued := response.JSON200
	if issued.Token == "" || issued.DeviceID != p.deviceID {
		return "", errors.New("rawclient: token response identity mismatch")
	}
	p.mu.Lock()
	p.current = issued.Token
	p.expires = issued.ExpiresAt
	p.mu.Unlock()
	return issued.Token, nil
}
