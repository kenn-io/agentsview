package remotecred

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

func TestToken(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		mode       os.FileMode
		ok         bool
	}{
		{"valid", " token\n", 0o600, true}, {"empty", " \n", 0o600, false}, {"oversize", strings.Repeat("x", 65537), 0o600, false}, {"limit", strings.Repeat("x", 65536), 0o600, true}, {"public", "token", 0o644, false}, {"sticky", "token", 0o600 | os.ModeSticky, false}, {"executable", "token", 0o700, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "token")
			require.NoError(t, os.WriteFile(p, []byte(tc.body), tc.mode))
			got, err := ReadToken(p)
			if tc.ok || runtime.GOOS == "windows" && tc.name == "public" || runtime.GOOS == "windows" && (tc.name == "executable" || tc.name == "sticky") {
				require.NoError(t, err)
				require.Equal(t, strings.TrimSpace(tc.body), got)
			} else {
				require.Error(t, err)
			}
		})
	}
	p := filepath.Join(t.TempDir(), "missing")
	_, err := ReadToken(p)
	require.Error(t, err)
	require.NoError(t, os.WriteFile(p, []byte("secret"), 0o600))
	require.NoError(t, os.Symlink(p, p+"-link"))
	_, err = ReadToken(p + "-link")
	require.Error(t, err)
	_, err = ReadToken(t.TempDir())
	require.Error(t, err)
}

func TestURLPolicy(t *testing.T) {
	for _, tc := range []struct {
		url          string
		insecure, ok bool
	}{
		{"https://hub.example/path?secret=value", false, true}, {"http://hub.example", false, false}, {"http://hub.example", true, true}, {"http://127.0.0.1", false, true}, {"http://[::1]", false, true}, {"http://localhost", false, true}, {"https://user:password@hub.example", false, false}, {"ftp://hub.example", true, false}, {"https:///path", false, false},
	} {
		_, err := Client(tc.url, tc.insecure)
		require.Equal(t, tc.ok, err == nil, tc.url)
	}
	hits := 0
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer sink.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, sink.URL, http.StatusFound) }))
	defer redirect.Close()
	c, err := Client(redirect.URL, false)
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, redirect.URL, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := c.Do(req)
	if resp != nil {
		resp.Body.Close()
	}
	require.Error(t, err)
	require.Zero(t, hits)
	require.Equal(t, "https://hub.example:42", Redact("https://u:p@hub.example:42/path?token=secret"))
}

func TestTokenModeProperty(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not enforce Unix mode bits")
	}
	p := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(p, []byte("value"), 0o600))
	hegel.Test(t, func(ht *hegel.T) {
		mode := hegel.Draw(ht, hegel.Integers[uint32](0, 0o7777))
		permission := os.FileMode(mode & 0o777)
		if mode&0o4000 != 0 {
			permission |= os.ModeSetuid
		}
		if mode&0o2000 != 0 {
			permission |= os.ModeSetgid
		}
		if mode&0o1000 != 0 {
			permission |= os.ModeSticky
		}
		require.NoError(ht, os.Chmod(p, permission))
		got, err := ReadToken(p)
		info, statErr := os.Stat(p)
		require.NoError(ht, statErr)
		observed := info.Mode() & (os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky)
		safe := observed&^os.FileMode(0o600) == 0 && (observed&0o400 != 0 || os.Geteuid() == 0)
		if safe {
			require.NoError(ht, err)
			require.Equal(ht, "value", got)
		} else {
			require.Error(ht, err)
		}
	}, hegel.WithTestCases(1000))
}

func TestRedirectPolicyProperty(t *testing.T) {
	client, err := Client("https://HUB.example/private?secret=value", false)
	require.NoError(t, err)
	hegel.Test(t, func(ht *hegel.T) {
		same := hegel.Draw(ht, hegel.Booleans())
		secure := hegel.Draw(ht, hegel.Booleans())
		user := hegel.Draw(ht, hegel.Booleans())
		port := hegel.Draw(ht, hegel.SampledFrom([]string{"", ":443", ":444"}))
		scheme, host := "https", "hub.example"
		if !same {
			host = "other.example"
		}
		if !secure {
			scheme = "http"
		}
		raw := scheme + "://" + host + port + "/new?secret=value"
		req, e := http.NewRequestWithContext(t.Context(), http.MethodGet, raw, nil)
		require.NoError(ht, e)
		if user {
			req.URL.User = url.UserPassword("user", "secret")
		}
		e = client.CheckRedirect(req, []*http.Request{{}})
		require.Equal(ht, same && secure && !user && port != ":444", e == nil)
	}, hegel.WithTestCases(1000))
}

func TestTokenByteBoundProperty(t *testing.T) {
	p := filepath.Join(t.TempDir(), "token")
	hegel.Test(t, func(ht *hegel.T) {
		size := hegel.Draw(ht, hegel.Integers[uint64](0, ^uint64(0)))
		body := strings.Repeat("x", int(min(size, 65537)))
		if hegel.Draw(ht, hegel.Booleans()) {
			body = " " + body + "\n"
		}
		require.NoError(ht, os.WriteFile(p, []byte(body), 0o600))
		got, err := ReadToken(p)
		if len(body) <= 65536 && strings.TrimSpace(body) != "" {
			require.NoError(ht, err)
			require.Equal(ht, strings.TrimSpace(body), got)
		} else {
			require.Error(ht, err)
		}
	}, hegel.WithTestCases(1000))
}

func TestResponsePolicyPreservesTypedFailuresProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		status := hegel.Draw(ht, hegel.Integers[int](400, 599))
		code := "untrusted_code"
		if status == http.StatusConflict {
			code = "source_changed"
		}
		body := fmt.Sprintf(
			`{"status":%d,"code":%q,"detail":"private /path?token=secret"}`,
			status, code,
		)
		resp := &http.Response{
			StatusCode: status,
			Body:       io.NopCloser(strings.NewReader(body)),
		}
		got, err := ResponsePolicy("https://hub.example/private?token=secret")(resp, nil) //nolint:bodyclose // Every returned response is closed below; the policy closes discarded responses.
		if got != nil {
			defer got.Body.Close()
		}

		switch status {
		case http.StatusConflict, http.StatusServiceUnavailable:
			require.NoError(ht, err)
			require.NotNil(ht, got)
			require.Equal(ht, status, got.StatusCode)
			gotBody, readErr := io.ReadAll(got.Body)
			require.NoError(ht, readErr)
			require.NoError(ht, got.Body.Close())
			require.NotContains(ht, string(gotBody), "private")
			require.NotContains(ht, string(gotBody), "secret")
			if status == http.StatusConflict {
				require.JSONEq(ht, `{"code":"source_changed"}`, string(gotBody))
			} else {
				require.Equal(ht, "remote request failed", string(gotBody))
			}
		case http.StatusNotFound, http.StatusNotImplemented:
			require.NoError(ht, err)
			require.NotNil(ht, got)
			require.Equal(ht, status, got.StatusCode)
			gotBody, readErr := io.ReadAll(got.Body)
			require.NoError(ht, readErr)
			require.NoError(ht, got.Body.Close())
			require.NotContains(ht, string(gotBody), "private")
			require.NotContains(ht, string(gotBody), "secret")
		default:
			require.Nil(ht, got)
			require.Error(ht, err)
			require.ErrorContains(ht, err, fmt.Sprintf("HTTP %d", status))
			require.ErrorContains(ht, err, "--local")
			require.NotContains(ht, err.Error(), "private")
			require.NotContains(ht, err.Error(), "secret")
		}
	}, hegel.WithTestCases(1000))
}
