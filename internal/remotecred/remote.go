// Package remotecred enforces credentials and transport policy for configured remotes.
package remotecred

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ReadToken reads a bounded token after checking the opened file's safety.
func ReadToken(path string) (string, error) {
	f, err := openToken(path)
	if err != nil {
		return "", fmt.Errorf("opening remote token file: %w", err)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil {
		return "", fmt.Errorf("reading remote token file: %w", err)
	}
	token := strings.TrimSpace(string(b))
	if len(b) > 65536 || token == "" {
		return "", errors.New("remote token file must contain a non-empty token of at most 64 KiB")
	}
	return token, nil
}

// Redact retains only the scheme and authority for diagnostics.
func Redact(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<invalid remote URL>"
	}
	return (&url.URL{Scheme: u.Scheme, Host: u.Host}).String()
}

// Client permits only secure URLs and same-origin redirects.
func Client(raw string, insecure bool) (*http.Client, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, errors.New("remote URL requires http(s), a host, and no userinfo; try --local")
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme == "http" && !insecure && !strings.EqualFold(u.Hostname(), "localhost") && (ip == nil || !ip.IsLoopback()) {
		return nil, fmt.Errorf("remote %s requires HTTPS or allow_insecure_http; try --local", Redact(raw))
	}
	safe := Redact(raw)
	return &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 || req.URL.User != nil || origin(req.URL) != origin(u) {
			return fmt.Errorf("remote %s refused redirect; try --local", safe)
		}
		return nil
	}}, nil
}

func origin(u *url.URL) string {
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return strings.ToLower(u.Scheme + "://" + net.JoinHostPort(u.Hostname(), port))
}

// ResponsePolicy sanitizes failures after http.Client has wrapped URL errors.
// Missing and unsupported endpoints keep their API meaning without response text.
func ResponsePolicy(raw string) func(*http.Response, error) (*http.Response, error) {
	safe := Redact(raw)
	return func(resp *http.Response, err error) (*http.Response, error) {
		if err != nil {
			if resp != nil && resp.Body != nil {
				resp.Body.Close()
			}
			return nil, &remoteError{safe: safe, cause: err}
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			switch resp.StatusCode {
			case http.StatusNotFound:
				closeBody(resp.Body)
				resp.Body = io.NopCloser(strings.NewReader("remote " + safe + " endpoint unavailable; try --local"))
			case http.StatusNotImplemented, http.StatusConflict, http.StatusServiceUnavailable:
				resp.Body = io.NopCloser(strings.NewReader(sanitizedFailureBody(resp.StatusCode, resp.Body)))
			default:
				closeBody(resp.Body)
				return nil, fmt.Errorf("remote %s returned HTTP %d; try --local", safe, resp.StatusCode)
			}
		}
		return resp, nil
	}
}

const maxErrorResponseBytes = 64 << 10

func sanitizedFailureBody(status int, body io.ReadCloser) string {
	var payload struct {
		Code   string `json:"code"`
		Error  string `json:"error"`
		Detail string `json:"detail"`
	}
	if body != nil {
		data, err := io.ReadAll(io.LimitReader(body, maxErrorResponseBytes+1))
		closeBody(body)
		if err == nil && len(data) <= maxErrorResponseBytes {
			parsed := json.Unmarshal(data, &payload) == nil
			if status == http.StatusNotImplemented &&
				((parsed && payload.Error == "not available in remote mode") || strings.TrimSpace(string(data)) == "not available in remote mode") {
				return `{"error":"not available in remote mode"}`
			}
			if status == http.StatusConflict && parsed &&
				(payload.Code == "source_changed" || strings.HasPrefix(strings.TrimSpace(payload.Detail), "source_changed:")) {
				return `{"code":"source_changed"}`
			}
		}
	}
	return "remote request failed"
}

func closeBody(body io.ReadCloser) {
	if body != nil {
		body.Close()
	}
}

// WrapError adds safe remote diagnostics without exposing or losing the cause.
func WrapError(raw string, err error) error {
	if err == nil {
		return nil
	}
	return &remoteError{safe: Redact(raw), cause: err}
}

type remoteError struct {
	safe  string
	cause error
}

func (e *remoteError) Error() string { return "remote " + e.safe + " request failed; try --local" }
func (e *remoteError) Unwrap() error { return e.cause }
