package importer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
)

const syncSummary = `{"uuid":"one","name":"Chat","is_starred":true,"current_leaf_message_uuid":"reply","created_at":"2026-03-01T10:00:00Z","updated_at":"2026-03-01T10:05:00Z"}`
const syncDetail = `{"uuid":"one","name":"Chat","current_leaf_message_uuid":"reply","created_at":"2026-03-01T10:00:00Z","updated_at":"2026-03-01T10:05:00Z","chat_messages":[
    {"uuid":"root","index":0,"parent_message_uuid":"00000000-0000-4000-8000-000000000000","sender":"human","text":"","content":[{"type":"text","text":"Hello"}],"created_at":"2026-03-01T10:00:00Z"},
    {"uuid":"reply","index":1,"parent_message_uuid":"root","sender":"assistant","text":"","content":[{"type":"text","text":"Chosen reply"}],"created_at":"2026-03-01T10:02:00Z"}]}`
const syncOrgs = `[{"uuid":"ignored","capabilities":["api"]},{"uuid":"org","capabilities":["chat"]}]`

func TestSyncClaudeAI(t *testing.T) {
	for _, tt := range []struct {
		name    string
		shorter bool
		want    []string
	}{
		{name: "shorter selected path", shorter: true, want: []string{"Edited question", "Chosen reply"}},
		{name: "rewritten selected path", want: []string{"Edited question", "Chosen reply", "Second question", "Second answer"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDB(t)
			detail := strings.TrimSuffix(strings.Replace(syncDetail, `"current_leaf_message_uuid":"reply"`, `"current_leaf_message_uuid":"a2"`, 1), "]}") + `,{"uuid":"q2","parent_message_uuid":"reply","sender":"human","text":"","content":[{"type":"text","text":"Second question"}]},{"uuid":"a2","parent_message_uuid":"q2","sender":"assistant","text":"","content":[{"type":"text","text":"Second answer"}]}]}`
			summary := strings.Replace(syncSummary, "reply", "a2", 1)
			details := 0
			fetch := func(ctx context.Context, path string) (ClaudeAIResponse, error) {
				switch path {
				case "/api/organizations/org/chat_conversations_v2?limit=50&offset=0&archived=true":
					return ClaudeAIResponse{Status: 200, Body: []byte(`{"data":[],"has_more":false}`)}, nil
				case "/api/organizations":
					return ClaudeAIResponse{Status: 200, Body: []byte(syncOrgs)}, nil
				case "/api/organizations/org/chat_conversations_v2?limit=50&offset=0&archived=false":
					return ClaudeAIResponse{Status: 200, Body: []byte(`{"data":[` + summary + `],"has_more":false}`)}, nil
				default:
					require.Equal(t, "/api/organizations/org/chat_conversations/one?tree=True&rendering_mode=messages&consistency=strong&render_all_tools=true&include_inline_comparison=true", path)
					details++
					return ClaudeAIResponse{Status: 200, Body: []byte(detail)}, nil
				}
			}
			stats, err := SyncClaudeAI(t.Context(), d, fetch, nil)
			require.NoError(t, err)
			require.Equal(t, 1, stats.Imported)
			messages, err := d.GetAllMessages(t.Context(), "claude-ai:one")
			require.NoError(t, err)
			require.Len(t, messages, 4)
			if tt.shorter {
				detail = syncDetail
				summary = strings.Replace(summary, "a2", "reply", 1)
			}
			detail = strings.Replace(strings.Replace(detail, "Hello", "Edited question", 1), "10:05:00Z", "10:06:00Z", 1)
			summary = strings.Replace(summary, "10:05:00Z", "10:06:00Z", 1)
			stats, err = SyncClaudeAI(t.Context(), d, fetch, nil)
			require.NoError(t, err)
			assert.Equal(t, 1, stats.Updated)
			assert.Empty(t, stats.Refusals)
			stats, err = SyncClaudeAI(t.Context(), d, fetch, nil)
			require.NoError(t, err)
			assert.Equal(t, 1, stats.Skipped)
			assert.Equal(t, 2, details)
			messages, err = d.GetAllMessages(t.Context(), "claude-ai:one")
			require.NoError(t, err)
			assert.Equal(t, tt.want, messageContents(messages))
			copies := replacedCopies(t, d, "claude-ai:one")
			require.Len(t, copies, 1)
			old, err := d.GetAllMessages(t.Context(), copies[0].ID)
			require.NoError(t, err)
			assert.Equal(t, []string{"Hello", "Chosen reply", "Second question", "Second answer"}, messageContents(old))
		})
	}
	t.Run("trashed during sync write is skipped", func(t *testing.T) {
		d := testDB(t)
		_, err := ImportClaudeAI(t.Context(), d, strings.NewReader("["+strings.Replace(syncDetail, "10:05:00Z", "10:04:00Z", 1)+"]"), nil)
		require.NoError(t, err)
		stats, err := SyncClaudeAI(t.Context(), trashDuringImportStore{d}, func(ctx context.Context, path string) (ClaudeAIResponse, error) {
			switch path {
			case "/api/organizations/org/chat_conversations_v2?limit=50&offset=0&archived=true":
				return ClaudeAIResponse{Status: 200, Body: []byte(`{"data":[],"has_more":false}`)}, nil
			case "/api/organizations":
				return ClaudeAIResponse{Status: 200, Body: []byte(syncOrgs)}, nil
			case "/api/organizations/org/chat_conversations_v2?limit=50&offset=0&archived=false":
				return ClaudeAIResponse{Status: 200, Body: []byte(`{"data":[` + syncSummary + `],"has_more":false}`)}, nil
			default:
				require.Equal(t, "/api/organizations/org/chat_conversations/one?tree=True&rendering_mode=messages&consistency=strong&render_all_tools=true&include_inline_comparison=true", path)
				return ClaudeAIResponse{Status: 200, Body: []byte(syncDetail)}, nil
			}
		}, nil)
		require.NoError(t, err)
		assert.Equal(t, 1, stats.Skipped)
		assert.Zero(t, stats.Errors)
		assert.Empty(t, stats.Refusals)
		assert.True(t, d.IsSessionTrashed(t.Context(), "claude-ai:one"))
	})
	t.Run("empty page with more fails", func(t *testing.T) {
		_, err := SyncClaudeAI(t.Context(), testDB(t), func(ctx context.Context, path string) (ClaudeAIResponse, error) {
			if path == "/api/organizations" {
				return ClaudeAIResponse{Status: 200, Body: []byte(syncOrgs)}, nil
			}
			require.Equal(t, "/api/organizations/org/chat_conversations_v2?limit=50&offset=0&archived=false", path)
			return ClaudeAIResponse{Status: 200, Body: []byte(`{"data":[],"has_more":true}`)}, nil
		}, nil)
		require.EqualError(t, err, "Claude list returned an empty page with has_more: true")
	})
	t.Run("restart and older zip freshness", func(t *testing.T) {
		d := testDB(t)
		details, notifications := 0, 0
		fetch := func(ctx context.Context, path string) (ClaudeAIResponse, error) {
			switch path {
			case "/api/organizations/org/chat_conversations_v2?limit=50&offset=0&archived=true":
				return ClaudeAIResponse{Status: 200, Body: []byte(`{"data":[],"has_more":false}`)}, nil
			case "/api/organizations":
				return ClaudeAIResponse{Status: 200, Body: []byte(syncOrgs)}, nil
			case "/api/organizations/org/chat_conversations_v2?limit=50&offset=0&archived=false":
				return ClaudeAIResponse{Status: 200, Body: []byte(`{"data":[` + syncSummary + `],"has_more":false}`)}, nil
			default:
				require.Equal(t, "/api/organizations/org/chat_conversations/one?tree=True&rendering_mode=messages&consistency=strong&render_all_tools=true&include_inline_comparison=true", path)
				details++
				return ClaudeAIResponse{Status: 200, Body: []byte(strings.Replace(syncDetail, `"chat_messages":[`, `"chat_messages":[
					{"uuid":"abandoned","parent_message_uuid":"root","sender":"assistant","text":"","content":[{"type":"text","text":"Abandoned reply"}]},`, 1))}, nil
			}
		}
		stats, err := SyncClaudeAI(t.Context(), d, fetch, &ImportCallbacks{OnPage: func() { notifications++ }})
		require.NoError(t, err)
		assert.Equal(t, 1, stats.Imported)
		assert.Equal(t, 1, notifications)
		messages, err := d.GetAllMessages(t.Context(), "claude-ai:one")
		require.NoError(t, err)
		require.Len(t, messages, 2)
		assert.Equal(t, "Chosen reply", messages[1].Content)
		hits, err := d.SearchSession(t.Context(), "claude-ai:one", "Chosen")
		require.NoError(t, err)
		assert.Equal(t, []int{1}, hits)
		stats, err = SyncClaudeAI(t.Context(), d, fetch, nil)
		require.NoError(t, err)
		assert.Equal(t, 1, stats.Skipped)
		assert.Equal(t, 1, details)
		zipPath := createTestZip(t, map[string]string{"conversations.json": "[" + strings.ReplaceAll(syncDetail, "10:05:00Z", "10:04:00Z") + "]"})
		dir, cleanup, err := ExtractZip(zipPath)
		require.NoError(t, err)
		defer cleanup()
		reader, err := os.Open(filepath.Join(dir, "conversations.json"))
		require.NoError(t, err)
		defer reader.Close()
		stats, err = ImportClaudeAI(t.Context(), d, reader, nil)
		require.NoError(t, err)
		assert.Equal(t, 1, stats.Updated)
		stats, err = SyncClaudeAI(t.Context(), d, fetch, nil)
		require.NoError(t, err)
		assert.Equal(t, 1, stats.Updated)
		assert.Equal(t, 2, details)
		session, err := d.GetSession(t.Context(), "claude-ai:one")
		require.NoError(t, err)
		assert.Equal(t, "2026-03-01T10:05:00Z", *session.EndedAt)
	})
	t.Run("malformed details allow later chats", func(t *testing.T) {
		for _, tt := range []struct {
			name   string
			detail string
			errors int
		}{
			{name: "null", detail: "null", errors: 1},
			{name: "bad timestamp", detail: strings.ReplaceAll(syncDetail, "10:05:00Z", "bad"), errors: 1},
		} {
			t.Run(tt.name, func(t *testing.T) {
				d := testDB(t)
				stats, err := SyncClaudeAI(t.Context(), d, func(ctx context.Context, path string) (ClaudeAIResponse, error) {
					switch path {
					case "/api/organizations/org/chat_conversations_v2?limit=50&offset=0&archived=true":
						return ClaudeAIResponse{Status: 200, Body: []byte(`{"data":[],"has_more":false}`)}, nil
					case "/api/organizations":
						return ClaudeAIResponse{Status: 200, Body: []byte(syncOrgs)}, nil
					case "/api/organizations/org/chat_conversations_v2?limit=50&offset=0&archived=false":
						return ClaudeAIResponse{Status: 200, Body: []byte(`{"data":[` + syncSummary + `,` + strings.ReplaceAll(syncSummary, "one", "two") + `],"has_more":false}`)}, nil
					case "/api/organizations/org/chat_conversations/one?tree=True&rendering_mode=messages&consistency=strong&render_all_tools=true&include_inline_comparison=true":
						return ClaudeAIResponse{Status: 200, Body: []byte(tt.detail)}, nil
					default:
						require.Equal(t, "/api/organizations/org/chat_conversations/two?tree=True&rendering_mode=messages&consistency=strong&render_all_tools=true&include_inline_comparison=true", path)
						return ClaudeAIResponse{Status: 200, Body: []byte(strings.ReplaceAll(syncDetail, "one", "two"))}, nil
					}
				}, nil)
				require.NoError(t, err)
				assert.Equal(t, 1, stats.Imported)
				assert.Zero(t, stats.Skipped)
				assert.Equal(t, tt.errors, stats.Errors)
				session, err := d.GetSession(t.Context(), "claude-ai:one")
				require.NoError(t, err)
				assert.Nil(t, session)
				messages, err := d.GetAllMessages(t.Context(), "claude-ai:two")
				require.NoError(t, err)
				require.Len(t, messages, 2)
				assert.Equal(t, "Chosen reply", messages[1].Content)
			})
		}
	})
	t.Run("trash exclusion and missing detail are skipped", func(t *testing.T) {
		for _, excluded := range []bool{false, true} {
			d := testDB(t)
			require.NoError(t, d.UpsertSession(t.Context(), db.Session{ID: "claude-ai:one", Agent: "claude-ai", Project: "test", Machine: "test"}))
			require.NoError(t, d.SoftDeleteSession(t.Context(), "claude-ai:one"))
			if excluded {
				_, err := d.DeleteSessionIfTrashed(t.Context(), "claude-ai:one")
				require.NoError(t, err)
			}
			stats, err := SyncClaudeAI(t.Context(), d, func(ctx context.Context, path string) (ClaudeAIResponse, error) {
				switch path {
				case "/api/organizations/org/chat_conversations_v2?limit=50&offset=0&archived=true":
					return ClaudeAIResponse{Status: 200, Body: []byte(`{"data":[],"has_more":false}`)}, nil
				case "/api/organizations":
					return ClaudeAIResponse{Status: 200, Body: []byte(syncOrgs)}, nil
				case "/api/organizations/org/chat_conversations_v2?limit=50&offset=0&archived=false":
					return ClaudeAIResponse{Status: 200, Body: []byte(`{"data":[` + syncSummary + `],"has_more":true}`)}, nil
				case "/api/organizations/org/chat_conversations_v2?limit=50&offset=1&archived=false":
					return ClaudeAIResponse{Status: 200, Body: []byte(`{"data":[` + strings.ReplaceAll(syncSummary, "one", "two") + `],"has_more":false}`)}, nil
				default:
					require.Equal(t, "/api/organizations/org/chat_conversations/two?tree=True&rendering_mode=messages&consistency=strong&render_all_tools=true&include_inline_comparison=true", path)
					return ClaudeAIResponse{Status: 404, Body: nil}, nil
				}
			}, nil)
			require.NoError(t, err)
			assert.Equal(t, 2, stats.Skipped)
			assert.Zero(t, stats.Errors)
			assert.Empty(t, stats.Refusals)
		}
	})
	t.Run("cancelled write preserves freshness and refetches", func(t *testing.T) {
		d := testDB(t)
		stats, err := ImportClaudeAI(t.Context(), d, strings.NewReader("["+strings.ReplaceAll(syncDetail, "10:05:00Z", "10:04:00Z")+"]"), nil)
		require.NoError(t, err)
		require.Equal(t, 1, stats.Imported)
		ctx, cancel := context.WithCancel(t.Context())
		store := &cancelSyncStore{DB: d, cancel: cancel}
		details := 0
		fetch := func(ctx context.Context, path string) (ClaudeAIResponse, error) {
			switch path {
			case "/api/organizations/org/chat_conversations_v2?limit=50&offset=0&archived=true":
				return ClaudeAIResponse{Status: 200, Body: []byte(`{"data":[],"has_more":false}`)}, nil
			case "/api/organizations":
				return ClaudeAIResponse{Status: 200, Body: []byte(syncOrgs)}, nil
			case "/api/organizations/org/chat_conversations_v2?limit=50&offset=0&archived=false":
				return ClaudeAIResponse{Status: 200, Body: []byte(`{"data":[` + syncSummary + `],"has_more":false}`)}, nil
			default:
				require.Equal(t, "/api/organizations/org/chat_conversations/one?tree=True&rendering_mode=messages&consistency=strong&render_all_tools=true&include_inline_comparison=true", path)
				details++
				return ClaudeAIResponse{Status: 200, Body: []byte(syncDetail)}, nil
			}
		}
		_, err = SyncClaudeAI(ctx, store, fetch, nil)
		require.ErrorIs(t, err, context.Canceled)
		session, err := d.GetSession(t.Context(), "claude-ai:one")
		require.NoError(t, err)
		assert.Equal(t, "2026-03-01T10:04:00Z", *session.EndedAt)
		stats, err = SyncClaudeAI(t.Context(), d, fetch, nil)
		require.NoError(t, err)
		assert.Equal(t, 1, stats.Updated)
		assert.Equal(t, 2, details)
	})
	t.Run("overlapping pages import later chats", func(t *testing.T) {
		d := testDB(t)
		details := 0
		stats, err := SyncClaudeAI(t.Context(), d, func(ctx context.Context, path string) (ClaudeAIResponse, error) {
			switch path {
			case "/api/organizations/org/chat_conversations_v2?limit=50&offset=0&archived=true":
				return ClaudeAIResponse{Status: 200, Body: []byte(`{"data":[],"has_more":false}`)}, nil
			case "/api/organizations":
				return ClaudeAIResponse{Status: 200, Body: []byte(syncOrgs)}, nil
			case "/api/organizations/org/chat_conversations_v2?limit=50&offset=0&archived=false":
				return ClaudeAIResponse{Status: 200, Body: []byte(`{"data":[` + syncSummary + `],"has_more":true}`)}, nil
			case "/api/organizations/org/chat_conversations_v2?limit=50&offset=1&archived=false":
				return ClaudeAIResponse{Status: 200, Body: []byte(`{"data":[` + syncSummary + `,` + strings.ReplaceAll(syncSummary, "one", "two") + `],"has_more":true}`)}, nil
			case "/api/organizations/org/chat_conversations_v2?limit=50&offset=3&archived=false":
				return ClaudeAIResponse{Status: 200, Body: []byte(`{"data":[],"has_more":false}`)}, nil
			case "/api/organizations/org/chat_conversations/one?tree=True&rendering_mode=messages&consistency=strong&render_all_tools=true&include_inline_comparison=true":
				details++
				return ClaudeAIResponse{Status: 200, Body: []byte(syncDetail)}, nil
			default:
				require.Equal(t, "/api/organizations/org/chat_conversations/two?tree=True&rendering_mode=messages&consistency=strong&render_all_tools=true&include_inline_comparison=true", path)
				details++
				return ClaudeAIResponse{Status: 200, Body: []byte(strings.ReplaceAll(syncDetail, "one", "two"))}, nil
			}
		}, nil)
		require.NoError(t, err)
		assert.Equal(t, 2, stats.Imported)
		assert.Equal(t, 1, stats.Skipped)
		assert.Equal(t, 2, details)
		messages, err := d.GetAllMessages(t.Context(), "claude-ai:two")
		require.NoError(t, err)
		require.Len(t, messages, 2)
		assert.Equal(t, "Chosen reply", messages[1].Content)
	})
	t.Run("missing data fails", func(t *testing.T) {
		for _, page := range []string{`{}`, `{"items":[]}`, `{"conversations":[]}`, `{"results":[]}`} {
			d := testDB(t)
			_, err := SyncClaudeAI(t.Context(), d, func(ctx context.Context, path string) (ClaudeAIResponse, error) {
				if path == "/api/organizations" {
					return ClaudeAIResponse{Status: 200, Body: []byte(syncOrgs)}, nil
				}
				require.Equal(t, "/api/organizations/org/chat_conversations_v2?limit=50&offset=0&archived=false", path)
				return ClaudeAIResponse{Status: 200, Body: []byte(page)}, nil
			}, nil)
			require.EqualError(t, err, "Claude list had no data")
		}
	})
	t.Run("retry limit and sign in", func(t *testing.T) {
		for _, status := range []int{429, 503, 401, 403} {
			t.Run(strconv.Itoa(status), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					d := testDB(t)
					calls := 0
					start := time.Now()
					_, err := SyncClaudeAI(t.Context(), d, func(context.Context, string) (ClaudeAIResponse, error) {
						calls++
						return ClaudeAIResponse{Status: status, RetryAfter: "120"}, nil
					}, nil)
					require.Error(t, err)
					if status == 401 || status == 403 {
						assert.Equal(t, "Sign in to Claude.ai, then Sync again", err.Error())
						assert.Equal(t, 1, calls)
					} else {
						assert.Equal(t, 5, calls)
						assert.Equal(t, 4*time.Minute, time.Since(start))
					}
				})
			})
		}
	})
}

