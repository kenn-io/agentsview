// Package claudeai fetches Claude.ai conversations using a session captured by
// the AgentsView desktop app. It intentionally contains no persistence or
// import behavior; those layers own scheduling and archive writes.
package claudeai

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const defaultBaseURL = "https://claude.ai"

// Credentials is the minimum browser-session material needed for a Claude.ai
// API request. Cookie is secret and must never be included in errors or logs.
type Credentials struct {
	Cookie string
}

// Client makes authenticated, read-only Claude.ai API requests.
type Client struct {
	baseURL           *url.URL
	httpClient        *http.Client
	creds             Credentials
	deviceID          string
	anonymousID       string
	activitySessionID string
}

// ConversationPage is a single conversation-list response.
type ConversationPage struct {
	Conversations []json.RawMessage `json:"conversations"`
	HasMore       bool              `json:"has_more"`
}

// NewClient returns a read-only Claude.ai client.
func NewClient(httpClient *http.Client, baseURL string, creds Credentials) (*Client, error) {
	if strings.TrimSpace(creds.Cookie) == "" {
		return nil, fmt.Errorf("Claude session is not connected")
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	parsedURL, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("parse Claude API base URL: %w", err)
	}
	return &Client{
		baseURL:           parsedURL,
		httpClient:        httpClient,
		creds:             creds,
		deviceID:          cookieValueOrNewID(creds.Cookie, "anthropic-device-id"),
		anonymousID:       "claudeai.v1." + newID(),
		activitySessionID: cookieValueOrNewID(creds.Cookie, "activitySessionId"),
	}, nil
}

// FirstConversationPage proves the browser session can access Claude.ai. It
// deliberately fetches only one small page and does not write any archive data.
func (c *Client) FirstConversationPage(ctx context.Context) (ConversationPage, error) {
	orgID, err := cookieValue(c.creds.Cookie, "lastActiveOrg")
	if err != nil {
		return ConversationPage{}, err
	}
	endpoint := c.baseURL.JoinPath("api", "organizations", orgID, "chat_conversations_v2")
	query := endpoint.Query()
	query.Set("limit", "1")
	query.Set("offset", "0")
	query.Set("starred", "false")
	query.Set("consistency", "eventual")
	endpoint.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return ConversationPage{}, fmt.Errorf("create Claude request: %w", err)
	}
	c.applyHeaders(req)
	response, err := c.httpClient.Do(req)
	if err != nil {
		return ConversationPage{}, fmt.Errorf("request Claude conversation list: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return ConversationPage{}, fmt.Errorf("Claude conversation list returned HTTP %d", response.StatusCode)
	}
	var page ConversationPage
	if err := json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(&page); err != nil {
		return ConversationPage{}, fmt.Errorf("decode Claude conversation list: %w", err)
	}
	return page, nil
}

func (c *Client) applyHeaders(req *http.Request) {
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Anthropic-Anonymous-Id", c.anonymousID)
	req.Header.Set("Anthropic-Client-Platform", "web_claude_ai")
	req.Header.Set("Anthropic-Client-Sha", "cbdcff92c28f90f26b8b9e9dfb4ae8e20b1eb957")
	req.Header.Set("Anthropic-Client-Version", "1.0.0")
	req.Header.Set("Anthropic-Device-Id", c.deviceID)
	req.Header.Set("X-Activity-Session-Id", c.activitySessionID)
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Origin", c.baseURL.String())
	req.Header.Set("Referer", c.baseURL.String()+"/")
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Cookie", c.creds.Cookie)
}

func cookieValueOrNewID(header, name string) string {
	value, err := cookieValue(header, name)
	if err == nil {
		return value
	}
	return newID()
}

func newID() string {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "agentsview"
	}
	return hex.EncodeToString(bytes)
}

func cookieValue(header, name string) (string, error) {
	for _, pair := range strings.Split(header, ";") {
		key, value, found := strings.Cut(strings.TrimSpace(pair), "=")
		if found && key == name && value != "" {
			return value, nil
		}
	}
	return "", fmt.Errorf("Claude session is missing %s", name)
}
