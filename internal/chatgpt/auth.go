// Package chatgpt provides opt-in text generation using the Codex device-login
// protocol. It owns its credentials and never reads another application's login.
package chatgpt

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	clientID          = "app_EMoamEEZ73f0CkXaXp7hrann"
	DeviceURL         = "https://auth.openai.com/codex/device"
	deviceRedirectURI = "https://auth.openai.com/deviceauth/callback"
	ResponsesURL      = "https://chatgpt.com/backend-api/codex/responses"
	userAgent         = "OpenAI File Downloader, XaiImageApiFetch/1.0"
)

// Client's endpoints are fixed in production; private overrides isolate tests.
type Client struct {
	dataDir      string
	authBase     string
	responsesURL string
	http         *http.Client
	wait         func(context.Context, time.Duration) error
}

// New uses only AgentsView's data directory, without accessing stored state.
func New(dataDir string) *Client {
	return &Client{
		dataDir: dataDir, authBase: "https://auth.openai.com", responsesURL: ResponsesURL,
		http: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		wait: waitContext,
	}
}

func waitContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// DeviceCode is shown only to the operator who explicitly starts login.
type DeviceCode struct {
	URL  string
	Code string
}

type authResponse struct {
	DeviceID          string         `json:"device_auth_id"`
	UserCode          string         `json:"user_code"`
	UserCodeAlias     string         `json:"usercode"`
	Interval          jsontext.Value `json:"interval"`
	AuthorizationCode string         `json:"authorization_code"`
	CodeVerifier      string         `json:"code_verifier"`
	AccessToken       string         `json:"access_token"`
	RefreshToken      string         `json:"refresh_token"`
	ExpiresIn         jsontext.Value `json:"expires_in"`
	Error             jsontext.Value `json:"error"`
}

func numericValue(raw []byte) (float64, error) {
	s := strings.TrimSpace(string(raw))
	if strings.HasPrefix(s, `"`) {
		if err := json.Unmarshal(raw, &s); err != nil {
			return 0, err
		}
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
		return 0, errors.New("invalid numeric value")
	}
	return n, nil
}

func pollInterval(raw []byte) (time.Duration, error) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" || s == `""` {
		return 5 * time.Second, nil
	}
	n, err := numericValue(raw)
	if err != nil || n < 0 {
		return 0, errors.New("invalid ChatGPT device polling interval")
	}
	return time.Duration(math.Round(math.Max(1, math.Min(60, n))*1000)) * time.Millisecond, nil
}

func authErrorCode(raw jsontext.Value) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var v struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(raw, &v) == nil {
		return v.Code
	}
	return ""
}

func (c *Client) authRequest(ctx context.Context, path, contentType string, body []byte) (authResponse, int, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.authBase+path, bytes.NewReader(body))
	if err != nil {
		return authResponse{}, 0, errors.New("cannot create ChatGPT authorization request")
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("originator", "agentsview")
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return authResponse{}, 0, ctx.Err()
		}
		return authResponse{}, 0, errors.New("ChatGPT authorization request failed")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, credentialLimit+1))
	if err != nil || len(data) > credentialLimit {
		return authResponse{}, resp.StatusCode, errors.New("invalid ChatGPT authorization response")
	}
	var decoded authResponse
	if json.Unmarshal(data, &decoded) != nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return authResponse{}, resp.StatusCode, errors.New("invalid ChatGPT authorization response")
	}
	return decoded, resp.StatusCode, nil
}

