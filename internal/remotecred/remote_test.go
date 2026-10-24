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
)

func TestToken(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
		mode             os.FileMode
		ok               bool
	}{
		{"valid", " token\n", "token", 0o600, true},
		{"read only", "token", "token", 0o400, true},
		{"write only", "token", "token", 0o200, os.Geteuid() == 0},
		{"no permissions", "token", "token", 0, os.Geteuid() == 0},
		{"empty", "", "", 0o600, false},
		{"whitespace", " \n", "", 0o600, false},
		{"oversize", strings.Repeat("x", 65537), "", 0o600, false},
		{"limit", strings.Repeat("x", 65536), strings.Repeat("x", 65536), 0o600, true},
		{"whitespace at limit", " " + strings.Repeat("x", 65534) + "\n", strings.Repeat("x", 65534), 0o600, true},
		{"whitespace over limit", " " + strings.Repeat("x", 65535) + "\n", "", 0o600, false},
		{"multibyte at limit", strings.Repeat("é", 32768), strings.Repeat("é", 32768), 0o600, true},
		{"multibyte over limit", strings.Repeat("é", 32768) + "x", "", 0o600, false},
		{"public", "token", "token", 0o644, false},
		{"group read", "token", "token", 0o640, false},
		{"group write", "token", "token", 0o620, false},
		{"group execute", "token", "token", 0o610, false},
		{"other read", "token", "token", 0o604, false},
		{"other write", "token", "token", 0o602, false},
		{"other execute", "token", "token", 0o601, false},
		{"setuid", "token", "token", 0o600 | os.ModeSetuid, false},
		{"setgid", "token", "token", 0o600 | os.ModeSetgid, false},
		{"sticky", "token", "token", 0o600 | os.ModeSticky, false},
		{"executable", "token", "token", 0o700, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "token")
			require.NoError(t, os.WriteFile(p, []byte(tc.body), 0o600))
			require.NoError(t, os.Chmod(p, tc.mode))
			got, err := ReadToken(p)
			if tc.ok || runtime.GOOS == "windows" && tc.body == "token" {
				require.NoError(t, err)
				require.Equal(t, tc.want, got)
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

func TestRedirectPolicy(t *testing.T) {
	client, err := Client("https://HUB.example/private?secret=value", false)
	require.NoError(t, err)
	for _, tc := range []struct {
		name, url string
		user      bool
		hops      int
		ok        bool
	}{
		{"same origin", "https://hub.example/new", false, 1, true},
		{"default port", "https://hub.example:443/new", false, 1, true},
		{"different port", "https://hub.example:444/new", false, 1, false},
		{"different host", "https://other.example/new", false, 1, false},
		{"downgrade", "http://hub.example/new", false, 1, false},
		{"downgrade same port", "http://hub.example:443/new", false, 1, false},
		{"userinfo", "https://hub.example/new", true, 1, false},
		{"last allowed hop", "https://hub.example/new", false, 9, true},
		{"too many hops", "https://hub.example/new", false, 10, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, tc.url, nil)
			require.NoError(t, err)
			if tc.user {
				req.URL.User = url.UserPassword("user", "secret")
			}
			err = client.CheckRedirect(req, make([]*http.Request, tc.hops))
			require.Equal(t, tc.ok, err == nil)
		})
	}
}

func TestResponsePolicyPreservesTypedFailures(t *testing.T) {
	for _, tc := range []struct {
		status     int
		body, want string
		retained   bool
	}{
		{400, `{"error":"private secret"}`, "", false},
		{401, `{"error":"private secret"}`, "", false},
		{403, `{"error":"private secret"}`, "", false},
		{404, `{"error":"private secret"}`, "remote https://hub.example endpoint unavailable; try --local", true},
		{409, `{"code":"source_changed","detail":"private secret"}`, `{"code":"source_changed"}`, true},
		{409, `{"detail":"source_changed: private secret"}`, `{"code":"source_changed"}`, true},
		{409, `{"code":"untrusted","detail":"private secret"}`, "remote request failed", true},
		{500, `{"error":"private secret"}`, "", false},
		{501, `{"error":"private secret"}`, "remote request failed", true},
		{501, `{"error":"not available in remote mode","detail":"private secret"}`, `{"error":"not available in remote mode"}`, true},
		{503, `{"detail":"private secret"}`, "remote request failed", true},
		{599, `{"detail":"private secret"}`, "", false},
	} {
		t.Run(fmt.Sprintf("%d/%s", tc.status, tc.body), func(t *testing.T) {
			resp := &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body))}
			got, err := ResponsePolicy("https://hub.example/private?token=secret")(resp, nil) //nolint:bodyclose // Returned bodies are closed below; policy closes discarded responses.
			if tc.retained {
				require.NoError(t, err)
				require.NotNil(t, got)
				defer got.Body.Close()
				require.Equal(t, tc.status, got.StatusCode)
				body, err := io.ReadAll(got.Body)
				require.NoError(t, err)
				require.Equal(t, tc.want, string(body))
			} else {
				require.Nil(t, got)
				require.ErrorContains(t, err, fmt.Sprintf("HTTP %d", tc.status))
				require.ErrorContains(t, err, "--local")
				require.NotContains(t, err.Error(), "private")
				require.NotContains(t, err.Error(), "secret")
			}
		})
	}
}
