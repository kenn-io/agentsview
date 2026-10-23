// Package remotecred enforces credentials and transport policy for configured remotes.
package remotecred

import (
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
			resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusNotImplemented {
				return nil, fmt.Errorf("remote %s returned HTTP %d; try --local", safe, resp.StatusCode)
			}
			resp.Body = io.NopCloser(strings.NewReader("remote " + safe + " endpoint unavailable; try --local"))
		}
		return resp, nil
	}
}

type remoteError struct {
	safe  string
	cause error
}

func (e *remoteError) Error() string { return "remote " + e.safe + " request failed; try --local" }
func (e *remoteError) Unwrap() error { return e.cause }
