package importer

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

const syncSummary = `{"uuid":"one","name":"Chat","is_starred":true,"created_at":"2026-03-01T10:00:00Z","updated_at":"2026-03-01T10:05:00Z"}`
const syncDetail = `{"uuid":"one","name":"Chat","created_at":"2026-03-01T10:00:00Z","updated_at":"2026-03-01T10:05:00Z","chat_messages":[
    {"uuid":"root","index":0,"parent_message_uuid":"00000000-0000-4000-8000-000000000000","sender":"human","text":"Hello","created_at":"2026-03-01T10:00:00Z"},
    {"uuid":"reply","index":1,"parent_message_uuid":"root","sender":"assistant","text":"Chosen reply","created_at":"2026-03-01T10:02:00Z"}]}`
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
			detail := strings.TrimSuffix(syncDetail, "]}") + `,{"uuid":"q2","parent_message_uuid":"reply","sender":"human","text":"Second question"},{"uuid":"a2","parent_message_uuid":"q2","sender":"assistant","text":"Second answer"}],"current_leaf_message_uuid":"a2"}`
			summary := syncSummary
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
				detail = strings.Replace(syncDetail, `"chat_messages":`, `"current_leaf_message_uuid":"reply","chat_messages":`, 1)
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
				return ClaudeAIResponse{Status: 200, Body: []byte(strings.Replace(syncDetail, `"chat_messages":[`, `"current_leaf_message_uuid":"reply","chat_messages":[
					{"uuid":"abandoned","parent_message_uuid":"root","sender":"assistant","text":"Abandoned reply"},`, 1))}, nil
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
	t.Run("malformed and empty details allow later chats", func(t *testing.T) {
		for _, tt := range []struct {
			name    string
			detail  string
			errors  int
			skipped int
		}{
			{name: "null", detail: "null", errors: 1},
			{name: "bad timestamp", detail: strings.ReplaceAll(syncDetail, "10:05:00Z", "bad"), errors: 1},
			{name: "empty", detail: `{"uuid":"one","name":"Empty","created_at":"2026-03-01T10:00:00Z","updated_at":"2026-03-01T10:05:00Z","chat_messages":[]}`, skipped: 1},
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
				assert.Equal(t, tt.skipped, stats.Skipped)
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
		{name: "grown history preserves ids", detail: strings.TrimSuffix(syncDetail, "]}") + `,{"uuid":"q2","parent_message_uuid":"reply","sender":"human","text":"More"},{"uuid":"a2","parent_message_uuid":"q2","sender":"assistant","text":"Answer"}]}`, want: []string{"Hello", "Chosen reply", "More", "Answer"}},
		{name: "rename refreshes ended at", detail: strings.Replace(syncDetail, `"name":"Chat"`, `"name":"Renamed"`, 1), want: []string{"Hello", "Chosen reply"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDB(t)
			stats, err := ImportClaudeAI(t.Context(), d, strings.NewReader("["+syncDetail+"]"), nil)
			require.NoError(t, err)
			require.Equal(t, 1, stats.Imported)
			before, err := d.GetAllMessages(t.Context(), "claude-ai:one")
			require.NoError(t, err)
			details := 0
			fetch := func(ctx context.Context, path string) (ClaudeAIResponse, error) {
				switch path {
				case "/api/organizations":
					return ClaudeAIResponse{Status: 200, Body: []byte(syncOrgs)}, nil
				case "/api/organizations/org/chat_conversations_v2?limit=50&offset=0&archived=false":
					return ClaudeAIResponse{Status: 200, Body: []byte(`{"data":[` + strings.ReplaceAll(syncSummary, "10:05:00Z", "10:06:00Z") + `],"has_more":false}`)}, nil
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
