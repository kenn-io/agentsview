package apiclient

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/agentsview/internal/requestsign"
)

// NewHTTPClient connects the generated API to the selected daemon transport.
func NewHTTPClient(baseURL, token string, client *http.Client) (*Client, error) {
	client, err := requestsign.ClientFromEnvironment(baseURL, client)
	if err != nil {
		return nil, err
	}
	origin, err := url.Parse(baseURL)
	if err != nil {
		return nil, err
	}
	origin.Path, origin.RawPath, origin.RawQuery, origin.Fragment = "", "", "", ""
	return NewDefaultClient(strings.TrimSuffix(baseURL, "/"), runtime.WithHTTPClient(httpTransport{client}), runtime.WithRequestEditorFn(func(_ context.Context, req *http.Request) error {
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if req.Method != http.MethodGet {
			req.Header.Set("Origin", origin.String())
		}
		return nil
	}))
}

type httpTransport struct{ *http.Client }

func (c httpTransport) Do(ctx context.Context, req *http.Request) (*http.Response, error) {
	response, err := c.Client.Do(req.WithContext(ctx))
	ObserveArchiveFreshness(ctx, response)
	return response, err
}

const (
	ArchiveStaleHeader  = "X-Agentsview-Archive-Stale"
	ArchiveStaleWarning = "Archive results may be stale."
)

type archiveFreshnessKey struct{}

type archiveFreshness struct {
	stale   atomic.Bool
	onStale func()
}

// TrackArchiveFreshness collects stale responses and optionally calls onStale once.
func TrackArchiveFreshness(ctx context.Context, onStale func()) (context.Context, func() bool) {
	freshness := &archiveFreshness{onStale: onStale}
	return context.WithValue(ctx, archiveFreshnessKey{}, freshness), freshness.stale.Load
}

// ObserveArchiveFreshness records freshness for results, including missing records.
func ObserveArchiveFreshness(ctx context.Context, response *http.Response) {
	if response == nil || (response.StatusCode < 200 || response.StatusCode >= 300) && response.StatusCode != http.StatusNotFound || response.Header.Get(ArchiveStaleHeader) != "true" {
		return
	}
	if freshness, ok := ctx.Value(archiveFreshnessKey{}).(*archiveFreshness); ok && freshness.stale.CompareAndSwap(false, true) && freshness.onStale != nil {
		freshness.onStale()
	}
}
