// Package kata is AgentsView's optional spoke client for a Kata issue tracker.
// It owns its HTTP transport and imports only Kata's generated API types.
package kata

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	kg "go.kenn.io/kata/pkg/client/generated"
)

const (
	DefaultTimeout                = 10 * time.Second
	DefaultMaxResponseBytes int64 = 1 << 20
	unixBaseURL                   = "http://kata.invalid"
	maxErrorMessageRunes          = 300
)

var (
	ErrUnavailable            = errors.New("kata unavailable")
	ErrIncompatible           = errors.New("kata incompatible")
	ErrWrongProject           = errors.New("kata project not found")
	ErrRedirect               = errors.New("kata redirect refused")
	ErrResponseTooLarge       = errors.New("kata response exceeds size limit")
	ErrInvalidResponse        = errors.New("kata response shape drifted")
	ErrIdempotencyKeyRequired = errors.New("kata idempotency key required")
	ErrNotReady               = errors.New("kata not ready")
)

// Config is the resolved connection configuration. Hub identifies the filing
// hub. TokenRequired means a token variable was named; a missing value must be
// rejected by its caller rather than silently making an anonymous request.
type Config struct {
	Enabled          bool
	Hub              bool
	Endpoint         string
	Token            string
	TokenEnv         string
	TokenRequired    bool
	Project          string
	Actor            string
	AllowInsecure    bool
	Timeout          time.Duration
	MaxResponseBytes int64
	HTTPClient       *http.Client
}

// APIError is a non-2xx Kata response. Code is the envelope's error.code.
type APIError struct {
	Status  int
	Code    string
	Message string
	Hint    string
	Data    jsontext.Value
}

func (e *APIError) Error() string {
	// Code, message and hint come from the remote server. Callers can inspect
	// their fields, but error strings may be logged or shown to users.
	return fmt.Sprintf("kata: HTTP %d", e.Status)
}

// IsCode reports whether err contains an APIError with the given code.
func IsCode(err error, code string) bool {
	apiErr, ok := errors.AsType[*APIError](err)
	return ok && apiErr.Code == code
}

// StatusOf returns the HTTP status of an APIError, or zero otherwise.
func StatusOf(err error) int {
	if apiErr, ok := errors.AsType[*APIError](err); ok {
		return apiErr.Status
	}
	return 0
}

// opaqueError preserves error identity without printing an underlying error.
// net/http errors can include the full URL, whose path may be a capability.
type opaqueError struct {
	message string
	causes  []error
}

func (e *opaqueError) Error() string   { return e.message }
func (e *opaqueError) Unwrap() []error { return e.causes }

// Client talks to one Kata instance and one project.
type Client struct {
	baseURL       string
	token         string
	tokenEnv      string
	tokenRequired bool
	actor         string
	project       string
	http          *http.Client
	maxBytes      int64

	mu          sync.RWMutex
	instanceUID string
	projectID   int64
	projectUID  string
}

