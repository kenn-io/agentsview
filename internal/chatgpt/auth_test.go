package chatgpt

import (
	"context"
	"encoding/base64"
	"encoding/json/v2"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func token(t *testing.T, account string, expiry time.Time) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"exp": expiry.Unix(), "https://api.openai.com/auth": map[string]string{"chatgpt_account_id": account}})
	require.NoError(t, err)
	return "header." + base64.RawURLEncoding.EncodeToString(b) + ".signature"
}

func fixtureClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	s := httptest.NewServer(handler)
	t.Cleanup(s.Close)
	c := New(t.TempDir())
	c.authBase = s.URL
	c.responsesURL = s.URL + "/responses"
	c.wait = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	return c
}

func TestIntervalRepresentationsAgree(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		n := hegel.Draw(ht, hegel.Integers(int64(0), int64(math.MaxInt64)))
		number := []byte(strconv.FormatInt(n, 10))
		quoted := []byte(strconv.Quote(" \t" + string(number) + " \n"))
		a, err := pollInterval(number)
		require.NoError(ht, err)
		b, err := pollInterval(quoted)
		require.NoError(ht, err)
		assert.Equal(ht, a, b)
		assert.GreaterOrEqual(ht, a, time.Second)
		assert.LessOrEqual(ht, a, time.Minute)
	})
}

func TestIntervalRejectsInvalid(t *testing.T) {
	for _, raw := range []string{`-1`, `"NaN"`, `true`, `{}`, `"no"`} {
		_, err := pollInterval([]byte(raw))
		require.Error(t, err, raw)
	}
	for _, raw := range []string{"", `null`, `""`} {
		d, err := pollInterval([]byte(raw))
		require.NoError(t, err)
		assert.Equal(t, 5*time.Second, d)
	}
}

func TestDeviceLoginAndRefresh(t *testing.T) {
	var polls, refreshes atomic.Int32
	access := token(t, "account", time.Now().Add(time.Hour))
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "OpenAI File Downloader, XaiImageApiFetch/1.0", r.Header.Get("User-Agent"))
		switch r.URL.Path {
		case "/api/accounts/deviceauth/usercode":
			var body map[string]string
			assert.NoError(t, json.UnmarshalRead(r.Body, &body))
			assert.Equal(t, clientID, body["client_id"])
			fmt.Fprint(w, `{"device_auth_id":"device","usercode":"ABCD-EFGH","interval":"1"}`)
		case "/api/accounts/deviceauth/token":
			var body map[string]string
			assert.NoError(t, json.UnmarshalRead(r.Body, &body))
			assert.Equal(t, "device", body["device_auth_id"])
			assert.Equal(t, "ABCD-EFGH", body["user_code"])
			switch polls.Add(1) {
			case 1:
				w.WriteHeader(http.StatusForbidden)
				fmt.Fprint(w, `{"error":"slow_down"}`)
			case 2:
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, `{}`)
			default:
				fmt.Fprint(w, `{"authorization_code":"code","code_verifier":"verifier"}`)
			}
		case "/oauth/token":
			assert.NoError(t, r.ParseForm())
			assert.Equal(t, clientID, r.Form.Get("client_id"))
			if r.Form.Get("grant_type") == "authorization_code" {
				assert.Equal(t, "code", r.Form.Get("code"))
				assert.Equal(t, "verifier", r.Form.Get("code_verifier"))
				assert.Equal(t, deviceRedirectURI, r.Form.Get("redirect_uri"))
				assert.NoError(t, json.MarshalWrite(w, map[string]any{"access_token": access, "refresh_token": "refresh", "expires_in": "3600"}))
			} else {
				refreshes.Add(1)
				assert.Equal(t, "refresh", r.Form.Get("refresh_token"))
				assert.NoError(t, json.MarshalWrite(w, map[string]any{"access_token": access, "refresh_token": "rotated", "expires_in": 3600}))
			}
		default:
			assert.Fail(t, "unexpected path", r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	var delays []time.Duration
	c.wait = func(ctx context.Context, d time.Duration) error { delays = append(delays, d); return ctx.Err() }
	require.NoError(t, c.Login(t.Context(), func(code DeviceCode) error {
		assert.Equal(t, DeviceURL, code.URL)
		assert.Equal(t, "ABCD-EFGH", code.Code)
		return nil
	}))
	assert.Equal(t, []time.Duration{time.Second, 6 * time.Second, 6 * time.Second}, delays)
	st, err := c.Status(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "connected", st.State)
	cred, err := c.access(t.Context(), access)
	require.NoError(t, err)
	assert.Equal(t, "rotated", cred.RefreshToken)
	assert.EqualValues(t, 1, refreshes.Load())
	require.NoError(t, c.Logout(t.Context()))
	st, err = c.Status(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "disconnected", st.State)
}

