package kata

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/kata/katatest"
)

func TestNewClientEndpoints(t *testing.T) {
	tests := []struct {
		name, endpoint, wantBase, wantErr string
		cfg                               Config
	}{
		{name: "https", endpoint: "https://kata.example.test/", wantBase: "https://kata.example.test"},
		{name: "loopback_http", endpoint: "http://127.0.0.1:7777", wantBase: "http://127.0.0.1:7777"},
		{name: "remote_http_refused", endpoint: "http://kata.example.test", wantErr: "plaintext http"},
		{name: "remote_http_allowed", cfg: Config{AllowInsecure: true}, endpoint: "http://kata.example.test", wantBase: "http://kata.example.test"},
		{name: "unix", endpoint: "unix:///tmp/kata.sock", wantBase: "http://kata.invalid"},
		{name: "unix_relative", endpoint: "unix://kata.sock", wantErr: "absolute"},
		{name: "userinfo", endpoint: "https://u:p@kata.example.test", wantErr: "credentials"},
		{name: "query", endpoint: "https://kata.example.test/?x=1", wantErr: "query"},
		{name: "fragment", endpoint: "https://kata.example.test/#secret", wantErr: "fragment"},
		{name: "scheme", endpoint: "ftp://kata.example.test", wantErr: "unix://, https:// or http://"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := newClient(tt.cfg, tt.endpoint)
			if tt.wantErr != "" {
				require.ErrorIs(t, err, ErrUnavailable)
				assert.Contains(t, err.Error(), tt.wantErr)
				assert.NotContains(t, err.Error(), "secret")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantBase, c.baseURL)
		})
	}
}

func TestDoRefusesRedirects(t *testing.T) {
	var followed bool
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { followed = true }))
	t.Cleanup(target.Close)
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/api/v1/health", http.StatusFound)
	}))
	t.Cleanup(src.Close)
	c, err := newClient(Config{}, src.URL)
	require.NoError(t, err)
	err = c.do(t.Context(), http.MethodGet, "/api/v1/health", nil, nil, nil, &struct{}{})
	require.ErrorIs(t, err, ErrRedirect)
	assert.False(t, followed)
}

func TestDoBoundsResponseBodies(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"pad":"` + strings.Repeat("x", 200) + `"}`))
	}))
	t.Cleanup(srv.Close)
	for _, tt := range []struct {
		name string
		max  int64
		want error
	}{{"over_limit", 64, ErrResponseTooLarge}, {"under_limit", 4096, nil}} {
		t.Run(tt.name, func(t *testing.T) {
			c, err := newClient(Config{MaxResponseBytes: tt.max}, srv.URL)
			require.NoError(t, err)
			var out map[string]any
			err = c.do(t.Context(), http.MethodGet, "/x", nil, nil, nil, &out)
			if tt.want != nil {
				require.ErrorIs(t, err, tt.want)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, true, out["ok"])
		})
	}
}

func TestDoDefaultResponseLimit(t *testing.T) {
	for _, tt := range []struct {
		name          string
		responseBytes int
		tooLarge      bool
	}{
		{name: "at_cap", responseBytes: 1_048_576},
		{name: "one_byte_over", responseBytes: 1_048_577, tooLarge: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			payload := strings.Repeat("x", tt.responseBytes-2)
			body := `"` + payload + `"`
			require.Len(t, body, tt.responseBytes)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(body))
			}))
			t.Cleanup(srv.Close)
			c, err := newClient(Config{}, srv.URL)
			require.NoError(t, err)
			var out string
			err = c.do(t.Context(), http.MethodGet, "/x", nil, nil, nil, &out)
			if tt.tooLarge {
				require.ErrorIs(t, err, ErrResponseTooLarge)
				assert.Empty(t, out)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, payload, out)
			assert.Len(t, out, 1_048_574)
		})
	}
}

func TestDoDecodesErrorEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"status":409,"error":{"code":"idempotency_mismatch","message":"key reused","hint":"h","data":{"uid":"01J0000000000000000000000A"}}}`))
	}))
	t.Cleanup(srv.Close)
	c, err := newClient(Config{}, srv.URL)
	require.NoError(t, err)
	err = c.do(t.Context(), http.MethodPost, "/x", nil, nil, map[string]string{"a": "b"}, &struct{}{})
	var apiErr *APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, 409, apiErr.Status)
	assert.Equal(t, "idempotency_mismatch", apiErr.Code)
	assert.Equal(t, "key reused", apiErr.Message)
	assert.Equal(t, "h", apiErr.Hint)
	assert.JSONEq(t, `{"uid":"01J0000000000000000000000A"}`, string(apiErr.Data))
	assert.True(t, IsCode(err, "idempotency_mismatch"))
	assert.Equal(t, 409, StatusOf(err))
}