type trashDuringImportStore struct{ *db.DB }

func (s trashDuringImportStore) WriteSessionBatchAtomic(ctx context.Context, writes []db.SessionBatchWrite, beforeCommit ...func() error) (db.SessionBatchResult, error) {
	if err := s.DB.SoftDeleteSession(ctx, writes[0].Session.ID); err != nil {
		return db.SessionBatchResult{}, err
	}
	return s.DB.WriteSessionBatchAtomic(ctx, writes, beforeCommit...)
}

type cancelSyncStore struct {
	*db.DB
	cancel context.CancelFunc
}

func (s *cancelSyncStore) WriteSessionBatchAtomic(ctx context.Context, writes []db.SessionBatchWrite, beforeCommit ...func() error) (db.SessionBatchResult, error) {
	return s.DB.WriteSessionBatchAtomic(ctx, writes, func() error { s.cancel(); return ctx.Err() })
}

func TestSyncClaudeAIHistory(t *testing.T) {
	for _, tt := range []struct {
		name, detail string
		failed       bool
		want         []string
	}{
		{name: "missing parent preserves archive", detail: strings.Replace(syncDetail, `"parent_message_uuid":"00000000-0000-4000-8000-000000000000"`, `"parent_message_uuid":"missing"`, 1), failed: true},
		{name: "requested uuid mismatch", detail: strings.Replace(syncDetail, `"uuid":"one"`, `"uuid":"other"`, 1), failed: true},
		{name: "duplicate message uuid", detail: strings.Replace(syncDetail, `"uuid":"reply"`, `"uuid":"root"`, 1), failed: true},
		{name: "grown history preserves ids", detail: strings.TrimSuffix(strings.Replace(syncDetail, `"current_leaf_message_uuid":"reply"`, `"current_leaf_message_uuid":"a2"`, 1), "]}") + `,{"uuid":"q2","parent_message_uuid":"reply","sender":"human","text":"","content":[{"type":"text","text":"More"}]},{"uuid":"a2","parent_message_uuid":"q2","sender":"assistant","text":"","content":[{"type":"text","text":"Answer"}]}]}`, want: []string{"Hello", "Chosen reply", "More", "Answer"}},
		{name: "rename refreshes ended at", detail: strings.Replace(syncDetail, `"name":"Chat"`, `"name":"Renamed"`, 1), want: []string{"Hello", "Chosen reply"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDB(t)
			stats, err := SyncClaudeAI(t.Context(), d, syncOneFetch(t, syncSummary, func() (ClaudeAIResponse, error) { return ClaudeAIResponse{Status: 200, Body: []byte(syncDetail)}, nil }), nil)
			require.NoError(t, err)
			require.Equal(t, 1, stats.Imported)
			before, err := d.GetAllMessages(t.Context(), "claude-ai:one")
			require.NoError(t, err)
			details := 0
			summary := strings.ReplaceAll(syncSummary, "10:05:00Z", "10:06:00Z")
			if tt.name == "grown history preserves ids" {
				summary = strings.Replace(summary, "reply", "a2", 1)
			}
			fetch := func(ctx context.Context, path string) (ClaudeAIResponse, error) {
				switch path {
				case "/api/organizations":
					return ClaudeAIResponse{Status: 200, Body: []byte(syncOrgs)}, nil
				case "/api/organizations/org/chat_conversations_v2?limit=50&offset=0&archived=false":
					return ClaudeAIResponse{Status: 200, Body: []byte(`{"data":[` + summary + `],"has_more":false}`)}, nil
				case "/api/organizations/org/chat_conversations_v2?limit=50&offset=0&archived=true":
					return ClaudeAIResponse{Status: 200, Body: []byte(`{"data":[],"has_more":false}`)}, nil
				default:
					require.Equal(t, "/api/organizations/org/chat_conversations/one?tree=True&rendering_mode=messages&consistency=strong&render_all_tools=true&include_inline_comparison=true", path)
					details++
					return ClaudeAIResponse{Status: 200, Body: []byte(strings.ReplaceAll(tt.detail, "10:05:00Z", "10:06:00Z"))}, nil
				}
			}
			stats, err = SyncClaudeAI(t.Context(), d, fetch, nil)
			require.NoError(t, err)
			after, err := d.GetAllMessages(t.Context(), "claude-ai:one")
			require.NoError(t, err)
			assert.Empty(t, replacedCopies(t, d, "claude-ai:one"))
			if tt.failed {
				assert.Equal(t, 1, stats.Errors)
				assert.Equal(t, before, after)
				session, err := d.GetSession(t.Context(), "claude-ai:one")
				require.NoError(t, err)
				require.NotNil(t, session)
				require.NotNil(t, session.EndedAt)
				assert.Equal(t, "2026-03-01T10:05:00Z", *session.EndedAt)
				return
			}
			assert.Equal(t, 1, stats.Updated)
			assert.Zero(t, stats.Errors)
			assert.Equal(t, tt.want, messageContents(after))
			for i, m := range before {
				assert.Equal(t, m.ID, after[i].ID)
			}
			session, err := d.GetSession(t.Context(), "claude-ai:one")
			require.NoError(t, err)
			require.NotNil(t, session)
			require.NotNil(t, session.EndedAt)
			assert.Equal(t, "2026-03-01T10:06:00Z", *session.EndedAt)
			if tt.name == "rename refreshes ended at" {
				require.NotNil(t, session.DisplayName)
				assert.Equal(t, "Renamed", *session.DisplayName)
			}
			stats, err = SyncClaudeAI(t.Context(), d, fetch, nil)
			require.NoError(t, err)
			assert.Equal(t, 1, stats.Skipped)
			assert.Equal(t, 1, details)
		})
	}
}

