package importer

import (
	"context"
	"database/sql"
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
			require.Equal(t, "/api/organizations/org/chat_conversations_v2?limit=50&offset=0", path)
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
					case "/api/organizations":
						return ClaudeAIResponse{Status: 200, Body: []byte(syncOrgs)}, nil
					case "/api/organizations/org/chat_conversations_v2?limit=50&offset=0":
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
				case "/api/organizations":
					return ClaudeAIResponse{Status: 200, Body: []byte(syncOrgs)}, nil
				case "/api/organizations/org/chat_conversations_v2?limit=50&offset=0":
					return ClaudeAIResponse{Status: 200, Body: []byte(`{"data":[` + syncSummary + `],"has_more":true}`)}, nil
				case "/api/organizations/org/chat_conversations_v2?limit=50&offset=1":
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
	t.Run("overlapping pages import later chats", func(t *testing.T) {
		d := testDB(t)
		details := 0
		stats, err := SyncClaudeAI(t.Context(), d, func(ctx context.Context, path string) (ClaudeAIResponse, error) {
			switch path {
			case "/api/organizations":
				return ClaudeAIResponse{Status: 200, Body: []byte(syncOrgs)}, nil
			case "/api/organizations/org/chat_conversations_v2?limit=50&offset=0":
				return ClaudeAIResponse{Status: 200, Body: []byte(`{"data":[` + syncSummary + `],"has_more":true}`)}, nil
			case "/api/organizations/org/chat_conversations_v2?limit=50&offset=1":
				return ClaudeAIResponse{Status: 200, Body: []byte(`{"data":[` + syncSummary + `,` + strings.ReplaceAll(strings.Replace(syncSummary, `"is_starred":true`, `"is_archived":true`, 1), "one", "two") + `],"has_more":true}`)}, nil
			case "/api/organizations/org/chat_conversations_v2?limit=50&offset=3":
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
				require.Equal(t, "/api/organizations/org/chat_conversations_v2?limit=50&offset=0", path)
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

func (s trashDuringImportStore) WriteSessionBatchAtomic(ctx context.Context, writes []db.SessionBatchWrite, beforeCommit ...func() error) (db.SessionBatchResult, error) {
	if err := s.DB.SoftDeleteSession(ctx, writes[0].Session.ID); err != nil {
		return db.SessionBatchResult{}, err
	}
	return s.DB.WriteSessionBatchAtomic(ctx, writes, beforeCommit...)
}

func (s trashDuringImportStore) ReplaceSessionKeepingTrashedCopy(ctx context.Context, write db.SessionBatchWrite) (string, error) {
	if err := s.DB.SoftDeleteSession(ctx, write.Session.ID); err != nil {
		return "", err
	}
	return s.DB.ReplaceSessionKeepingTrashedCopy(ctx, write)
}

type interruptedSyncStore struct {
	*db.DB
	fail   error
	cancel context.CancelFunc
}

func (s *interruptedSyncStore) WriteSessionBatchAtomic(ctx context.Context, writes []db.SessionBatchWrite, beforeCommit ...func() error) (db.SessionBatchResult, error) {
	if s.fail != nil {
		beforeCommit = []func() error{func() error { return s.fail }}
	}
	result, err := s.DB.WriteSessionBatchAtomic(ctx, writes, beforeCommit...)
	if err == nil && s.cancel != nil {
		s.cancel()
	}
	return result, err
}

func TestSyncClaudeAIInterruptedWrite(t *testing.T) {
	t.Run("unchanged text rolls back metadata and marker", func(t *testing.T) {
		d := testDB(t)
		_, err := ImportClaudeAI(t.Context(), d, strings.NewReader("["+syncDetail+"]"), nil)
		require.NoError(t, err)
		require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
			_, err := tx.ExecContext(t.Context(), "UPDATE sessions SET local_modified_at = '2026-03-01T10:00:00Z' WHERE id = 'claude-ai:one'")
			return err
		}))
		before, err := d.GetSessionFull(t.Context(), "claude-ai:one")
		require.NoError(t, err)
		calls := 0
		fetch := syncOneFetch(t, strings.ReplaceAll(syncSummary, "10:05:00Z", "10:06:00Z"), func() (ClaudeAIResponse, error) {
			calls++
			return ClaudeAIResponse{Status: 200, Body: []byte(strings.ReplaceAll(syncDetail, "10:05:00Z", "10:06:00Z"))}, nil
		})
		stats, err := SyncClaudeAI(t.Context(), &interruptedSyncStore{DB: d, fail: errors.New("commit failed")}, fetch, nil)
		require.NoError(t, err)
		assert.Equal(t, 1, stats.Errors)
		after, err := d.GetSessionFull(t.Context(), "claude-ai:one")
		require.NoError(t, err)
		assert.Equal(t, before, after)
		stats, err = SyncClaudeAI(t.Context(), d, fetch, nil)
		require.NoError(t, err)
		assert.Equal(t, 1, stats.Updated)
		after, err = d.GetSessionFull(t.Context(), "claude-ai:one")
		require.NoError(t, err)
		require.NotNil(t, after)
		assert.Equal(t, strPtr("2026-03-01T10:06:00Z"), after.EndedAt)
		assert.Equal(t, strPtr("reply"), after.LastEntryUUID)
		assert.NotEqual(t, before.LocalModifiedAt, after.LocalModifiedAt)
		stats, err = SyncClaudeAI(t.Context(), d, fetch, nil)
		require.NoError(t, err)
		assert.Equal(t, 1, stats.Skipped)
		assert.Equal(t, 2, calls)
	})
	for _, committed := range []bool{false, true} {
		t.Run(strconv.FormatBool(committed), func(t *testing.T) {
			d := testDB(t)
			oldDetail := strings.Replace(syncDetail, "10:05:00Z", "10:04:00Z", 1)
			_, err := SyncClaudeAI(t.Context(), d, syncOneFetch(t, strings.Replace(syncSummary, "10:05:00Z", "10:04:00Z", 1), func() (ClaudeAIResponse, error) {
				return ClaudeAIResponse{Status: 200, Body: []byte(oldDetail)}, nil
			}), nil)
			require.NoError(t, err)
			beforeSession, err := d.GetSessionFull(t.Context(), "claude-ai:one")
			require.NoError(t, err)
			require.NotNil(t, beforeSession)
			beforeMessages, err := d.GetAllMessages(t.Context(), "claude-ai:one")
			require.NoError(t, err)
			detail := strings.TrimSuffix(strings.Replace(syncDetail, `"current_leaf_message_uuid":"reply"`, `"current_leaf_message_uuid":"a2"`, 1), "]}") + `,{"uuid":"q2","parent_message_uuid":"reply","sender":"human","text":"More"},{"uuid":"a2","parent_message_uuid":"q2","sender":"assistant","text":"Answer"}]}`
			details := 0
			fetch := syncOneFetch(t, strings.Replace(syncSummary, "reply", "a2", 1), func() (ClaudeAIResponse, error) {
				details++
				return ClaudeAIResponse{Status: 200, Body: []byte(detail)}, nil
			})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			store := &interruptedSyncStore{DB: d, fail: errors.New("message write failed")}
			if committed {
				store.fail, store.cancel = nil, cancel
			}
			stats, err := SyncClaudeAI(ctx, store, fetch, nil)
			session, readErr := d.GetSessionFull(t.Context(), "claude-ai:one")
			require.NoError(t, readErr)
			require.NotNil(t, session)
			messages, readErr := d.GetAllMessages(t.Context(), session.ID)
			require.NoError(t, readErr)
			if committed {
				require.ErrorIs(t, err, context.Canceled)
				assert.Equal(t, 1, stats.Updated)
				assert.Zero(t, stats.Errors)
				assert.Equal(t, 4, session.MessageCount)
				assert.Equal(t, strPtr("2026-03-01T10:05:00Z"), session.EndedAt)
				assert.Equal(t, strPtr("a2"), session.LastEntryUUID)
				assert.Equal(t, []string{"Hello", "Chosen reply", "More", "Answer"}, messageContents(messages))
			} else {
				require.NoError(t, err)
				assert.Equal(t, 1, stats.Errors)
				assert.Zero(t, stats.Updated)
				assert.Equal(t, beforeSession.MessageCount, session.MessageCount)
				assert.Equal(t, beforeSession.EndedAt, session.EndedAt)
				assert.Equal(t, beforeSession.LastEntryUUID, session.LastEntryUUID)
				assert.Equal(t, beforeMessages, messages)
			}
			stats, err = SyncClaudeAI(t.Context(), d, fetch, nil)
			require.NoError(t, err)
			if committed {
				assert.Equal(t, 1, stats.Skipped)
				assert.Equal(t, 1, details)
			} else {
				assert.Equal(t, 1, stats.Updated)
				assert.Equal(t, 2, details)
			}
		})
	}
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

func syncOneFetch(t *testing.T, summary string, detail func() (ClaudeAIResponse, error)) func(context.Context, string) (ClaudeAIResponse, error) {
	t.Helper()
	return func(ctx context.Context, path string) (ClaudeAIResponse, error) {
		switch path {
		case "/api/organizations":
			return ClaudeAIResponse{Status: 200, Body: []byte(syncOrgs)}, nil
		case "/api/organizations/org/chat_conversations_v2?limit=50&offset=0":
			return ClaudeAIResponse{Status: 200, Body: []byte(`{"data":[` + summary + `],"has_more":false}`)}, nil
		default:
			require.Equal(t, "/api/organizations/org/chat_conversations/one?tree=True&rendering_mode=messages&consistency=strong&render_all_tools=true&include_inline_comparison=true", path)
			return detail()
		}
	}
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
		wantCalls int
	}{
		{name: "bad request", status: 400, wantCalls: 1}, {name: "not found", status: 404, wantCalls: 1}, {name: "exhausted retries", status: 503, wantCalls: 5},
		{name: "oversize", err: ErrClaudeAIResponseTooLarge, wantCalls: 1}, {name: "unauthorized", status: 401, wantCalls: 1}, {name: "forbidden", status: 403, wantCalls: 1},
		{name: "transport", err: errors.New("transport failed"), wantCalls: 1}, {name: "host cancelled", err: context.Canceled, wantCalls: 1},
		{name: "host deadline", err: context.DeadlineExceeded, wantCalls: 1},
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
				require.NoError(t, err)
				assert.Equal(t, 1, later)
				if tt.status == 404 {
					assert.Equal(t, 1, stats.Skipped)
					assert.Zero(t, stats.Errors)
				} else {
					assert.Equal(t, 1, stats.Errors)
					assert.Equal(t, []ImportRefusal{{SessionID: "claude-ai:one", Reason: RefusalTransient}}, stats.Refusals)
				}
				assert.Equal(t, 1, stats.Imported)
			})
		})
	}
}

