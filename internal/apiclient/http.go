package apiclient

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
)

// NewHTTPClient connects the generated API to the selected daemon transport.
func NewHTTPClient(baseURL, token string, client *http.Client) (*Client, error) {
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

// TrackArchiveFreshness collects stale responses for one operation.
func TrackArchiveFreshness(ctx context.Context) (context.Context, func() bool) {
	freshness := &atomic.Bool{}
	return context.WithValue(ctx, archiveFreshnessKey{}, freshness), freshness.Load
}

// ObserveArchiveFreshness records freshness for successful raw HTTP responses.
func ObserveArchiveFreshness(ctx context.Context, response *http.Response) {
	if response == nil || response.StatusCode < 200 || response.StatusCode >= 300 || response.Header.Get(ArchiveStaleHeader) != "true" {
		return
	}
	if freshness, ok := ctx.Value(archiveFreshnessKey{}).(*atomic.Bool); ok {
		freshness.Store(true)
	}
}
