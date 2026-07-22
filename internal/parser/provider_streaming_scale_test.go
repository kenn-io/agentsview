package parser

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDatabaseAndContainerProvidersStreamLargeArchives(t *testing.T) {
	const sessions = 130

	tests := []struct {
		name  string
		setup func(*testing.T) Provider
	}{
		{name: "aider", setup: func(t *testing.T) Provider {
			root := t.TempDir()
			for i := range sessions {
				dir := filepath.Join(root, fmt.Sprintf("project-%03d", i))
				require.NoError(t, os.MkdirAll(dir, 0o755))
				require.NoError(t, os.WriteFile(
					filepath.Join(dir, aiderHistoryFile),
					[]byte("# aider chat started at 2026-07-14 12:00:00\n#### hello\nworld\n"),
					0o600,
				))
			}
			provider, ok := NewProvider(AgentAider, ProviderConfig{Roots: []string{root}})
			require.True(t, ok)
			return provider
		}},
		{name: "kiro", setup: func(t *testing.T) Provider {
			dbPath, db := newKiroSQLiteTestDB(t)
			for i := range sessions {
				seedKiroSQLiteSession(t, db, "/tmp/project", fmt.Sprintf("session-%03d", i),
					`{"conversation_id":"ignored"}`, 1, int64(i+1))
			}
			provider, ok := NewProvider(AgentKiro, ProviderConfig{Roots: []string{filepath.Dir(dbPath)}})
			require.True(t, ok)
			return provider
		}},
		{name: "hermes", setup: func(t *testing.T) Provider {
			root := t.TempDir()
			createHermesStateDB(t, root)
			db, err := sql.Open("sqlite3", filepath.Join(root, "state.db"))
			require.NoError(t, err)
			for i := 1; i < sessions; i++ {
				id := fmt.Sprintf("session-%03d", i)
				_, err = db.Exec(`INSERT INTO sessions
					(id, source, started_at, estimated_cost_usd, actual_cost_usd)
					VALUES (?, 'cli', ?, 0, 0)`, id, i)
				require.NoError(t, err)
				_, err = db.Exec(`INSERT INTO messages (session_id, role, content, timestamp)
					VALUES (?, 'user', 'hello', ?)`, id, i)
				require.NoError(t, err)
			}
			require.NoError(t, db.Close())
			provider, ok := NewProvider(AgentHermes, ProviderConfig{Roots: []string{root}})
			require.True(t, ok)
			return provider
		}},
		{name: "visualstudio-copilot", setup: func(t *testing.T) Provider {
			root := t.TempDir()
			path := filepath.Join(root, "20260714T120000_00000000_VSGitHubCopilot_traces.jsonl")
			var data strings.Builder
			for i := range sessions {
				id := fmt.Sprintf("00000000-0000-0000-0000-%012x", i)
				data.WriteString(vsCopilotTraceLineJSON(id, "chat", "1", "2", map[string]string{
					"gen_ai.operation.name": "chat",
				}))
				data.WriteByte('\n')
			}
			require.NoError(t, os.WriteFile(path, []byte(data.String()), 0o600))
			provider, ok := NewProvider(AgentVSCopilot, ProviderConfig{Roots: []string{root}})
			require.True(t, ok)
			return provider
		}},
		{name: "windsurf", setup: func(t *testing.T) Provider {
			root := filepath.Join(t.TempDir(), "Windsurf", "User")
			dbPath := filepath.Join(root, "workspaceStorage", "workspace", windsurfStateDBName)
			require.NoError(t, os.MkdirAll(filepath.Dir(dbPath), 0o755))
			chat := windsurfChatData{Tabs: make([]windsurfChatTab, 0, sessions)}
			for i := range sessions {
				chat.Tabs = append(chat.Tabs, windsurfChatTab{
					TabID:   fmt.Sprintf("session-%03d", i),
					Bubbles: []windsurfChatBubble{{Type: "user", Text: "hello"}},
				})
			}
			payload, err := json.Marshal(chat)
			require.NoError(t, err)
			writeWindsurfStateDB(t, dbPath, string(payload))
			provider, ok := NewProvider(AgentWindsurf, ProviderConfig{Roots: []string{root}})
			require.True(t, ok)
			return provider
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			provider := tc.setup(t)
			streaming, ok := provider.(StreamingDiscoverer)
			require.True(t, ok)
			maxBuffered := 0
			ctx := WithStreamingDiscoveryBufferObserver(t.Context(), func(buffered int) {
				maxBuffered = max(maxBuffered, buffered)
			})
			count := 0
			err := streaming.DiscoverEach(ctx, func(SourceRef) error {
				count++
				return nil
			})
			require.NoError(t, err)
			assert.Equal(t, sessions, count)
			assert.LessOrEqual(t, maxBuffered, streamingDirectoryBatchSize)

			stop := errors.New("stop after first source")
			err = streaming.DiscoverEach(context.Background(), func(SourceRef) error {
				return stop
			})
			require.ErrorIs(t, err, stop)
		})
	}
}

