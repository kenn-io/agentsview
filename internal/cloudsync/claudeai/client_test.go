package claudeai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFirstConversationPage(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/api/organizations/org-123/chat_conversations_v2", r.URL.Path)
		assert.Equal(t, "1", r.URL.Query().Get("limit"))
		assert.Equal(t, "sessionKey=secret; lastActiveOrg=org-123", r.Header.Get("Cookie"))
		assert.Equal(t, "web_claude_ai", r.Header.Get("Anthropic-Client-Platform"))
		_, _ = w.Write([]byte(`{"conversations":[{"uuid":"conversation-1"}],"has_more":true}`))
	}))
	t.Cleanup(server.Close)

	client, err := NewClient(server.Client(), server.URL, Credentials{Cookie: "sessionKey=secret; lastActiveOrg=org-123"})
	require.NoError(t, err)

	page, err := client.FirstConversationPage(context.Background())
	require.NoError(t, err)
	assert.Len(t, page.Conversations, 1)
	require.NotNil(t, page.HasMore)
	assert.True(t, *page.HasMore)
}

func TestListConversationsUsesRequestedSmallPage(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "2", r.URL.Query().Get("limit"))
		_, _ = w.Write([]byte(`{"conversations":[{"uuid":"conversation-1"},{"uuid":"conversation-2"}],"has_more":true}`))
	}))
	t.Cleanup(server.Close)

	client, err := NewClient(server.Client(), server.URL, Credentials{Cookie: "sessionKey=secret; lastActiveOrg=org-123"})
	require.NoError(t, err)
	conversations, err := client.ListConversations(context.Background(), 2)
	require.NoError(t, err)
	assert.Len(t, conversations, 2)
}

func TestFirstConversationPageDoesNotExposeCookieOnHTTPError(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("not authenticated"))
	}))
	t.Cleanup(server.Close)

	cookie := "sessionKey=very-secret; lastActiveOrg=org-123"
	client, err := NewClient(server.Client(), server.URL, Credentials{Cookie: cookie})
	require.NoError(t, err)

	_, err = client.FirstConversationPage(context.Background())
	require.Error(t, err)
	assert.ErrorContains(t, err, "HTTP 401")
	assert.NotContains(t, err.Error(), cookie)
	assert.NotContains(t, err.Error(), "very-secret")
}

func TestNewClientRequiresSessionAndOrganization(t *testing.T) {
	t.Parallel()

	_, err := NewClient(nil, "", Credentials{})
	require.ErrorContains(t, err, "not connected")

	client, err := NewClient(nil, "", Credentials{Cookie: "sessionKey=secret"})
	require.NoError(t, err)
	_, err = client.FirstConversationPage(context.Background())
	require.ErrorContains(t, err, "lastActiveOrg")
}
