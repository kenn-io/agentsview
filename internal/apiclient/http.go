package apiclient

import (
	"context"
	"net/http"
	"net/url"
	"strings"

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
	return c.Client.Do(req.WithContext(ctx))
}