func TestRefreshPersistsRotatedCredentialWhenRequestIsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	oldToken := token(t, "account", time.Now().Add(-time.Hour))
	newToken := token(t, "account", time.Now().Add(time.Hour))
	c := New(t.TempDir())
	c.authBase = "https://auth.invalid"
	started := make(chan struct{})
	resume := make(chan struct{})
	c.http.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		assert.Equal(t, "/oauth/token", req.URL.Path)
		close(started)
		<-resume
		if err := req.Context().Err(); err != nil {
			return nil, err
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(fmt.Sprintf(`{"access_token":%q,"refresh_token":"rotated","expires_in":3600}`, newToken))),
			Request:    req,
		}, nil
	})
	require.NoError(t, c.saveCredential(t.Context(), credential{
		AccessToken: oldToken, RefreshToken: "old-refresh", AccountID: "account", ExpiresAt: time.Now().Add(-time.Hour),
	}))

	type accessResult struct {
		credential credential
		err        error
	}
	done := make(chan accessResult, 1)
	go func() {
		refreshed, err := c.access(ctx, oldToken)
		done <- accessResult{credential: refreshed, err: err}
	}()
	<-started
	cancel()
	close(resume)
	result := <-done
	require.NoError(t, result.err)
	refreshed := result.credential
	assert.Equal(t, newToken, refreshed.AccessToken)
	assert.Equal(t, "rotated", refreshed.RefreshToken)

	saved, err := c.loadCredential(t.Context())
	require.NoError(t, err)
	assert.Equal(t, refreshed.AccessToken, saved.AccessToken)
	assert.Equal(t, refreshed.RefreshToken, saved.RefreshToken)
	assert.Equal(t, refreshed.AccountID, saved.AccountID)
	assert.WithinDuration(t, refreshed.ExpiresAt, saved.ExpiresAt, time.Second)
	usable, err := c.access(t.Context(), oldToken)
	require.NoError(t, err)
	assert.Equal(t, newToken, usable.AccessToken)
	assert.Equal(t, "rotated", usable.RefreshToken)
}

func TestLoginCannotUndoLogoutOrNewLogin(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		t.Run(strconv.FormatBool(replacement), func(t *testing.T) {
			jwt := token(t, "account", time.Now().Add(time.Hour))
			c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/accounts/deviceauth/usercode":
					fmt.Fprint(w, `{"device_auth_id":"device","user_code":"code"}`)
				case "/api/accounts/deviceauth/token":
					fmt.Fprint(w, `{"authorization_code":"code","code_verifier":"verifier"}`)
				case "/oauth/token":
					assert.NoError(t, json.MarshalWrite(w, map[string]any{"access_token": jwt, "refresh_token": "refresh"}))
				}
			})
			first := true
			err := c.Login(t.Context(), func(DeviceCode) error {
				if first {
					first = false
					if replacement {
						return c.Login(t.Context(), func(DeviceCode) error { return nil })
					}
					return c.Logout(t.Context())
				}
				return nil
			})
			require.ErrorContains(t, err, "cancelled")
			st, err := c.Status(t.Context())
			require.NoError(t, err)
			if replacement {
				assert.Equal(t, "connected", st.State)
			} else {
				assert.Equal(t, "disconnected", st.State)
			}
		})
	}
}