func credentialFromResponse(r authResponse, previousRefresh string) (credential, error) {
	fail := errors.New("invalid ChatGPT token identity or expiry")
	parts := strings.Split(r.AccessToken, ".")
	if len(parts) != 3 {
		return credential{}, fail
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return credential{}, fail
	}
	var claims struct {
		Exp     float64 `json:"exp"`
		Account string  `json:"chatgpt_account_id"`
		Auth    struct {
			Account string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return credential{}, fail
	}
	account := claims.Auth.Account
	if account == "" {
		account = claims.Account
	}
	if account == "" || strings.ContainsAny(account, "\r\n") {
		return credential{}, fail
	}
	now := time.Now()
	var expiry time.Time
	seconds, err := numericValue(r.ExpiresIn)
	if err == nil && seconds > 0 && seconds < float64(math.MaxInt64)/float64(time.Second) {
		expiry = now.Add(time.Duration(seconds * float64(time.Second)))
	} else if claims.Exp > 0 && claims.Exp < float64(math.MaxInt64)/1e9 {
		expiry = time.Unix(int64(claims.Exp), 0)
	}
	refresh := r.RefreshToken
	if refresh == "" {
		refresh = previousRefresh
	}
	if expiry.IsZero() || !expiry.After(now) || refresh == "" {
		return credential{}, fail
	}
	return credential{AccessToken: r.AccessToken, RefreshToken: refresh, AccountID: account, ExpiresAt: expiry}, nil
}

// Login starts a cancellable device attempt. A newer login or logout fences a
// late browser approval without blocking current inference during approval.
func (c *Client) Login(ctx context.Context, show func(DeviceCode) error) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	revision := rand.Text()
	if err := c.withLock(ctx, func() error { return c.writePrivate(ctx, "revision", []byte(revision)) }); err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]string{"client_id": clientID})
	r, status, err := c.authRequest(ctx, "/api/accounts/deviceauth/usercode", "application/json", body)
	if err != nil {
		return err
	}
	if status != 200 {
		return fmt.Errorf("ChatGPT device login returned HTTP %d; check that device-code login is enabled in your account or workspace", status)
	}
	interval, err := pollInterval(r.Interval)
	if err != nil {
		return err
	}
	if r.UserCode == "" {
		r.UserCode = r.UserCodeAlias
	}
	if r.DeviceID == "" || r.UserCode == "" || strings.ContainsAny(r.UserCode, "\x1b\r\n") {
		return errors.New("invalid ChatGPT device code response")
	}
	if err := show(DeviceCode{URL: DeviceURL, Code: r.UserCode}); err != nil {
		return err
	}
	pollBody, _ := json.Marshal(map[string]string{"device_auth_id": r.DeviceID, "user_code": r.UserCode})
	for {
		if err := c.wait(ctx, interval); err != nil {
			return err
		}
		if err := c.withLock(ctx, func() error {
			current, err := c.readPrivate(ctx, "revision")
			if err != nil {
				return err
			}
			if string(current) != revision {
				return errors.New("ChatGPT login cancelled by logout or a newer login")
			}
			return nil
		}); err != nil {
			return err
		}
		poll, code, err := c.authRequest(ctx, "/api/accounts/deviceauth/token", "application/json", pollBody)
		if err != nil {
			return err
		}
		switch authErrorCode(poll.Error) {
		case "access_denied", "expired_token", "authorization_declined", "deviceauth_expired":
			return errors.New("ChatGPT device authorization denied or expired")
		case "slow_down":
			interval = min(time.Minute, interval+5*time.Second)
			continue
		case "authorization_pending", "deviceauth_authorization_pending":
			continue
		}
		if code != 200 {
			if code == 403 || code == 404 {
				continue
			}
			return fmt.Errorf("ChatGPT device authorization returned HTTP %d", code)
		}
		if poll.AuthorizationCode == "" || poll.CodeVerifier == "" {
			return errors.New("invalid ChatGPT device authorization response")
		}
		values := url.Values{"grant_type": {"authorization_code"}, "client_id": {clientID}, "code": {poll.AuthorizationCode}, "code_verifier": {poll.CodeVerifier}, "redirect_uri": {deviceRedirectURI}}
		exchanged, code, err := c.authRequest(ctx, "/oauth/token", "application/x-www-form-urlencoded", []byte(values.Encode()))
		if err != nil {
			return err
		}
		if code != 200 {
			return fmt.Errorf("ChatGPT token exchange returned HTTP %d", code)
		}
		cr, err := credentialFromResponse(exchanged, "")
		if err != nil {
			return err
		}
		return c.withLock(ctx, func() error {
			current, err := c.readPrivate(ctx, "revision")
			if err != nil {
				return err
			}
			if string(current) != revision {
				return errors.New("ChatGPT login cancelled by logout or a newer login")
			}
			if err := c.writePrivate(ctx, "revision", []byte(rand.Text())); err != nil {
				return err
			}
			return c.saveCredential(ctx, cr)
		})
	}
}

func (c *Client) access(ctx context.Context, rejected string) (credential, error) {
	var result credential
	err := c.withLock(ctx, func() error {
		cr, err := c.loadCredential(ctx)
		if err != nil {
			return err
		}
		if cr.Reconnect {
			return errors.New("ChatGPT authorization expired; run agentsview insight chatgpt login")
		}
		if cr.AccessToken != rejected && cr.ExpiresAt.After(time.Now().Add(time.Minute)) {
			result = cr
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		// The server may rotate the refresh token before returning its response.
		// Finish this bounded exchange and persistence even if the caller cancels.
		refreshCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
		defer cancel()
		values := url.Values{"grant_type": {"refresh_token"}, "client_id": {clientID}, "refresh_token": {cr.RefreshToken}}
		response, status, err := c.authRequest(refreshCtx, "/oauth/token", "application/x-www-form-urlencoded", []byte(values.Encode()))
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		if status != 200 {
			switch authErrorCode(response.Error) {
			case "invalid_grant", "invalid_client", "unauthorized_client":
				if err := c.saveCredential(refreshCtx, credential{Reconnect: true}); err != nil {
					return err
				}
				return errors.New("ChatGPT authorization expired; run agentsview insight chatgpt login")
			}
			return fmt.Errorf("ChatGPT token refresh returned HTTP %d", status)
		}
		next, err := credentialFromResponse(response, cr.RefreshToken)
		if err != nil {
			return err
		}
		if err := c.saveCredential(refreshCtx, next); err != nil {
			return err
		}
		result = next
		return nil
	})
	return result, err
}