func TestSyncClaudeAIMalformedRetries(t *testing.T) {
	for _, detail := range []string{"null", `{"uuid":"one"}`, strings.ReplaceAll(syncDetail, "10:05:00Z", "bad"), strings.Replace(syncDetail, `"current_leaf_message_uuid":"reply"`, `"current_leaf_message_uuid":null`, 1)} {
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
	d := testDB(t)
	stats, err := ImportClaudeAI(t.Context(), d, strings.NewReader("["+syncDetail+"]"), nil)
	require.NoError(t, err)
	require.Equal(t, 1, stats.Imported)
	require.NoError(t, d.ReplaceSessionSecretFindings(t.Context(), "claude-ai:one", []db.SecretFinding{{RuleName: "test-secret", Confidence: "high", LocationKind: "message", MessageOrdinal: 0, MatchStart: 0, MatchEnd: 5, RedactedMatch: "[redacted]"}}, 1, "test-rules"))
	findings, err := d.SessionSecretFindings(t.Context(), "claude-ai:one")
	require.NoError(t, err)
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
	afterFindings, err := d.SessionSecretFindings(t.Context(), "claude-ai:one")
	require.NoError(t, err)
	assert.Equal(t, findings, afterFindings)
	assert.Empty(t, replacedCopies(t, d, "claude-ai:one"))
	marked, err := d.GetSessionFull(t.Context(), "claude-ai:one")
	require.NoError(t, err)
	require.NotNil(t, marked)
	assert.Equal(t, strPtr("reply"), marked.LastEntryUUID)
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
}

func TestSyncClaudeAIBranchSwitch(t *testing.T) {
	const extra = `,{"uuid":"q2","parent_message_uuid":"reply","sender":"human","text":"More"},{"uuid":"a2","parent_message_uuid":"q2","sender":"assistant","text":"Answer"},{"uuid":"edit","parent_message_uuid":"reply","sender":"human","text":"Edited question"},{"uuid":"b2","parent_message_uuid":"edit","sender":"assistant","text":"Edited answer"}]}`
	for _, leaf := range []string{"reply", "b2"} {
		t.Run(leaf, func(t *testing.T) {
			d := testDB(t)
			selected := "a2"
			fetch := func(ctx context.Context, path string) (ClaudeAIResponse, error) {
				detail := strings.Replace(strings.TrimSuffix(syncDetail, "]}")+extra, `"current_leaf_message_uuid":"reply"`, `"current_leaf_message_uuid":"`+selected+`"`, 1)
				return syncOneFetch(t, strings.Replace(syncSummary, "reply", selected, 1), func() (ClaudeAIResponse, error) {
					return ClaudeAIResponse{Status: 200, Body: []byte(detail)}, nil
				})(ctx, path)
			}
			stats, err := SyncClaudeAI(t.Context(), d, fetch, nil)
			require.NoError(t, err)
			require.Equal(t, 1, stats.Imported)
			before, err := d.GetAllMessages(t.Context(), "claude-ai:one")
			require.NoError(t, err)
			require.Len(t, before, 4)
			for _, message := range []db.Message{before[1], before[3]} {
				_, err := d.PinMessage(t.Context(), "claude-ai:one", message.ID, nil)
				require.NoError(t, err)
			}
			for flip := 0; flip < 5; flip++ {
				selected = leaf
				if flip%2 == 1 {
					selected = "a2"
				}
				stats, err = SyncClaudeAI(t.Context(), d, fetch, nil)
				require.NoError(t, err)
				assert.Equal(t, 1, stats.Updated)
				assert.Zero(t, stats.Errors)
				assert.Empty(t, stats.Refusals)
				messages, err := d.GetAllMessages(t.Context(), "claude-ai:one")
				require.NoError(t, err)
				want := []string{"Hello", "Chosen reply"}
				if selected == "a2" {
					want = append(want, "More", "Answer")
				} else if selected == "b2" {
					want = append(want, "Edited question", "Edited answer")
				}
				assert.Equal(t, want, messageContents(messages))
				pins, err := d.ListPinnedMessages(t.Context(), "claude-ai:one", "")
				require.NoError(t, err)
				require.Len(t, pins, 1)
				assert.Equal(t, messages[1].ID, pins[0].MessageID)
				assert.Empty(t, replacedCopies(t, d, "claude-ai:one"))
			}
		})
	}
}

func TestSyncClaudeAIShorterZipStillRefused(t *testing.T) {
	d := testDB(t)
	_, err := SyncClaudeAI(t.Context(), d, syncOneFetch(t, syncSummary, func() (ClaudeAIResponse, error) {
		return ClaudeAIResponse{Status: 200, Body: []byte(syncDetail)}, nil
	}), nil)
	require.NoError(t, err)
	stats, err := ImportClaudeAI(t.Context(), d, strings.NewReader(`[{"uuid":"one","created_at":"2026-03-01T10:00:00Z","updated_at":"2026-03-01T10:04:00Z","chat_messages":[{"sender":"human","text":"Hello"}]}]`), nil)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Errors)
	assert.Equal(t, []ImportRefusal{{SessionID: "claude-ai:one", Reason: RefusalShorterExport}}, stats.Refusals)
	marked, err := d.GetSessionFull(t.Context(), "claude-ai:one")
	require.NoError(t, err)
	require.NotNil(t, marked)
	assert.Equal(t, strPtr("reply"), marked.LastEntryUUID)
}