func TestDeviceDenialAndCancellation(t *testing.T) {
	for _, code := range []string{"access_denied", "expired_token", "authorization_declined"} {
		t.Run(code, func(t *testing.T) {
			c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "usercode") {
					fmt.Fprint(w, `{"device_auth_id":"device","user_code":"code"}`)
					return
				}
				w.WriteHeader(http.StatusForbidden)
				assert.NoError(t, json.MarshalWrite(w, map[string]string{"error": code}))
			})
			require.ErrorContains(t, c.Login(t.Context(), func(DeviceCode) error { return nil }), "denied or expired")
		})
	}
	c := fixtureClient(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"device_auth_id":"device","user_code":"code"}`)
	})
	ctx, cancel := context.WithCancel(t.Context())
	err := c.Login(ctx, func(DeviceCode) error { cancel(); return nil })
	require.ErrorIs(t, err, context.Canceled)
}

func TestRefreshFailuresPreserveOrClearCredential(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(strconv.FormatBool(terminal), func(t *testing.T) {
			c := fixtureClient(t, func(w http.ResponseWriter, _ *http.Request) {
				if terminal {
					w.WriteHeader(http.StatusBadRequest)
					fmt.Fprint(w, `{"error":"invalid_grant","message":"private-echo"}`)
				} else {
					w.WriteHeader(http.StatusServiceUnavailable)
					fmt.Fprint(w, `private-echo`)
				}
			})
			require.NoError(t, c.saveCredential(t.Context(), credential{AccessToken: token(t, "account", time.Now().Add(-time.Hour)), RefreshToken: "refresh", AccountID: "account", ExpiresAt: time.Now().Add(-time.Hour)}))
			_, err := c.access(t.Context(), "")
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "private-echo")
			st, err := c.Status(t.Context())
			require.NoError(t, err)
			if terminal {
				assert.Equal(t, "reconnect_required", st.State)
			} else {
				assert.Equal(t, "connected", st.State)
			}
		})
	}
}

// Numeric strings with whitespace are equivalent to numeric JSON values.
func TestIntervalWhitespaceZeroRegression(t *testing.T) {
	interval, err := pollInterval([]byte(`" \t0 \n"`))
	require.NoError(t, err)
	assert.Equal(t, time.Second, interval)
}

func TestTokenAccountAndExpiryProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		account := hegel.Draw(ht, hegel.Text())
		expiry := time.Now().Add(time.Hour)
		response := authResponse{AccessToken: token(t, account, expiry), RefreshToken: "refresh"}
		cr, err := credentialFromResponse(response, "")
		if account == "" || strings.ContainsAny(account, "\r\n") {
			require.Error(ht, err)
			return
		}
		require.NoError(ht, err)
		assert.Equal(ht, account, cr.AccountID)
		assert.Equal(ht, expiry.Unix(), cr.ExpiresAt.Unix())
	})
}

func TestDeviceStatesTakePrecedenceOverHTTPStatus(t *testing.T) {
	for _, status := range []int{200, 400, 403, 404} {
		for _, state := range []string{"authorization_pending", "deviceauth_authorization_pending", "slow_down", "access_denied", "expired_token", "authorization_declined", "deviceauth_expired"} {
			t.Run(fmt.Sprintf("%d/%s", status, state), func(t *testing.T) {
				var polls int
				jwt := token(t, "account", time.Now().Add(time.Hour))
				c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/api/accounts/deviceauth/usercode":
						fmt.Fprint(w, `{"device_auth_id":"device","user_code":"code","interval":1}`)
					case "/api/accounts/deviceauth/token":
						polls++
						if polls == 1 {
							w.WriteHeader(status)
							assert.NoError(t, json.MarshalWrite(w, map[string]any{"error": map[string]string{"code": state}}))
							return
						}
						fmt.Fprint(w, `{"authorization_code":"code","code_verifier":"verifier"}`)
					case "/oauth/token":
						assert.NoError(t, json.MarshalWrite(w, map[string]any{"access_token": jwt, "refresh_token": "refresh"}))
					}
				})
				var delays []time.Duration
				c.wait = func(ctx context.Context, d time.Duration) error { delays = append(delays, d); return ctx.Err() }
				err := c.Login(t.Context(), func(DeviceCode) error { return nil })
				switch state {
				case "authorization_pending", "deviceauth_authorization_pending", "slow_down":
					require.NoError(t, err)
					assert.Equal(t, 2, polls)
					expected := time.Second
					if state == "slow_down" {
						expected = 6 * time.Second
					}
					assert.Equal(t, []time.Duration{time.Second, expected}, delays)
				default:
					require.ErrorContains(t, err, "denied or expired")
					assert.Equal(t, 1, polls)
				}
			})
		}
	}
}

func TestLoginPollWaitsForStoreLockRegression(t *testing.T) {
	var polls atomic.Int32
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "usercode") {
			fmt.Fprint(w, `{"device_auth_id":"device","user_code":"code"}`)
			return
		}
		polls.Add(1)
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"error":"authorization_pending"}`)
	})
	locked, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	err := c.Login(ctx, func(DeviceCode) error {
		go func() { done <- c.withLock(t.Context(), func() error { close(locked); <-release; return nil }) }()
		<-locked
		return nil
	})
	close(release)
	require.NoError(t, <-done)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Zero(t, polls.Load())
}

func TestApprovedLoginCannotCommitAfterLogoutOrReplacement(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		t.Run(strconv.FormatBool(replacement), func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{})
			var exchanges atomic.Int32
			oldToken := token(t, "old-account", time.Now().Add(time.Hour))
			newToken := token(t, "new-account", time.Now().Add(time.Hour))
			c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/accounts/deviceauth/usercode":
					fmt.Fprint(w, `{"device_auth_id":"device","user_code":"code"}`)
				case "/api/accounts/deviceauth/token":
					fmt.Fprint(w, `{"authorization_code":"code","code_verifier":"verifier"}`)
				case "/oauth/token":
					access := newToken
					if exchanges.Add(1) == 1 {
						close(started)
						<-release
						access = oldToken
					}
					assert.NoError(t, json.MarshalWrite(w, map[string]any{"access_token": access, "refresh_token": "refresh"}))
				}
			})
			t.Cleanup(func() {
				select {
				case <-release:
				default:
					close(release)
				}
			})
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- c.Login(ctx, func(DeviceCode) error { return nil }) }()
			select {
			case <-started:
			case <-ctx.Done():
				require.FailNow(t, "token exchange did not start")
			}
			if replacement {
				require.NoError(t, c.Login(ctx, func(DeviceCode) error { return nil }))
			} else {
				require.NoError(t, c.Logout(ctx))
			}
			close(release)
			require.ErrorContains(t, <-done, "cancelled")
			st, err := c.Status(ctx)
			require.NoError(t, err)
			if replacement {
				assert.Equal(t, "connected", st.State)
				cr, err := c.loadCredential(ctx)
				require.NoError(t, err)
				assert.Equal(t, "new-account", cr.AccountID)
			} else {
				assert.Equal(t, "disconnected", st.State)
			}
		})
	}
}
