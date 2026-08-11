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

const (
	defaultBaseURL = "https://claude.ai"

	// KeychainService and KeychainAccount are shared by the desktop login flow
	// and the Go daemon. The value stored under this item is secret.
	KeychainService = "io.agentsview.desktop"
	KeychainAccount = "claude-ai/default"
)

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
	Conversations []json.RawMessage
	HasMore       *bool
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
	return c.conversationPage(ctx, 1, 0)
}

// ListConversations returns every conversation summary, newest first. Claude's
// offset pagination can repeat a conversation if it changes during the walk,
// so duplicate UUIDs are suppressed locally.
func (c *Client) ListConversations(ctx context.Context, limit int) ([]json.RawMessage, error) {
	var (
		all  []json.RawMessage
		seen = make(map[string]struct{})
	)
	pageSize := 100
	if limit > 0 && limit < pageSize {
		pageSize = limit
	}
	for offset := 0; ; {
		page, err := c.conversationPage(ctx, pageSize, offset)
		if err != nil {
			return nil, err
		}
		for _, raw := range page.Conversations {
			var summary struct {
				UUID string `json:"uuid"`
			}
			if err := json.Unmarshal(raw, &summary); err != nil || summary.UUID == "" {
				continue
			}
			if _, exists := seen[summary.UUID]; exists {
				continue
			}
			seen[summary.UUID] = struct{}{}
			all = append(all, raw)
			if limit > 0 && len(all) >= limit {
				return all, nil
			}
		}
		if len(page.Conversations) == 0 ||
			(page.HasMore != nil && !*page.HasMore) ||
			(page.HasMore == nil && len(page.Conversations) < pageSize) {
			return all, nil
		}
		offset += len(page.Conversations)
	}
}

// Conversation fetches one complete conversation tree. It is intentionally
// read-only and returns the provider payload unchanged for the private cache.
func (c *Client) Conversation(ctx context.Context, conversationID string) (json.RawMessage, error) {
	orgID, err := c.organizationID()
	if err != nil {
		return nil, err
	}
	endpoint := c.baseURL.JoinPath("api", "organizations", orgID, "chat_conversations", conversationID)
	query := endpoint.Query()
	query.Set("tree", "True")
	query.Set("rendering_mode", "messages")
	query.Set("consistency", "strong")
	endpoint.RawQuery = query.Encode()
	return c.getJSON(ctx, endpoint, "conversation")
}

func (c *Client) conversationPage(ctx context.Context, limit, offset int) (ConversationPage, error) {
	orgID, err := c.organizationID()
	if err != nil {
		return ConversationPage{}, err
	}
	endpoint := c.baseURL.JoinPath("api", "organizations", orgID, "chat_conversations_v2")
	query := endpoint.Query()
	query.Set("limit", fmt.Sprint(limit))
	query.Set("offset", fmt.Sprint(offset))
	query.Set("starred", "false")
	query.Set("consistency", "eventual")
	endpoint.RawQuery = query.Encode()
	raw, err := c.getJSON(ctx, endpoint, "conversation list")
	if err != nil {
		return ConversationPage{}, err
	}
	page, err := decodeConversationPage(raw)
	if err != nil {
		return ConversationPage{}, fmt.Errorf("decode Claude conversation list: %w", err)
	}
	return page, nil
}

func decodeConversationPage(raw json.RawMessage) (ConversationPage, error) {
	var body any
	if err := json.Unmarshal(raw, &body); err != nil {
		return ConversationPage{}, err
	}
	page := ConversationPage{}
	switch value := body.(type) {
	case []any:
		for _, item := range value {
			encoded, err := json.Marshal(item)
			if err != nil {
				return ConversationPage{}, err
			}
			page.Conversations = append(page.Conversations, encoded)
		}
	case map[string]any:
		for _, key := range []string{"conversations", "items", "data", "results"} {
			items, ok := value[key].([]any)
			if !ok {
				continue
			}
			for _, item := range items {
				encoded, err := json.Marshal(item)
				if err != nil {
					return ConversationPage{}, err
				}
				page.Conversations = append(page.Conversations, encoded)
			}
			break
		}
		if hasMore, ok := value["has_more"].(bool); ok {
			page.HasMore = &hasMore
		}
	default:
		return ConversationPage{}, fmt.Errorf("expected an object or list")
	}
	return page, nil
}

func (c *Client) organizationID() (string, error) {
	return cookieValue(c.creds.Cookie, "lastActiveOrg")
}

func (c *Client) getJSON(ctx context.Context, endpoint *url.URL, operation string) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("create Claude request: %w", err)
	}
	c.applyHeaders(req)
	response, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request Claude %s: %w", operation, err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return nil, fmt.Errorf("Claude %s returned HTTP %d", operation, response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("read Claude %s: %w", operation, err)
	}
	if !json.Valid(body) {
		return nil, fmt.Errorf("decode Claude %s: invalid JSON", operation)
	}
	return body, nil
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
