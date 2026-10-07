package importer

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

const syncSummary = `{"uuid":"one","name":"Chat","created_at":"2026-03-01T10:00:00Z","updated_at":"2026-03-01T10:05:00Z"}`
const syncDetail = `{"current_leaf_message_uuid":"active","chat_messages":[
    {"uuid":"root","parent_message_uuid":"","sender":"human","text":"Hello","created_at":"2026-03-01T10:00:00Z"},
    {"uuid":"other","parent_message_uuid":"root","sender":"assistant","text":"Wrong branch","created_at":"2026-03-01T10:01:00Z"},
    {"uuid":"active","parent_message_uuid":"root","sender":"assistant","text":"Chosen reply","created_at":"2026-03-01T10:02:00Z"}]}`

func TestSyncClaudeAI(t *testing.T) {
	t.Run("refusals keep checkpoints and missing sessions refetch", func(t *testing.T) {
		d := testDB(t)
		ctx := t.Context()
		require.NoError(t, d.UpsertSession(ctx, db.Session{ID: "claude-ai:one", Agent: "claude-ai", Project: "test", Machine: "test", MessageCount: 3}))
		require.NoError(t, d.ReplaceSessionMessages(ctx, "claude-ai:one", []db.Message{
			{SessionID: "claude-ai:one", Ordinal: 0, Role: "user", Content: "Hello"},
			{SessionID: "claude-ai:one", Ordinal: 1, Role: "assistant", Content: "Chosen reply"},
			{SessionID: "claude-ai:one", Ordinal: 2, Role: "user", Content: "Keep this turn"},
		}))
		require.NoError(t, d.SetSyncState(ctx, "claude_ai_sync:org:one", "previous"))
		require.NoError(t, d.SetSyncState(ctx, "claude_ai_sync:org:two", "2026-03-01T10:05:00Z"))
		details := 0
		stats, err := SyncClaudeAI(ctx, d, "org", func(ctx context.Context, path string) (int, []byte, error) {
			if path == "/api/organizations/org/chat_conversations_v2?limit=50&offset=0" {
				return 200, []byte(`{"conversations":[` + syncSummary + `,` + strings.ReplaceAll(syncSummary, "one", "two") + `],"has_more":false}`), nil
			}
			require.Contains(t, []string{"/api/organizations/org/chat_conversations/one?tree=True", "/api/organizations/org/chat_conversations/two?tree=True"}, path)
			details++
			return 200, []byte(syncDetail), nil
		}, nil)
		require.NoError(t, err)
		assert.Equal(t, 1, stats.Imported)
		assert.Equal(t, 2, details)
		assert.Equal(t, []ImportRefusal{{SessionID: "claude-ai:one", Reason: RefusalShorterExport}}, stats.Refusals)
		checkpoint, err := d.GetSyncState(ctx, "claude_ai_sync:org:one")
		require.NoError(t, err)
		assert.Equal(t, "previous", checkpoint)
		messages, err := d.GetAllMessages(ctx, "claude-ai:one")
		require.NoError(t, err)
		require.Len(t, messages, 3)
		assert.Equal(t, "Keep this turn", messages[2].Content)
	})
	t.Run("pages checkpoints changes and search", func(t *testing.T) {
		d := testDB(t)
		ctx := t.Context()
		changed := false
		details := []string{}
		pages := []string{}
		fetch := func(ctx context.Context, path string) (int, []byte, error) {
			if strings.Contains(path, "chat_conversations_v2") {
				pages = append(pages, path)
				if strings.HasSuffix(path, "offset=0") {
					summary := syncSummary
					if changed {
						summary = strings.ReplaceAll(summary, "10:05:00", "10:06:00")
					}
					return 200, []byte(`{"conversations":[` + summary + `],"has_more":true}`), nil
				}
				require.Equal(t, "/api/organizations/org/chat_conversations_v2?limit=50&offset=1", path)
				assert.True(t, d.HasFTS(ctx))
				hits, err := d.SearchSession(ctx, "claude-ai:one", "Chosen")
				require.NoError(t, err)
				assert.Equal(t, []int{1}, hits)
				return 200, []byte(`{"conversations":[` + strings.ReplaceAll(syncSummary, "one", "two") + `],"has_more":false}`), nil
			}
			details = append(details, path)
			require.Contains(t, []string{"/api/organizations/org/chat_conversations/one?tree=True", "/api/organizations/org/chat_conversations/two?tree=True"}, path)
			return 200, []byte(syncDetail), nil
		}
		pageWrites := 0
		stats, err := SyncClaudeAI(ctx, d, "org", fetch, &ImportCallbacks{OnPage: func() { pageWrites++ }})
		require.NoError(t, err)
		assert.Equal(t, 2, stats.Imported)
		assert.Equal(t, 2, pageWrites)
		assert.Len(t, pages, 2)
		assert.Len(t, details, 2)
		messages, err := d.GetAllMessages(ctx, "claude-ai:one")
		require.NoError(t, err)
		require.Len(t, messages, 2)
		assert.Equal(t, "Chosen reply", messages[1].Content)
		details = nil
		stats, err = SyncClaudeAI(ctx, d, "org", fetch, nil)
		require.NoError(t, err)
		assert.Equal(t, 2, stats.Skipped)
		assert.Empty(t, details)
		changed = true
		stats, err = SyncClaudeAI(ctx, d, "org", fetch, nil)
		require.NoError(t, err)
		assert.Equal(t, 1, stats.Updated)
		assert.Equal(t, []string{"/api/organizations/org/chat_conversations/one?tree=True"}, details)
	})

	t.Run("trash and exclusion allow later pages", func(t *testing.T) {
		for _, excluded := range []bool{false, true} {
			d := testDB(t)
			ctx := t.Context()
			require.NoError(t, d.UpsertSession(ctx, db.Session{ID: "claude-ai:one", Agent: "claude-ai", Project: "test", Machine: "test"}))
			require.NoError(t, d.SoftDeleteSession(ctx, "claude-ai:one"))
			if excluded {
				_, err := d.DeleteSessionIfTrashed(ctx, "claude-ai:one")
				require.NoError(t, err)
			}
			fetch := func(ctx context.Context, path string) (int, []byte, error) {
				switch path {
				case "/api/organizations/org/chat_conversations_v2?limit=50&offset=0":
					return 200, []byte(`{"conversations":[` + syncSummary + `],"has_more":true}`), nil
				case "/api/organizations/org/chat_conversations_v2?limit=50&offset=1":
					return 200, []byte(`{"conversations":[` + strings.ReplaceAll(syncSummary, "one", "two") + `],"has_more":false}`), nil
				case "/api/organizations/org/chat_conversations/two?tree=True":
					return 200, []byte(syncDetail), nil
				default:
					t.Fatalf("unexpected fetch %s", path)
					return 0, nil, nil
				}
			}
			stats, err := SyncClaudeAI(ctx, d, "org", fetch, nil)
			require.NoError(t, err)
			assert.Equal(t, 1, stats.Imported)
			if excluded {
				assert.Equal(t, 1, stats.Skipped)
			} else {
				assert.Equal(t, []ImportRefusal{{SessionID: "claude-ai:one", Reason: RefusalTrashed}}, stats.Refusals)
			}
		}
	})

	t.Run("cancelled write refetches", func(t *testing.T) {
		d := testDB(t)
		ctx, cancel := context.WithCancel(t.Context())
		store := &cancelSyncStore{DB: d, cancel: cancel}
		details := 0
		fetch := func(ctx context.Context, path string) (int, []byte, error) {
			if strings.Contains(path, "chat_conversations_v2") {
				return 200, []byte(`{"conversations":[` + syncSummary + `],"has_more":false}`), nil
			}
			details++
			return 200, []byte(syncDetail), nil
		}
		_, err := SyncClaudeAI(ctx, store, "org", fetch, nil)
		require.ErrorIs(t, err, context.Canceled)
		checkpoint, err := d.GetSyncState(t.Context(), "claude_ai_sync:org:one")
		require.NoError(t, err)
		assert.Empty(t, checkpoint)
		stats, err := SyncClaudeAI(t.Context(), d, "org", fetch, nil)
		require.NoError(t, err)
		assert.Equal(t, 1, stats.Updated)
		assert.Equal(t, 2, details)
		messages, err := d.GetAllMessages(t.Context(), "claude-ai:one")
		require.NoError(t, err)
		assert.Len(t, messages, 2)
	})

	t.Run("retry limit auth and empty page", func(t *testing.T) {
		for _, status := range []int{429, 503, 401, 403, 200} {
			t.Run(strconv.Itoa(status), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					d := testDB(t)
					calls := 0
					start := time.Now()
					_, err := SyncClaudeAI(t.Context(), d, "org", func(context.Context, string) (int, []byte, error) {
						calls++
						return status, []byte(`{"conversations":[],"has_more":true}`), ClaudeAIRetryAfter("120")
					}, nil)
					if status == 200 {
						require.NoError(t, err)
						assert.Equal(t, 1, calls)
					} else if status == 401 || status == 403 {
						require.ErrorContains(t, err, "Reconnect")
						assert.Equal(t, 1, calls)
					} else {
						require.Error(t, err)
						assert.Equal(t, 5, calls)
						assert.Equal(t, 4*time.Minute, time.Since(start))
					}
				})
			})
		}
	})
}

type cancelSyncStore struct {
	*db.DB
	cancel context.CancelFunc
}

func (s *cancelSyncStore) UpsertSession(ctx context.Context, session db.Session) error {
	err := s.DB.UpsertSession(ctx, session)
	if err == nil {
		s.cancel()
	}
	return err
}