func TestAPIErrorStringDoesNotExposeServerText(t *testing.T) {
	const secret = "synthetic-capability-secret"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"` + secret + `-code","message":"` + secret + `-message","hint":"` + secret + `-hint"}}`))
	}))
	t.Cleanup(srv.Close)
	c, err := newClient(Config{}, srv.URL)
	require.NoError(t, err)
	err = c.do(t.Context(), http.MethodGet, "/x", nil, nil, nil, &struct{}{})
	var apiErr *APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, secret+"-code", apiErr.Code)
	assert.Equal(t, secret+"-message", apiErr.Message)
	assert.Equal(t, secret+"-hint", apiErr.Hint)
	assert.True(t, IsCode(err, secret+"-code"))
	assert.Equal(t, "kata: HTTP 409", err.Error())
	assert.NotContains(t, err.Error(), secret)
}

func TestDoNonEnvelopeErrorAndMalformedSuccess(t *testing.T) {
	for _, tt := range []struct {
		name, body string
		status     int
		want       error
	}{{"html_502", "<html>bad gateway</html>", 502, nil}, {"malformed_200", "{not json", 200, ErrInvalidResponse}} {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			t.Cleanup(srv.Close)
			c, err := newClient(Config{}, srv.URL)
			require.NoError(t, err)
			err = c.do(t.Context(), http.MethodGet, "/x", nil, nil, nil, &map[string]any{})
			require.Error(t, err)
			if tt.want != nil {
				require.ErrorIs(t, err, tt.want)
				assert.Zero(t, StatusOf(err))
			} else {
				assert.Equal(t, tt.status, StatusOf(err))
				assert.NotContains(t, err.Error(), "<html>")
			}
		})
	}
}

func TestDoSendsBearerOnlyWhenTokenSet(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	for _, token := range []string{"", "tok-1"} {
		c, err := newClient(Config{Token: token}, srv.URL)
		require.NoError(t, err)
		require.NoError(t, c.do(t.Context(), http.MethodGet, "/x", nil, nil, nil, &map[string]any{}))
	}
	assert.Equal(t, []string{"", "Bearer tok-1"}, got)
}

func TestDoOverUnixSocket(t *testing.T) {
	//nolint:usetesting // t.TempDir can exceed the 107-byte Unix socket path limit.
	dir, err := os.MkdirTemp("", "kt")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", sock)
	require.NoError(t, err)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "kata.invalid", r.Host)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	srv.Listener = l
	srv.Start()
	t.Cleanup(srv.Close)
	c, err := newClient(Config{}, katatest.UnixEndpoint(sock))
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, c.do(t.Context(), http.MethodGet, "/api/v1/health", nil, nil, nil, &out))
	assert.Equal(t, true, out["ok"])
}

func TestWindowsUnixSocketPath(t *testing.T) {
	for _, tt := range []struct {
		name, path, want string
		wantWindowsPath  bool
	}{
		{name: "drive letter", path: "/C:/Program Files/Kata/daemon.sock", want: `C:\Program Files\Kata\daemon.sock`, wantWindowsPath: true},
		{name: "lowercase drive letter", path: "/d:/kata/daemon.sock", want: `d:\kata\daemon.sock`, wantWindowsPath: true},
		{name: "posix path", path: "/tmp/kata.sock", want: "/tmp/kata.sock"},
		{name: "not a drive path", path: "/1:/tmp/kata.sock", want: "/1:/tmp/kata.sock"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := windowsUnixSocketPath(tt.path)
			assert.Equal(t, tt.wantWindowsPath, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestStatusOfNonAPIError(t *testing.T) {
	assert.Zero(t, StatusOf(errors.New("other")))
	assert.False(t, IsCode(errors.New("other"), "any"))
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDoErrorsDoNotExposeURLSecrets(t *testing.T) {
	const baseSecret = "base-capability-secret"
	const pathSecret = "path-capability-secret"
	const querySecret = "query-capability-secret"
	const transportSecret = "transport-internal-secret"
	query := url.Values{"key": {querySecret}}
	assertRedacted := func(t *testing.T, err error) {
		t.Helper()
		for _, secret := range []string{baseSecret, pathSecret, querySecret, transportSecret} {
			assert.NotContains(t, err.Error(), secret)
		}
	}

	t.Run("transport", func(t *testing.T) {
		cause := errors.New(transportSecret)
		transport := roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, cause })
		c, err := newClient(Config{HTTPClient: &http.Client{Transport: transport}}, "https://kata.example.test/"+baseSecret)
		require.NoError(t, err)
		err = c.do(t.Context(), http.MethodGet, "/"+pathSecret, query, nil, nil, &struct{}{})
		require.ErrorIs(t, err, ErrUnavailable)
		require.ErrorIs(t, err, cause)
		assertRedacted(t, err)
	})

	t.Run("invalid_request_url", func(t *testing.T) {
		c, err := newClient(Config{}, "https://kata.example.test/"+baseSecret)
		require.NoError(t, err)
		err = c.do(t.Context(), http.MethodGet, "/"+pathSecret+"%ZZ", query, nil, nil, &struct{}{})
		require.ErrorIs(t, err, ErrUnavailable)
		assertRedacted(t, err)
	})

	t.Run("malformed_success", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("{not json"))
		}))
		t.Cleanup(srv.Close)
		c, err := newClient(Config{}, srv.URL+"/"+baseSecret)
		require.NoError(t, err)
		err = c.do(t.Context(), http.MethodGet, "/"+pathSecret, query, nil, nil, &map[string]any{})
		require.ErrorIs(t, err, ErrInvalidResponse)
		assertRedacted(t, err)
	})
}