func TestSyncClaudeAIArchivedList(t *testing.T) {
	for _, mode := range []string{"archived only", "flag ignored", "flag rejected"} {
		t.Run(mode, func(t *testing.T) {
			d := testDB(t)
			details := 0
			stats, err := SyncClaudeAI(t.Context(), d, func(ctx context.Context, path string) (ClaudeAIResponse, error) {
				switch path {
				case "/api/organizations":
					return ClaudeAIResponse{Status: 200, Body: []byte(syncOrgs)}, nil
				case "/api/organizations/org/chat_conversations_v2?limit=50&offset=0&archived=false":
					if mode != "flag ignored" {
						return ClaudeAIResponse{Status: 200, Body: []byte(`{"data":[],"has_more":false}`)}, nil
					}
					return ClaudeAIResponse{Status: 200, Body: []byte(`{"data":[` + syncSummary + `],"has_more":false}`)}, nil
				case "/api/organizations/org/chat_conversations_v2?limit=50&offset=0&archived=true":
					if mode == "flag rejected" {
						return ClaudeAIResponse{Status: 400}, nil
					}
					return ClaudeAIResponse{Status: 200, Body: []byte(`{"data":[` + syncSummary + `],"has_more":false}`)}, nil
				default:
					require.Equal(t, "/api/organizations/org/chat_conversations/one?tree=True&rendering_mode=messages&consistency=strong&render_all_tools=true&include_inline_comparison=true", path)
					details++
					return ClaudeAIResponse{Status: 200, Body: []byte(syncDetail)}, nil
				}
			}, nil)
			if mode == "flag rejected" {
				require.EqualError(t, err, "Claude returned HTTP 400")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, 1, stats.Imported)
			assert.Equal(t, 1, details)
			messages, err := d.GetAllMessages(t.Context(), "claude-ai:one")
			require.NoError(t, err)
			assert.Equal(t, []string{"Hello", "Chosen reply"}, messageContents(messages))
			if mode == "flag ignored" {
				assert.Equal(t, 1, stats.Skipped)
			}
		})
	}
}

