// Package rawclient is the laptop-side HTTP client for the authenticated
// raw-ingest transport: scoped-token exchange, missing-object negotiation,
// resumable object uploads, and manifest-last commits.
package rawclient

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/rawsync"
)

// octetBody marks a raw pre-encoded request body. doOnce sends it verbatim
// instead of JSON-encoding it, so octet-stream uploads reuse the same
// authenticated retry path as JSON requests.
type octetBody struct {
	data []byte
}

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

// wireError mirrors the server's error response shape. The server always
// sets "error", so an empty message means the body was not an API error.
type wireError struct {
	Code                string `json:"code,omitempty"`
	Message             string `json:"error"`
	CurrentManifestID   string `json:"current_manifest_id,omitempty"`
	CurrentReceipt      string `json:"current_receipt,omitempty"`
	CurrentGeneration   int64  `json:"current_generation,omitzero"`
	CurrentUploadOffset *int64 `json:"upload_offset,omitempty"`
}

// decodeAPIError converts a non-2xx response body into an *APIError. Bodies
// that do not decode as a wire error still report the status under
// CodeInternal, so transport failures never vanish.
func decodeAPIError(status int, body []byte) error {
	apiErr := &APIError{Status: status}
	var wire wireError
	if err := json.Unmarshal(body, &wire); err == nil && wire.Message != "" {
		apiErr.Code = wire.Code
		apiErr.Message = wire.Message
		apiErr.CurrentManifestID = wire.CurrentManifestID
		apiErr.CurrentReceipt = wire.CurrentReceipt
		apiErr.CurrentGeneration = wire.CurrentGeneration
		apiErr.CurrentUploadOffset = wire.CurrentUploadOffset
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
		return nil, fmt.Errorf("rawclient: device ID and credential are required")
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

// do performs one authenticated JSON request, retrying exactly once with a
// refreshed token after an unauthorized response. The request body, when
// non-nil, is encoded with encoding/json/v2 semantics.
func (c *Client) do(
	ctx context.Context,
	method string,
	path string,
	header http.Header,
	body any,
) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		token, err := c.tokens.token(ctx)
		if err != nil {
			return nil, err
		}
		resp, err := c.doOnce(ctx, method, path, header, body, token)
		if err == nil {
			return resp, nil
		}
		var apiErr APIError
		if attempt >= 1 ||
			!AsAPIError(err, &apiErr) || apiErr.Status != http.StatusUnauthorized {
			return nil, err
		}
		c.tokens.invalidate()
	}
}

// doOnce sends a single request with the given bearer token. It returns the
// response for 2xx statuses; any other status becomes an *APIError decoded
// from at most maxErrorBodyBytes of the body.
func (c *Client) doOnce(
	ctx context.Context,
	method string,
	path string,
	header http.Header,
	body any,
	token string,
) (*http.Response, error) {
	var payload io.Reader
	if body != nil {
		// octetBody bypasses JSON encoding: the caller already serialized
		// the payload and set its Content-Type header.
		if raw, ok := body.(octetBody); ok {
			payload = bytes.NewReader(raw.data)
		} else {
			data, err := json.Marshal(body)
			if err != nil {
				return nil, fmt.Errorf("rawclient: encoding request body: %w", err)
			}
			payload = bytes.NewReader(data)
		}
	}
	req, err := http.NewRequestWithContext(
		ctx, method, c.baseURL.String()+path, payload,
	)
	if err != nil {
		return nil, err
	}
	for key, values := range header {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	if body != nil && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		_ = resp.Body.Close()
		return nil, decodeAPIError(resp.StatusCode, errBody)
	}
	return resp, nil
}

// rawRequest sends one JSON request without client-managed authentication:
// the caller supplies its own Authorization header. The device-credential
// token exchange uses this path so it never recurses through do's
// scoped-token handling.
func (c *Client) rawRequest(
	ctx context.Context,
	method string,
	path string,
	header http.Header,
	body any,
) (*http.Response, error) {
	return c.doOnce(ctx, method, path, header, body, "")
}

// jsonDecode decodes a JSON response body into dst with encoding/json/v2.
func jsonDecode(body io.Reader, dst any) error {
	return json.UnmarshalRead(body, dst)
}