func newClient(cfg Config, endpoint string) (*Client, error) {
	u, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil {
		return nil, fmt.Errorf("%w: endpoint is not a URL", ErrUnavailable)
	}
	if u.User != nil {
		return nil, fmt.Errorf("%w: endpoint must not contain URL credentials", ErrUnavailable)
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return nil, fmt.Errorf("%w: endpoint must not carry a query or fragment", ErrUnavailable)
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	maxBytes := cfg.MaxResponseBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxResponseBytes
	}
	var base, socket string
	switch u.Scheme {
	case "unix":
		if u.Host != "" || !strings.HasPrefix(u.Path, "/") {
			return nil, fmt.Errorf("%w: unix endpoint must be unix:///absolute/path", ErrUnavailable)
		}
		socket = u.Path
		if runtime.GOOS == "windows" {
			if windowsPath, ok := windowsUnixSocketPath(socket); ok {
				socket = windowsPath
			}
		}
		base = unixBaseURL
	case "https", "http":
		if u.Host == "" {
			return nil, fmt.Errorf("%w: endpoint has no host", ErrUnavailable)
		}
		if u.Scheme == "http" && !cfg.AllowInsecure && !isLoopback(u.Hostname()) {
			return nil, fmt.Errorf("%w: plaintext http to a non-loopback host needs allow_insecure", ErrUnavailable)
		}
		base = strings.TrimRight(u.Scheme+"://"+u.Host+u.Path, "/")
	default:
		return nil, fmt.Errorf("%w: endpoint must be unix://, https:// or http://", ErrUnavailable)
	}
	return &Client{
		baseURL:       base,
		token:         cfg.Token,
		tokenEnv:      strings.TrimSpace(cfg.TokenEnv),
		tokenRequired: cfg.TokenRequired || strings.TrimSpace(cfg.TokenEnv) != "",
		actor:         strings.TrimSpace(cfg.Actor),
		project:       strings.TrimSpace(cfg.Project),
		http:          buildHTTPClient(cfg.HTTPClient, socket, timeout),
		maxBytes:      maxBytes,
	}, nil
}

func windowsUnixSocketPath(path string) (string, bool) {
	if len(path) < 4 || path[0] != '/' || path[2] != ':' || path[3] != '/' {
		return path, false
	}
	drive := path[1]
	if (drive < 'A' || drive > 'Z') && (drive < 'a' || drive > 'z') {
		return path, false
	}
	return strings.ReplaceAll(path[1:], "/", `\`), true
}

func buildHTTPClient(template *http.Client, socket string, timeout time.Duration) *http.Client {
	var c http.Client
	if template != nil {
		c = *template
	}
	if socket != "" {
		c.Transport = &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		}}
	}
	c.Timeout = timeout
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &c
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// do performs one bounded request. Redirects are refused, non-2xx replies
// become APIError, and malformed success replies become ErrInvalidResponse.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, headers http.Header, reqBody, out any) error {
	var body io.Reader
	if reqBody != nil {
		data, err := json.Marshal(reqBody)
		if err != nil {
			return &opaqueError{message: "kata: encode request failed", causes: []error{err}}
		}
		body = bytes.NewReader(data)
	}
	target := c.baseURL + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return &opaqueError{message: "kata: invalid request", causes: []error{ErrUnavailable, err}}
	}
	req.Header.Set("Accept", "application/json")
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, values := range headers {
		for _, value := range values {
			req.Header.Add(k, value)
		}
	}
	token := c.token
	if c.tokenEnv != "" {
		token = os.Getenv(c.tokenEnv)
	}
	if token == "" && c.tokenRequired {
		return fmt.Errorf("%w: bearer token is unavailable", ErrNotReady)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return &opaqueError{message: "kata: request failed", causes: []error{ErrUnavailable, err}}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, c.maxBytes+1))
	if err != nil {
		return &opaqueError{message: "kata: read response failed", causes: []error{ErrUnavailable, err}}
	}
	if int64(len(data)) > c.maxBytes {
		return fmt.Errorf("%w (%d bytes)", ErrResponseTooLarge, c.maxBytes)
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return fmt.Errorf("kata: HTTP %d: %w", resp.StatusCode, ErrRedirect)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return decodeAPIError(resp.StatusCode, data)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return ErrInvalidResponse
	}
	return nil
}

func decodeAPIError(status int, data []byte) error {
	var env kg.ErrorEnvelope
	if err := json.Unmarshal(data, &env); err != nil || env.ErrorData.Code == "" {
		return &APIError{Status: status, Message: http.StatusText(status)}
	}
	apiErr := &APIError{Status: status, Code: env.ErrorData.Code, Message: env.ErrorData.Message}
	if env.ErrorData.Hint != nil {
		apiErr.Hint = *env.ErrorData.Hint
	}
	if len(env.ErrorData.Data) > 0 {
		if raw, err := json.Marshal(env.ErrorData.Data); err == nil {
			apiErr.Data = raw
		}
	}
	return apiErr
}