func syncOneFetch(t *testing.T, summary string, detail func() (ClaudeAIResponse, error)) func(context.Context, string) (ClaudeAIResponse, error) {
	t.Helper()
	return func(ctx context.Context, path string) (ClaudeAIResponse, error) {
		switch path {
		case "/api/organizations":
			return ClaudeAIResponse{Status: 200, Body: []byte(syncOrgs)}, nil
		case "/api/organizations/org/chat_conversations_v2?limit=50&offset=0&archived=false":
			return ClaudeAIResponse{Status: 200, Body: []byte(`{"data":[` + summary + `],"has_more":false}`)}, nil
		case "/api/organizations/org/chat_conversations_v2?limit=50&offset=0&archived=true":
			return ClaudeAIResponse{Status: 200, Body: []byte(`{"data":[],"has_more":false}`)}, nil
		default:
			require.Equal(t, "/api/organizations/org/chat_conversations/one?tree=True&rendering_mode=messages&consistency=strong&render_all_tools=true&include_inline_comparison=true", path)
			return detail()
		}
	}
}

func TestSyncClaudeAIKeepsNewerImport(t *testing.T) {
	d := testDB(t)
	fetched := false
	fetch := syncOneFetch(t, syncSummary, func() (ClaudeAIResponse, error) {
		fetched = true
		return ClaudeAIResponse{Status: 200, Body: []byte(syncDetail)}, nil
	})
	stats, err := SyncClaudeAI(t.Context(), d, fetch, &ImportCallbacks{SerializeWrite: func(write func() error) error {
		require.True(t, fetched)
		newer := strings.ReplaceAll(strings.Replace(syncDetail, "Hello", "Newer question", 1), "10:05:00Z", "10:06:00Z")
		imported, err := ImportClaudeAI(t.Context(), d, strings.NewReader("["+newer+"]"), nil)
		require.NoError(t, err)
		require.Equal(t, 1, imported.Imported)
		return write()
	}})
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Skipped)
	assert.Zero(t, stats.Updated)
	messages, err := d.GetAllMessages(t.Context(), "claude-ai:one")
	require.NoError(t, err)
	assert.Equal(t, []string{"Newer question", "Chosen reply"}, messageContents(messages))
	session, err := d.GetSession(t.Context(), "claude-ai:one")
	require.NoError(t, err)
	assert.Equal(t, "2026-03-01T10:06:00Z", *session.EndedAt)
	assert.Empty(t, replacedCopies(t, d, "claude-ai:one"))
}

