//go:build claude_live

package claudeai

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"
)

// TestLiveClaudeConversationList is an intentionally opt-in, read-only smoke
// test for this machine. It reads the user-approved macOS Keychain entry but
// never writes transcript data, cache files, or secrets to test output.
//
// Run it with:
// AGENTSVIEW_LIVE_CLAUDE=1 go test -tags claude_live ./internal/cloudsync/claudeai -run TestLiveClaudeConversationList
func TestLiveClaudeConversationList(t *testing.T) {
	if os.Getenv("AGENTSVIEW_LIVE_CLAUDE") != "1" {
		t.Skip("set AGENTSVIEW_LIVE_CLAUDE=1 to use the local Claude session")
	}
	cookie, err := keyring.Get(KeychainService, KeychainAccount)
	require.NoError(t, err, "connect Claude.ai in the desktop app first")

	client, err := NewClient(&http.Client{Timeout: 30 * time.Second}, "", Credentials{Cookie: cookie})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	conversations, err := client.ListConversations(ctx, 2)
	require.NoError(t, err)
	require.NotEmpty(t, conversations)
	t.Logf("Claude returned %d conversation summaries", len(conversations))
}

// TestLiveClaudeConversationDetail proves the detail request used by import.
// It is opt-in for the same reason as TestLiveClaudeConversationList.
func TestLiveClaudeConversationDetail(t *testing.T) {
	if os.Getenv("AGENTSVIEW_LIVE_CLAUDE") != "1" {
		t.Skip("set AGENTSVIEW_LIVE_CLAUDE=1 to use the local Claude session")
	}
	cookie, err := keyring.Get(KeychainService, KeychainAccount)
	require.NoError(t, err, "connect Claude.ai in the desktop app first")
	client, err := NewClient(&http.Client{Timeout: 30 * time.Second}, "", Credentials{Cookie: cookie})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	conversations, err := client.ListConversations(ctx, 1)
	require.NoError(t, err)
	require.NotEmpty(t, conversations)
	var summary struct {
		UUID string `json:"uuid"`
	}
	require.NoError(t, json.Unmarshal(conversations[0], &summary))
	require.NotEmpty(t, summary.UUID)
	detail, err := client.Conversation(ctx, summary.UUID)
	if err != nil {
		t.Fatalf("fetch Claude conversation detail failed: %#v", err)
	}
	require.True(t, json.Valid(detail))
}
