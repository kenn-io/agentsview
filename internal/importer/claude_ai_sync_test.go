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
	t.Run("trashed during sync write is skipped", func(t *testing.T) {
		d := testDB(t)
		_, err := ImportClaudeAI(t.Context(), d, strings.NewReader("["+strings.Replace(syncDetail, "10:05:00Z", "10:04:00Z", 1)+"]"), nil)
		require.NoError(t, err)
		stats, err := SyncClaudeAI(t.Context(), trashDuringImportStore{d}, syncOneFetch(t, syncSummary, func() (ClaudeAIResponse, error) {
			return ClaudeAIResponse{Status: 200, Body: []byte(syncDetail)}, nil
		}), nil)
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
		details := 0
		fetch := syncOneFetch(t, syncSummary, func() (ClaudeAIResponse, error) {
			details++
			return ClaudeAIResponse{Status: 200, Body: []byte(strings.Replace(syncDetail, `"chat_messages":[`, `"chat_messages":[
					{"uuid":"abandoned","parent_message_uuid":"root","sender":"assistant","text":"","content":[{"type":"text","text":"Abandoned reply"}]},`, 1))}, nil
		})
		stats, err := SyncClaudeAI(t.Context(), d, fetch, nil)
		require.NoError(t, err)
		assert.Equal(t, 1, stats.Imported)
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
		fetch := syncOneFetch(t, syncSummary, func() (ClaudeAIResponse, error) {
			details++
			return ClaudeAIResponse{Status: 200, Body: []byte(syncDetail)}, nil
		})
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

func (s trashDuringImportStore) UpsertSession(ctx context.Context, session db.Session) error {
	if err := s.DB.SoftDeleteSession(ctx, session.ID); err != nil {
		return err
	}
	return s.DB.UpsertSession(ctx, session)
}

type cancelSyncStore struct {
	*db.DB
	cancel context.CancelFunc
}

func (s *cancelSyncStore) UpsertSession(ctx context.Context, session db.Session) error {
	s.cancel()
	return ctx.Err()
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
			fetch := syncOneFetch(t, summary, func() (ClaudeAIResponse, error) {
				details++
				return ClaudeAIResponse{Status: 200, Body: []byte(strings.ReplaceAll(tt.detail, "10:05:00Z", "10:06:00Z"))}, nil
			})
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

func TestSyncClaudeAIZipFreshness(t *testing.T) {
	for _, policy := range []config.ArchiveContent{config.ArchiveContentFull, config.ArchiveContentTranscripts, config.ArchiveContentUsage} {
		t.Run(string(policy), func(t *testing.T) {
			d := testDB(t)
			d.SetArchiveContent(policy)
			stats, err := ImportClaudeAI(t.Context(), d, strings.NewReader("["+syncDetail+"]"), nil)
			require.NoError(t, err)
			require.Equal(t, 1, stats.Imported)
			before, err := d.GetAllMessages(t.Context(), "claude-ai:one")
			require.NoError(t, err)
			calls := 0
			fetch := syncOneFetch(t, syncSummary, func() (ClaudeAIResponse, error) {
				calls++
				return ClaudeAIResponse{Status: 200, Body: []byte(syncDetail)}, nil
			})
			for range 2 {
				stats, err = SyncClaudeAI(t.Context(), d, fetch, nil)
				require.NoError(t, err)
				assert.Equal(t, 1, stats.Skipped)
			}
			assert.Equal(t, 1, calls)
			after, err := d.GetAllMessages(t.Context(), "claude-ai:one")
			require.NoError(t, err)
			assert.Equal(t, before, after)
			assert.Empty(t, replacedCopies(t, d, "claude-ai:one"))
			_, err = ImportClaudeAI(t.Context(), d, strings.NewReader("["+syncDetail+"]"), nil)
			require.NoError(t, err)
			session, err := d.GetSessionFull(t.Context(), "claude-ai:one")
			require.NoError(t, err)
			require.NotNil(t, session)
			assert.Nil(t, session.LastEntryUUID)
			for range 2 {
				stats, err = SyncClaudeAI(t.Context(), d, fetch, nil)
				require.NoError(t, err)
				assert.Equal(t, 1, stats.Skipped)
			}
			assert.Equal(t, 2, calls)
		})
	}
}

func TestSyncClaudeAIUsageWithoutRows(t *testing.T) {
	d := testDB(t)
	d.SetArchiveContent(config.ArchiveContentUsage)
	detail := `{"uuid":"one","current_leaf_message_uuid":"root","created_at":"2026-03-01T10:00:00Z","updated_at":"2026-03-01T10:05:00Z","chat_messages":[{"uuid":"root","sender":"human","text":"Hello"}]}`
	calls := 0
	fetch := syncOneFetch(t, strings.Replace(syncSummary, "reply", "root", 1), func() (ClaudeAIResponse, error) {
		calls++
		return ClaudeAIResponse{Status: 200, Body: []byte(detail)}, nil
	})
	stats, err := SyncClaudeAI(t.Context(), d, fetch, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Imported)
	messages, err := d.GetAllMessages(t.Context(), "claude-ai:one")
	require.NoError(t, err)
	assert.Empty(t, messages)
	stats, err = SyncClaudeAI(t.Context(), d, fetch, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Skipped)
	assert.Equal(t, 1, calls)
}

func TestSyncClaudeAIBranchSwitch(t *testing.T) {
	const extra = `,{"uuid":"q2","parent_message_uuid":"reply","sender":"human","text":"More","created_at":"2026-03-01T10:03:00Z"},{"uuid":"a2","parent_message_uuid":"q2","sender":"assistant","text":"Answer","created_at":"2026-03-01T10:04:00Z"},{"uuid":"retry","parent_message_uuid":"root","sender":"assistant","text":"Retried reply","created_at":"2026-03-01T10:05:00Z"},{"uuid":"q3","parent_message_uuid":"reply","sender":"human","text":"Changed question","created_at":"2026-03-01T10:03:00Z"},{"uuid":"b2","parent_message_uuid":"q3","sender":"assistant","text":"Changed answer","created_at":"2026-03-01T10:05:00Z"}`
	for _, policy := range []config.ArchiveContent{config.ArchiveContentFull, config.ArchiveContentTranscripts, config.ArchiveContentUsage} {
		for _, leaf := range []string{"retry", "b2"} {
			t.Run(string(policy)+"/"+leaf, func(t *testing.T) {
				d := testDB(t)
				d.SetArchiveContent(policy)
				detail := strings.Replace(strings.TrimSuffix(syncDetail, "]}")+extra+"]}", `"current_leaf_message_uuid":"reply"`, `"current_leaf_message_uuid":"a2"`, 1)
				summary := strings.Replace(syncSummary, "reply", "a2", 1)
				calls := 0
				getDetail := func() (ClaudeAIResponse, error) {
					calls++
					return ClaudeAIResponse{Status: 200, Body: []byte(detail)}, nil
				}
				stats, err := SyncClaudeAI(t.Context(), d, syncOneFetch(t, summary, getDetail), nil)
				require.NoError(t, err)
				require.Equal(t, 1, stats.Imported)
				oldLeaf := "a2"
				detail = strings.Replace(detail, `"current_leaf_message_uuid":"`+oldLeaf+`"`, `"current_leaf_message_uuid":"`+leaf+`"`, 1)
				summary = strings.Replace(summary, oldLeaf, leaf, 1)
				fetch := syncOneFetch(t, summary, getDetail)
				stats, err = SyncClaudeAI(t.Context(), d, fetch, nil)
				require.NoError(t, err)
				assert.Equal(t, 1, stats.Updated)
				assert.Zero(t, stats.Errors)
				session, err := d.GetSessionFull(t.Context(), "claude-ai:one")
				require.NoError(t, err)
				require.NotNil(t, session)
				require.NotNil(t, session.LastEntryUUID)
				assert.Equal(t, leaf, *session.LastEntryUUID)
				count := 4
				if leaf == "retry" {
					count = 2
				}
				assert.Equal(t, count, session.MessageCount)
				assert.Equal(t, count/2, session.UserMessageCount)
				messages, err := d.GetAllMessages(t.Context(), session.ID)
				require.NoError(t, err)
				if policy == config.ArchiveContentUsage {
					require.Len(t, messages, count/2)
					for i, m := range messages {
						assert.Equal(t, "assistant", m.Role)
						assert.Empty(t, m.Content)
						assert.Equal(t, 2*i+1, m.Ordinal)
					}
					if leaf == "retry" {
						assert.Equal(t, "2026-03-01T10:05:00Z", messages[0].Timestamp)
					} else {
						assert.Equal(t, "2026-03-01T10:02:00Z", messages[0].Timestamp)
						assert.Equal(t, "2026-03-01T10:05:00Z", messages[1].Timestamp)
					}
				} else if leaf == "retry" {
					assert.Equal(t, []string{"Hello", "Retried reply"}, messageContents(messages))
				} else {
					assert.Equal(t, []string{"Hello", "Chosen reply", "Changed question", "Changed answer"}, messageContents(messages))
				}
				if leaf == "retry" {
					require.Len(t, replacedCopies(t, d, session.ID), 1)
				} else {
					assert.Empty(t, replacedCopies(t, d, session.ID))
				}
				stats, err = SyncClaudeAI(t.Context(), d, fetch, nil)
				require.NoError(t, err)
				assert.Equal(t, 1, stats.Skipped)
				assert.Equal(t, 2, calls)
			})
		}
	}
}