func TestSyncClaudeAIUsageRefresh(t *testing.T) {
	d := testDB(t)
	d.SetArchiveContent(config.ArchiveContentUsage)
	detail := syncDetail
	summary := syncSummary
	calls := 0
	fetch := syncOneFetch(t, summary, func() (ClaudeAIResponse, error) {
		calls++
		return ClaudeAIResponse{Status: 200, Body: []byte(detail)}, nil
	})
	stats, err := SyncClaudeAI(t.Context(), d, fetch, nil)
	require.NoError(t, err)
	require.Equal(t, 1, stats.Imported)
	detail = strings.ReplaceAll(strings.Replace(syncDetail, `"name":"Chat"`, `"name":"Renamed"`, 1), "10:05:00Z", "10:06:00Z")
	detail = strings.Replace(detail, `"current_leaf_message_uuid":"reply"`, `"current_leaf_message_uuid":"a2"`, 1)
	detail = strings.TrimSuffix(detail, "]}") + `,{"uuid":"q2","parent_message_uuid":"reply","sender":"human","text":"","content":[{"type":"text","text":"Next"}]},{"uuid":"a2","parent_message_uuid":"q2","sender":"assistant","text":"","content":[{"type":"text","text":"Answer"}]}]}`
	summary = strings.ReplaceAll(summary, "10:05:00Z", "10:06:00Z")
	summary = strings.Replace(summary, "reply", "a2", 1)
	fetch = syncOneFetch(t, summary, func() (ClaudeAIResponse, error) {
		calls++
		return ClaudeAIResponse{Status: 200, Body: []byte(detail)}, nil
	})
	stats, err = SyncClaudeAI(t.Context(), d, fetch, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Updated)
	session, err := d.GetSession(t.Context(), "claude-ai:one")
	require.NoError(t, err)
	assert.Equal(t, "2026-03-01T10:06:00Z", *session.EndedAt)
	assert.Nil(t, session.DisplayName)
	assert.Nil(t, session.SessionName)
	assert.Equal(t, 4, session.MessageCount)
	assert.Equal(t, 2, session.UserMessageCount)
	messages, err := d.GetAllMessages(t.Context(), "claude-ai:one")
	require.NoError(t, err)
	require.Len(t, messages, 2)
	assert.Equal(t, []string{"", ""}, messageContents(messages))
	assert.Equal(t, 1, messages[0].Ordinal)
	assert.Equal(t, 3, messages[1].Ordinal)
	stats, err = SyncClaudeAI(t.Context(), d, fetch, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Skipped)
	assert.Equal(t, 2, calls)
}

