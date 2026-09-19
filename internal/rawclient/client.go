// Package rawclient is the laptop-side HTTP client for the authenticated
// raw-ingest transport: scoped-token exchange, missing-object negotiation,
// resumable object uploads, and manifest-last commits.
package rawclient

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/agentsview/internal/apiclient"
	"go.kenn.io/agentsview/internal/rawsync"
)

// Error codes produced by the raw-sync HTTP surface.
const (
	CodeUnauthorized     = "unauthorized"
	CodeInvalidRequest   = "invalid_request"
	CodeNotFound         = "not_found"
	CodeConflict         = "conflict"
	CodeMissingObject    = "missing_object"
	CodeHeadConflict     = "head_conflict"
	CodeUploadOffset     = "upload_offset_conflict"
	CodeChecksumMismatch = "checksum_mismatch"
	// Gateway timeouts must be matched on HTTP Status 504, not this code field.
	CodeGatewayTimeout = "gateway_timeout"
	CodeInternal       = "internal_error"
)

const (
	defaultChunkBytes  = int64(4 << 20) // matches rawsync.DefaultUploadChunkBytes
	defaultTokenMargin = time.Minute
)

// maxErrorBodyBytes caps how much of an error response body is read before
// decoding it; larger bodies are truncated, never buffered whole.
const maxErrorBodyBytes = 64 << 10

// APIError is a decoded raw-sync API error response.
type APIError struct {
	Status              int    `json:"-"`
	Code                string `json:"code,omitempty"`
	Message             string `json:"error"`
	CurrentManifestID   string `json:"current_manifest_id,omitempty"`
	CurrentReceipt      string `json:"current_receipt,omitempty"`
	CurrentGeneration   int64  `json:"current_generation,omitzero"`
	CurrentUploadOffset *int64 `json:"upload_offset,omitempty"`
}

func (e *APIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("raw sync api error %d %s: %s", e.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("raw sync api error %d: %s", e.Status, e.Message)
}

// AsAPIError reports whether err carries a decoded API error, copying it into
// out when it does.
func AsAPIError(err error, out *APIError) bool {
	if apiErr, ok := errors.AsType[*APIError](err); ok {
		*out = *apiErr
		return true
	}
	return false
}

// decodeAPIError converts a non-2xx response body into an *APIError. Bodies
// that do not decode as a wire error still report the status under
// CodeInternal, so transport failures never vanish.
func decodeAPIError(status int, body []byte) error {
	apiErr := &APIError{Status: status}
	var wire apiclient.APIErrorResponse
	if err := json.Unmarshal(body, &wire); err == nil && wire.ErrorData != "" {
		if wire.Code != nil {
			apiErr.Code = *wire.Code
		}
		apiErr.Message = wire.ErrorData
		if wire.CurrentManifestID != nil {
			apiErr.CurrentManifestID = *wire.CurrentManifestID
		}
		if wire.CurrentReceipt != nil {
			apiErr.CurrentReceipt = *wire.CurrentReceipt
		}
		if wire.CurrentGeneration != nil {
			apiErr.CurrentGeneration = *wire.CurrentGeneration
		}
		apiErr.CurrentUploadOffset = wire.UploadOffset
	} else {
		apiErr.Code = CodeInternal
		apiErr.Message = "raw sync request failed"
	}
	return apiErr
}

// Config constructs a Client. Credential holds the injected avdc_ device
// credential; the client never persists or logs it.
type Config struct {
	BaseURL     string
	DeviceID    string
	Credential  string
	HTTPClient  *http.Client
	ChunkBytes  int64
	TokenMargin time.Duration
}

// Client talks to one agentsview server's raw-sync surface for one device.
type Client struct {
	baseURL    *url.URL
	httpClient *http.Client
	tokens     *tokenProvider
	chunkBytes int64
}

// NewClient validates configuration and returns a Client that authenticates
// each do request with scoped avdt_ bearer tokens, exchanged on demand for
// the device credential. A zero TokenMargin falls back to defaultTokenMargin.
func NewClient(cfg Config) (*Client, error) {
	base, err := url.Parse(strings.TrimRight(cfg.BaseURL, "/"))
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("rawclient: invalid base URL %q", cfg.BaseURL)
	}
	if cfg.DeviceID == "" || cfg.Credential == "" {
		return nil, errors.New("rawclient: device ID and credential are required")
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{
			Timeout:       10 * time.Minute,
			CheckRedirect: refuseRedirects,
		}
	} else {
		// Always replace the caller's policy: 307 and 308 responses can
		// replay the token-exchange body containing the device credential.
		enforced := *httpClient
		enforced.CheckRedirect = refuseRedirects
		httpClient = &enforced
	}
	chunkBytes := cfg.ChunkBytes
	if chunkBytes <= 0 {
		chunkBytes = defaultChunkBytes
	}
	if chunkBytes > rawsync.DefaultUploadChunkBytes {
		chunkBytes = rawsync.DefaultUploadChunkBytes
	}
	client := &Client{
		baseURL:    base,
		httpClient: httpClient,
		chunkBytes: chunkBytes,
	}
	margin := cfg.TokenMargin
	if margin <= 0 {
		margin = defaultTokenMargin
	}
	client.tokens = newTokenProvider(client, cfg.DeviceID, cfg.Credential, margin)
	return client, nil
}

func refuseRedirects(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

// do invokes a generated operation, retrying once with a refreshed scoped
// token after an unauthorized response.
func (c *Client) do[T any](ctx context.Context, operation func(*apiclient.Client) (T, error)) (T, error) {
	var zero T
	for attempt := 0; ; attempt++ {
		token, err := c.tokens.token(ctx)
		if err != nil {
			return zero, err
		}
		resp, err := c.request(operation, token)
		if err == nil {
			return resp, nil
		}
		var apiErr APIError
		if attempt >= 1 || !AsAPIError(err, &apiErr) || apiErr.Status != http.StatusUnauthorized {
			return zero, err
		}
		c.tokens.invalidate()
	}
}

func (c *Client) request[T any](operation func(*apiclient.Client) (T, error), token string) (T, error) {
	var zero T
	api, err := apiclient.NewDefaultClient(c.baseURL.String(),
		runtime.WithHTTPClient(rawHTTPTransport{c.httpClient}),
		runtime.WithRequestEditorFn(func(_ context.Context, req *http.Request) error {
			if token != "" {
				req.Header.Set("Authorization", "Bearer "+token)
			}
			return nil
		}))
	if err != nil {
		return zero, err
	}
	return operation(api)
}

// Bound error reads before the generated runtime buffers the response. Successful
// responses pass through to its generated JSON decoder.
type rawHTTPTransport struct{ *http.Client }

func (t rawHTTPTransport) Do(ctx context.Context, req *http.Request) (*http.Response, error) {
	resp, err := t.Client.Do(req.WithContext(ctx))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		return nil, decodeAPIError(resp.StatusCode, body)
	}
	return resp, nil
}