func TestSharedContainerReconciliationCacheAvoidsPerMemberRescans(t *testing.T) {
	const sessions = 300
	for _, tc := range []struct {
		name  string
		setup func(*testing.T) Provider
	}{
		{name: "visualstudio-copilot", setup: func(t *testing.T) Provider {
			root := t.TempDir()
			path := filepath.Join(root, "20260714T120000_00000000_VSGitHubCopilot_traces.jsonl")
			var data strings.Builder
			for i := range sessions {
				id := fmt.Sprintf("00000000-0000-0000-0000-%012x", i)
				data.WriteString(vsCopilotTraceLineJSON(id, "chat", "1", "2", map[string]string{
					"gen_ai.operation.name": "chat",
				}))
				data.WriteByte('\n')
			}
			require.NoError(t, os.WriteFile(path, []byte(data.String()), 0o600))
			provider, ok := NewProvider(AgentVSCopilot, ProviderConfig{Roots: []string{root}})
			require.True(t, ok)
			return provider
		}},
		{name: "windsurf", setup: func(t *testing.T) Provider {
			root := filepath.Join(t.TempDir(), "Windsurf", "User")
			dbPath := filepath.Join(root, "workspaceStorage", "workspace", windsurfStateDBName)
			require.NoError(t, os.MkdirAll(filepath.Dir(dbPath), 0o755))
			chat := windsurfChatData{Tabs: make([]windsurfChatTab, 0, sessions)}
			for i := range sessions {
				chat.Tabs = append(chat.Tabs, windsurfChatTab{
					TabID: fmt.Sprintf("session-%03d", i),
					Bubbles: []windsurfChatBubble{
						{Type: "user", Text: "hello"},
						{Type: "assistant", Text: "world"},
					},
				})
			}
			payload, err := json.Marshal(chat)
			require.NoError(t, err)
			writeWindsurfStateDB(t, dbPath, string(payload))
			provider, ok := NewProvider(AgentWindsurf, ProviderConfig{Roots: []string{root}})
			require.True(t, ok)
			return provider
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := tc.setup(t)
			scans := 0
			ctx := WithSharedContainerScanObserver(t.Context(), func() { scans++ })
			ctx, cleanup, err := WithReconciliationCache(ctx)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, cleanup()) })
			var sources []SourceRef
			err = provider.(StreamingDiscoverer).DiscoverEach(ctx, func(source SourceRef) error {
				sources = append(sources, source)
				return nil
			})
			require.NoError(t, err)
			require.Len(t, sources, sessions)
			for _, source := range sources {
				fingerprint, err := provider.Fingerprint(ctx, source)
				require.NoError(t, err)
				_, err = provider.Parse(ctx, ParseRequest{Source: source, Fingerprint: fingerprint})
				require.NoError(t, err)
			}
			assert.Equal(t, 1, scans)
		})
	}
}