func TestSyncClaudeAIUsageWithoutRows(t *testing.T) {
	d := testDB(t)
	d.SetArchiveContent(config.ArchiveContentUsage)
	summary := strings.Replace(syncSummary, "reply", "root", 1)
	detail := `{"uuid":"one","name":"Chat","current_leaf_message_uuid":"root","created_at":"2026-03-01T10:00:00Z","updated_at":"2026-03-01T10:05:00Z","chat_messages":[{"uuid":"root","index":0,"parent_message_uuid":"00000000-0000-4000-8000-000000000000","sender":"human","text":"","content":[{"type":"text","text":"Hello"}],"created_at":"2026-03-01T10:00:00Z"}]}`
	calls := 0
	fetch := syncOneFetch(t, summary, func() (ClaudeAIResponse, error) {
		calls++
		return ClaudeAIResponse{Status: 200, Body: []byte(detail)}, nil
	})
	stats, err := SyncClaudeAI(t.Context(), d, fetch, nil)
	require.NoError(t, err)
	require.Equal(t, 1, stats.Imported)
	messages, err := d.GetAllMessages(t.Context(), "claude-ai:one")
	require.NoError(t, err)
	assert.Empty(t, messages)
	stats, err = SyncClaudeAI(t.Context(), d, fetch, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Skipped)
	assert.Equal(t, 1, calls)
}

func TestSyncClaudeAIEmptyListSkipsDetail(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(strconv.FormatBool(existing), func(t *testing.T) {
			d := testDB(t)
			if existing {
				_, err := ImportClaudeAI(t.Context(), d, strings.NewReader("["+syncDetail+"]"), nil)
				require.NoError(t, err)
			}
			before, err := d.GetAllMessages(t.Context(), "claude-ai:one")
			require.NoError(t, err)
			summary := strings.Replace(syncSummary, `"current_leaf_message_uuid":"reply"`, `"current_leaf_message_uuid":null`, 1)
			fetch := syncOneFetch(t, summary, func() (ClaudeAIResponse, error) {
				t.Fatal("empty list item fetched its detail")
				return ClaudeAIResponse{}, nil
			})
			for range 2 {
				stats, err := SyncClaudeAI(t.Context(), d, fetch, nil)
				require.NoError(t, err)
				assert.Equal(t, 1, stats.Skipped)
				assert.Zero(t, stats.Imported+stats.Updated+stats.Errors)
			}
			after, err := d.GetAllMessages(t.Context(), "claude-ai:one")
			require.NoError(t, err)
			assert.Equal(t, before, after)
			assert.Empty(t, replacedCopies(t, d, "claude-ai:one"))
			session, err := d.GetSession(t.Context(), "claude-ai:one")
			require.NoError(t, err)
			if existing {
				require.NotNil(t, session)
				assert.Equal(t, "2026-03-01T10:05:00Z", *session.EndedAt)
			} else {
				assert.Nil(t, session)
			}
		})
	}
}

func TestSyncClaudeAIDetailFailures(t *testing.T) {
	for _, tt := range []struct {
		name      string
		status    int
		err       error
		stop      bool
		wantCalls int
	}{
		{name: "bad request", status: 400, wantCalls: 1}, {name: "not found", status: 404, wantCalls: 1}, {name: "exhausted retries", status: 503, wantCalls: 5},
		{name: "oversize", err: ErrClaudeAIResponseTooLarge, wantCalls: 1}, {name: "unauthorized", status: 401, stop: true, wantCalls: 1}, {name: "forbidden", status: 403, stop: true, wantCalls: 1},
		{name: "transport", err: errors.New("transport failed"), stop: true, wantCalls: 1}, {name: "cancelled", err: context.Canceled, stop: true, wantCalls: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				d := testDB(t)
				calls := 0
				later := 0
				fetch := syncOneFetch(t, syncSummary+","+strings.ReplaceAll(syncSummary, "one", "two"), func() (ClaudeAIResponse, error) {
					calls++
					return ClaudeAIResponse{Status: tt.status, RetryAfter: "1"}, tt.err
				})
				stats, err := SyncClaudeAI(t.Context(), d, func(ctx context.Context, path string) (ClaudeAIResponse, error) {
					if strings.Contains(path, "/chat_conversations/two?") {
						later++
						return ClaudeAIResponse{Status: 200, Body: []byte(strings.ReplaceAll(syncDetail, "one", "two"))}, nil
					}
					return fetch(ctx, path)
				}, nil)
				assert.Equal(t, tt.wantCalls, calls)
				if tt.stop {
					require.Error(t, err)
					assert.Zero(t, later)
				} else {
					require.NoError(t, err)
					assert.Equal(t, 1, later)
					if tt.status == 404 {
						assert.Equal(t, 1, stats.Skipped)
						assert.Zero(t, stats.Errors)
					} else {
						assert.Equal(t, 1, stats.Errors)
					}
					assert.Equal(t, 1, stats.Imported)
				}
			})
		})
	}
}

func TestSyncClaudeAIMalformedRetries(t *testing.T) {
	for _, detail := range []string{"null", `{"uuid":"one"}`, strings.ReplaceAll(syncDetail, "10:05:00Z", "bad")} {
		d := testDB(t)
		calls := 0
		fetch := syncOneFetch(t, syncSummary, func() (ClaudeAIResponse, error) {
			calls++
			return ClaudeAIResponse{Status: 200, Body: []byte(detail)}, nil
		})
		for range 2 {
			stats, err := SyncClaudeAI(t.Context(), d, fetch, nil)
			require.NoError(t, err)
			assert.Equal(t, 1, stats.Errors)
			session, err := d.GetSession(t.Context(), "claude-ai:one")
			require.NoError(t, err)
			assert.Nil(t, session)
		}
		assert.Equal(t, 2, calls)
	}
}

func TestSyncClaudeAIBranchSwitch(t *testing.T) {
	for _, sameText := range []bool{false, true} {
		t.Run(strconv.FormatBool(sameText), func(t *testing.T) {
			d := testDB(t)
			detail := strings.TrimSuffix(syncDetail, "]}") + `,{"uuid":"retry","index":2,"parent_message_uuid":"root","sender":"assistant","text":"","content":[{"type":"text","text":"Retried reply"}],"created_at":"2026-03-01T10:02:00Z"}]}`
			want := "Retried reply"
			if sameText {
				detail = strings.Replace(detail, "Retried reply", "Chosen reply", 1)
				want = "Chosen reply"
			}
			calls := 0
			fetchDetail := func() (ClaudeAIResponse, error) {
				calls++
				return ClaudeAIResponse{Status: 200, Body: []byte(detail)}, nil
			}
			stats, err := SyncClaudeAI(t.Context(), d, syncOneFetch(t, syncSummary, fetchDetail), nil)
			require.NoError(t, err)
			require.Equal(t, 1, stats.Imported)
			before, err := d.GetAllMessages(t.Context(), "claude-ai:one")
			require.NoError(t, err)
			require.Len(t, before, 2)
			assert.Equal(t, "root", before[0].SourceUUID)
			assert.Equal(t, "reply", before[1].SourceUUID)
			detail = strings.Replace(detail, `"current_leaf_message_uuid":"reply"`, `"current_leaf_message_uuid":"retry"`, 1)
			fetch := syncOneFetch(t, strings.Replace(syncSummary, "reply", "retry", 1), fetchDetail)
			stats, err = SyncClaudeAI(t.Context(), d, fetch, nil)
			require.NoError(t, err)
			assert.Equal(t, 1, stats.Updated)
			assert.Zero(t, stats.Errors)
			after, err := d.GetAllMessages(t.Context(), "claude-ai:one")
			require.NoError(t, err)
			require.Len(t, after, 2)
			assert.Equal(t, []string{"Hello", want}, messageContents(after))
			assert.Equal(t, "retry", after[1].SourceUUID)
			require.Len(t, replacedCopies(t, d, "claude-ai:one"), 1)
			stats, err = SyncClaudeAI(t.Context(), d, fetch, nil)
			require.NoError(t, err)
			assert.Equal(t, 1, stats.Skipped)
			assert.Equal(t, 2, calls)
		})
	}
}

func TestSyncClaudeAILegacyRowsStayUnchanged(t *testing.T) {
	for _, policy := range []config.ArchiveContent{config.ArchiveContentFull, config.ArchiveContentTranscripts} {
		t.Run(string(policy), func(t *testing.T) {
			d := testDB(t)
			d.SetArchiveContent(policy)
			stats, err := ImportClaudeAI(t.Context(), d, strings.NewReader("["+syncDetail+"]"), nil)
			require.NoError(t, err)
			require.Equal(t, 1, stats.Imported)
			before, err := d.GetAllMessages(t.Context(), "claude-ai:one")
			require.NoError(t, err)
			require.Len(t, before, 2)
			assert.Empty(t, before[1].SourceUUID)
			fetch := syncOneFetch(t, syncSummary, func() (ClaudeAIResponse, error) {
				return ClaudeAIResponse{Status: 200, Body: []byte(syncDetail)}, nil
			})
			stats, err = SyncClaudeAI(t.Context(), d, fetch, nil)
			require.NoError(t, err)
			assert.Equal(t, 1, stats.Updated)
			assert.Empty(t, stats.Refusals)
			assert.Empty(t, replacedCopies(t, d, "claude-ai:one"))
			after, err := d.GetAllMessages(t.Context(), "claude-ai:one")
			require.NoError(t, err)
			assert.Equal(t, before, after)
			stats, err = ImportClaudeAI(t.Context(), d, strings.NewReader("["+syncDetail+"]"), nil)
			require.NoError(t, err)
			assert.Equal(t, 1, stats.Skipped)
		})
	}
}
