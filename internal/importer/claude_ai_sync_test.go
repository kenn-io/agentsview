package importer

import (
	"context"
	_ "embed"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

var syncSummary = func() string {
	var page struct {
		Data []jsontext.Value `json:"data"`
	}
	if err := json.Unmarshal([]byte(syncAllList), &page); err != nil {
		panic(err)
	}
	return string(page.Data[0])
}()

//go:embed testdata/claude_ai_sync/detail.json
var syncDetail string

//go:embed testdata/claude_ai_sync/organizations.json
var syncOrgs string

//go:embed testdata/claude_ai_sync/list_all.json
var syncAllList string

func TestSyncClaudeAIResyncRestoresTranscript(t *testing.T) {
	const id = "claude-ai:22222222-2222-4222-8222-222222222222"
	source := testDB(t)
	details := 0
	fetch := syncOneFetch(t, syncSummary, func() (ClaudeAIResponse, error) {
		details++
		return ClaudeAIResponse{Status: 200, Body: []byte(syncDetail)}, nil
	})
	_, err := SyncClaudeAI(t.Context(), source, fetch, nil)
	require.NoError(t, err)
	require.NoError(t, source.Close())
	path := filepath.Join(t.TempDir(), "archive.db")
	destination, err := db.OpenWithArchiveContent(t.Context(), path, config.ArchiveContentUsage)
	require.NoError(t, err)
	_, err = destination.CopyOrphanedDataFrom(source.Path())
	require.NoError(t, err)
	before, err := destination.GetAllMessages(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, []string{""}, messageContents(before))
	require.NoError(t, destination.Close())
	destination, err = db.OpenWithArchiveContent(t.Context(), path, config.ArchiveContentFull)
	require.NoError(t, err)
	t.Cleanup(func() { destination.Close() })
	_, err = SyncClaudeAI(t.Context(), destination, fetch, nil)
	require.NoError(t, err)
	assert.Equal(t, 2, details)
	after, err := destination.GetAllMessages(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, []string{"Hello", "Chosen reply"}, messageContents(after))
}

func TestSyncClaudeAIInvalidAccountSession(t *testing.T) {
	body, err := os.ReadFile("testdata/claude_ai_sync/signed_out.json")
	require.NoError(t, err)
	paths := []string{
		"/api/organizations",
		"/api/organizations/11111111-1111-4111-8111-111111111111/chat_conversations_v2?limit=50&offset=0",
		"/api/organizations/11111111-1111-4111-8111-111111111111/chat_conversations/22222222-2222-4222-8222-222222222222?tree=True&rendering_mode=messages&consistency=strong&render_all_tools=true&include_inline_comparison=true",
	}
	for stage, path := range paths {
		for _, status := range []int{401, 403, 429, 503} {
			t.Run(path+"/"+strconv.Itoa(status), func(t *testing.T) {
				d := testDB(t)
				calls := 0
				stats, err := SyncClaudeAI(t.Context(), d, func(ctx context.Context, got string) (ClaudeAIResponse, error) {
					require.Less(t, calls, len(paths))
					require.Equal(t, paths[calls], got)
					calls++
					if got == path {
						return ClaudeAIResponse{Status: status, Body: body}, nil
					}
					if got == paths[0] {
						return ClaudeAIResponse{Status: 200, Body: []byte(syncOrgs)}, nil
					}
					return ClaudeAIResponse{Status: 200, Body: []byte(`{"data":[` + syncSummary + `],"has_more":false}`)}, nil
				}, nil)
				require.ErrorIs(t, err, ErrClaudeAIAuthRequired)
				require.EqualError(t, err, "claude.ai sign-in required")
				assert.Equal(t, stage+1, calls)
				assert.Zero(t, stats.Imported+stats.Errors+stats.Skipped)
				session, err := d.GetSession(t.Context(), "claude-ai:22222222-2222-4222-8222-222222222222")
				require.NoError(t, err)
				assert.Nil(t, session)
			})
		}
	}
}

func TestSyncClaudeAI(t *testing.T) {
	t.Run("installation machine", func(t *testing.T) {
		d := testDB(t)
		const machine = "installation-a"
		_, err := d.EnsureInstallationIdentity(t.Context(), machine)
		require.NoError(t, err)
		stats, err := SyncClaudeAI(t.Context(), d, syncOneFetch(t, syncSummary, func() (ClaudeAIResponse, error) {
			return ClaudeAIResponse{Status: 200, Body: []byte(syncDetail)}, nil
		}), nil, machine)
		require.NoError(t, err)
		assert.Equal(t, 1, stats.Imported)
		assert.Zero(t, stats.Errors)
		session, err := d.GetSession(t.Context(), "claude-ai:22222222-2222-4222-8222-222222222222")
		require.NoError(t, err)
		require.NotNil(t, session)
		assert.Equal(t, machine, session.Machine)
	})
	t.Run("trashed during sync write is skipped", func(t *testing.T) {
		d := testDB(t)
		_, err := ImportClaudeAI(t.Context(), d, strings.NewReader("["+strings.Replace(syncDetail, "10:05:00.123456Z", "10:04:00Z", 1)+"]"), nil)
		require.NoError(t, err)
		stats, err := SyncClaudeAI(t.Context(), trashDuringImportStore{d}, syncOneFetch(t, syncSummary, func() (ClaudeAIResponse, error) {
			return ClaudeAIResponse{Status: 200, Body: []byte(syncDetail)}, nil
		}), nil)
		require.NoError(t, err)
		assert.Equal(t, 1, stats.Skipped)
		assert.Zero(t, stats.Errors)
		assert.Empty(t, stats.Refusals)
		assert.True(t, d.IsSessionTrashed(t.Context(), "claude-ai:22222222-2222-4222-8222-222222222222"))
	})
	t.Run("empty page with more fails", func(t *testing.T) {
		_, err := SyncClaudeAI(t.Context(), testDB(t), func(ctx context.Context, path string) (ClaudeAIResponse, error) {
			if path == "/api/organizations" {
				return ClaudeAIResponse{Status: 200, Body: []byte(syncOrgs)}, nil
			}
			require.Equal(t, "/api/organizations/11111111-1111-4111-8111-111111111111/chat_conversations_v2?limit=50&offset=0", path)
			return ClaudeAIResponse{Status: 200, Body: []byte(`{"data":[],"has_more":true}`)}, nil
		}, nil)
		require.EqualError(t, err, "claude list returned an empty page with has_more: true")
	})
	t.Run("trash exclusion and missing detail are skipped", func(t *testing.T) {
		for _, excluded := range []bool{false, true} {
			d := testDB(t)
			require.NoError(t, d.UpsertSession(t.Context(), db.Session{ID: "claude-ai:22222222-2222-4222-8222-222222222222", Agent: "claude-ai", Project: "test", Machine: "test"}))
			require.NoError(t, d.SoftDeleteSession(t.Context(), "claude-ai:22222222-2222-4222-8222-222222222222"))
			if excluded {
				_, err := d.DeleteSessionIfTrashed(t.Context(), "claude-ai:22222222-2222-4222-8222-222222222222")
				require.NoError(t, err)
			}
			stats, err := SyncClaudeAI(t.Context(), d, func(ctx context.Context, path string) (ClaudeAIResponse, error) {
				switch path {
				case "/api/organizations":
					return ClaudeAIResponse{Status: 200, Body: []byte(syncOrgs)}, nil
				case "/api/organizations/11111111-1111-4111-8111-111111111111/chat_conversations_v2?limit=50&offset=0":
					return ClaudeAIResponse{Status: 200, Body: []byte(`{"data":[` + syncSummary + `],"has_more":true}`)}, nil
				case "/api/organizations/11111111-1111-4111-8111-111111111111/chat_conversations_v2?limit=50&offset=1":
					return ClaudeAIResponse{Status: 200, Body: []byte(`{"data":[` + strings.ReplaceAll(syncSummary, "22222222-2222-4222-8222-222222222222", "22222222-2222-4222-8222-222222222223") + `],"has_more":false}`)}, nil
				default:
					require.Equal(t, "/api/organizations/11111111-1111-4111-8111-111111111111/chat_conversations/22222222-2222-4222-8222-222222222223?tree=True&rendering_mode=messages&consistency=strong&render_all_tools=true&include_inline_comparison=true", path)
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
			case "/api/organizations/11111111-1111-4111-8111-111111111111/chat_conversations_v2?limit=50&offset=0":
				return ClaudeAIResponse{Status: 200, Body: []byte(`{"data":[` + syncSummary + `],"has_more":true}`)}, nil
			case "/api/organizations/11111111-1111-4111-8111-111111111111/chat_conversations_v2?limit=50&offset=1":
				return ClaudeAIResponse{Status: 200, Body: []byte(`{"data":[` + syncSummary + `,` + strings.ReplaceAll(strings.Replace(syncSummary, `"is_archived":false`, `"is_archived":true`, 1), "22222222-2222-4222-8222-222222222222", "22222222-2222-4222-8222-222222222223") + `],"has_more":true}`)}, nil
			case "/api/organizations/11111111-1111-4111-8111-111111111111/chat_conversations_v2?limit=50&offset=3":
				return ClaudeAIResponse{Status: 200, Body: []byte(`{"data":[],"has_more":false}`)}, nil
			case "/api/organizations/11111111-1111-4111-8111-111111111111/chat_conversations/22222222-2222-4222-8222-222222222222?tree=True&rendering_mode=messages&consistency=strong&render_all_tools=true&include_inline_comparison=true":
				details++
				return ClaudeAIResponse{Status: 200, Body: []byte(syncDetail)}, nil
			default:
				require.Equal(t, "/api/organizations/11111111-1111-4111-8111-111111111111/chat_conversations/22222222-2222-4222-8222-222222222223?tree=True&rendering_mode=messages&consistency=strong&render_all_tools=true&include_inline_comparison=true", path)
				details++
				return ClaudeAIResponse{Status: 200, Body: []byte(strings.ReplaceAll(syncDetail, "22222222-2222-4222-8222-222222222222", "22222222-2222-4222-8222-222222222223"))}, nil
			}
		}, nil)
		require.NoError(t, err)
		assert.Equal(t, 2, stats.Imported)
		assert.Equal(t, 1, stats.Skipped)
		assert.Equal(t, 2, details)
		messages, err := d.GetAllMessages(t.Context(), "claude-ai:22222222-2222-4222-8222-222222222223")
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
				require.Equal(t, "/api/organizations/11111111-1111-4111-8111-111111111111/chat_conversations_v2?limit=50&offset=0", path)
				return ClaudeAIResponse{Status: 200, Body: []byte(page)}, nil
			}, nil)
			require.EqualError(t, err, "claude list had no data")
		}
	})
	t.Run("retry limit and sign in", func(t *testing.T) {
		for _, status := range []int{429, 503} {
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
					assert.Equal(t, 5, calls)
					assert.Equal(t, 4*time.Minute, time.Since(start))
				})
			})
		}
	})
}

type trashDuringImportStore struct{ *db.DB }

func (s trashDuringImportStore) UpsertSession(ctx context.Context, session db.Session) error {
	if err := s.SoftDeleteSession(ctx, session.ID); err != nil {
		return err
	}
	return s.DB.UpsertSession(ctx, session)
}

func (s trashDuringImportStore) WriteSessionBatchAtomic(ctx context.Context, writes []db.SessionBatchWrite, beforeCommit ...func() error) (db.SessionBatchResult, error) {
	if err := s.SoftDeleteSession(ctx, writes[0].Session.ID); err != nil {
		return db.SessionBatchResult{}, err
	}
	return s.DB.WriteSessionBatchAtomic(ctx, writes, beforeCommit...)
}

func (s trashDuringImportStore) ReplaceSessionKeepingTrashedCopy(ctx context.Context, write db.SessionBatchWrite) (string, error) {
	if err := s.SoftDeleteSession(ctx, write.Session.ID); err != nil {
		return "", err
	}
	return s.DB.ReplaceSessionKeepingTrashedCopy(ctx, write)
}

func TestSyncClaudeAIHistory(t *testing.T) {
	for _, tt := range []struct {
		name, detail string
		failed       bool
		want         []string
	}{
		{name: "requested uuid mismatch", detail: strings.Replace(syncDetail, `"uuid":"22222222-2222-4222-8222-222222222222"`, `"uuid":"22222222-2222-4222-8222-222222222224"`, 1), failed: true},
		{name: "grown history", detail: strings.TrimSuffix(strings.Replace(syncDetail, `"current_leaf_message_uuid":"reply"`, `"current_leaf_message_uuid":"a2"`, 1), "]}") + `,{"uuid":"q2","parent_message_uuid":"reply","sender":"human","text":"","content":[{"type":"text","text":"More"}]},{"uuid":"a2","parent_message_uuid":"q2","sender":"assistant","text":"","content":[{"type":"text","text":"Answer"}]}]}`, want: []string{"Hello", "Chosen reply", "More", "Answer"}},
		{name: "rename refreshes ended at", detail: strings.Replace(syncDetail, `"name":"Chat"`, `"name":"Renamed"`, 1), want: []string{"Hello", "Chosen reply"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDB(t)
			stats, err := SyncClaudeAI(t.Context(), d, syncOneFetch(t, syncSummary, func() (ClaudeAIResponse, error) { return ClaudeAIResponse{Status: 200, Body: []byte(syncDetail)}, nil }), nil)
			require.NoError(t, err)
			require.Equal(t, 1, stats.Imported)
			before, err := d.GetAllMessages(t.Context(), "claude-ai:22222222-2222-4222-8222-222222222222")
			require.NoError(t, err)
			details := 0
			summary := strings.ReplaceAll(syncSummary, "10:05:00.123456Z", "10:06:00Z")
			if tt.name == "grown history" {
				summary = strings.Replace(summary, "reply", "a2", 1)
			}
			fetch := syncOneFetch(t, summary, func() (ClaudeAIResponse, error) {
				details++
				return ClaudeAIResponse{Status: 200, Body: []byte(strings.ReplaceAll(tt.detail, "10:05:00.123456Z", "10:06:00Z"))}, nil
			})
			stats, err = SyncClaudeAI(t.Context(), d, fetch, nil)
			require.NoError(t, err)
			after, err := d.GetAllMessages(t.Context(), "claude-ai:22222222-2222-4222-8222-222222222222")
			require.NoError(t, err)
			assert.Empty(t, replacedCopies(t, d, "claude-ai:22222222-2222-4222-8222-222222222222"))
			if tt.failed {
				assert.Equal(t, 1, stats.Errors)
				assert.Equal(t, before, after)
				session, err := d.GetSession(t.Context(), "claude-ai:22222222-2222-4222-8222-222222222222")
				require.NoError(t, err)
				require.NotNil(t, session)
				require.NotNil(t, session.EndedAt)
				assert.Equal(t, "2026-03-01T10:05:00.123456Z", *session.EndedAt)
				return
			}
			assert.Equal(t, 1, stats.Updated)
			assert.Zero(t, stats.Errors)
			assert.Equal(t, tt.want, messageContents(after))
			if tt.name == "rename refreshes ended at" {
				assert.Equal(t, before, after)
			}
			session, err := d.GetSession(t.Context(), "claude-ai:22222222-2222-4222-8222-222222222222")
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
		case "/api/organizations/11111111-1111-4111-8111-111111111111/chat_conversations_v2?limit=50&offset=0":
			return ClaudeAIResponse{Status: 200, Body: []byte(`{"data":[` + summary + `],"has_more":false}`)}, nil
		default:
			require.Equal(t, "/api/organizations/11111111-1111-4111-8111-111111111111/chat_conversations/22222222-2222-4222-8222-222222222222?tree=True&rendering_mode=messages&consistency=strong&render_all_tools=true&include_inline_comparison=true", path)
			return detail()
		}
	}
}

func TestSyncClaudeAIDetailFailures(t *testing.T) {
	for _, tt := range []struct {
		name      string
		status    int
		err       error
		wantCalls int
	}{
		{name: "bad request", status: 400, wantCalls: 1},
		{name: "forbidden", status: 403, wantCalls: 1},
		{name: "transport", err: errors.New("transport failed"), wantCalls: 1},
		{name: "host cancelled", err: context.Canceled, wantCalls: 1},
		{name: "host deadline", err: context.DeadlineExceeded, wantCalls: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				d := testDB(t)
				calls := 0
				later := 0
				fetch := syncOneFetch(t, syncSummary+","+strings.ReplaceAll(syncSummary, "22222222-2222-4222-8222-222222222222", "22222222-2222-4222-8222-222222222223"), func() (ClaudeAIResponse, error) {
					calls++
					return ClaudeAIResponse{Status: tt.status, RetryAfter: "1"}, tt.err
				})
				stats, err := SyncClaudeAI(t.Context(), d, func(ctx context.Context, path string) (ClaudeAIResponse, error) {
					if strings.Contains(path, "/chat_conversations/22222222-2222-4222-8222-222222222223?") {
						later++
						return ClaudeAIResponse{Status: 200, Body: []byte(strings.ReplaceAll(syncDetail, "22222222-2222-4222-8222-222222222222", "22222222-2222-4222-8222-222222222223"))}, nil
					}
					return fetch(ctx, path)
				}, nil)
				assert.Equal(t, tt.wantCalls, calls)
				require.NoError(t, err)
				assert.Equal(t, 1, later)
				assert.Equal(t, 1, stats.Errors)
				assert.Equal(t, []ImportRefusal{{SessionID: "claude-ai:22222222-2222-4222-8222-222222222222", Reason: RefusalTransient}}, stats.Refusals)
				assert.Equal(t, 1, stats.Imported)
			})
		})
	}
}

type failedSyncStore struct{ *db.DB }

func (s failedSyncStore) UpsertSession(context.Context, db.Session) error {
	return errors.New("write failed")
}

func (s failedSyncStore) WriteSessionBatchAtomic(context.Context, []db.SessionBatchWrite, ...func() error) (db.SessionBatchResult, error) {
	return db.SessionBatchResult{}, errors.New("write failed")
}

func TestSyncClaudeAIDetailProcessingFailureStreak(t *testing.T) {
	for _, tt := range []struct {
		name        string
		details     [3]string
		statuses    [3]int
		failWrite   bool
		unchanged   bool
		wantStop    bool
		wantCalls   int
		wantErrors  int
		wantImport  int
		wantSkipped int
		wantThird   int
		wantError   string
		wantStored  [3]bool
	}{
		{name: "two retry ladders stop sync", statuses: [3]int{503, 503, 200}, wantCalls: 10, wantErrors: 2, wantStop: true, wantError: "claude returned HTTP 503"},
		{name: "success resets failure streak", statuses: [3]int{503, 200, 503}, wantCalls: 11, wantErrors: 2, wantImport: 1, wantThird: 5, wantStored: [3]bool{false, true, false}},
		{name: "two oversized host responses continue", statuses: [3]int{413, 413, 200}, wantCalls: 3, wantErrors: 2, wantImport: 1, wantThird: 1, wantStored: [3]bool{false, false, true}},
		{name: "malformed details stop after second", details: [3]string{"null", "null", "null"}, wantStop: true, wantCalls: 2, wantErrors: 2},
		{name: "valid write resets streak", details: [3]string{"null", "", "null"}, wantCalls: 3, wantErrors: 2, wantImport: 1, wantThird: 1, wantStored: [3]bool{false, true, false}},
		{name: "uuid mismatches stop after second", details: [3]string{syncDetail, syncDetail, syncDetail}, wantStop: true, wantCalls: 2, wantErrors: 2},
		{name: "write failures stop after second", failWrite: true, wantStop: true, wantCalls: 2, wantErrors: 2},
		{name: "unchanged resets streak", details: [3]string{"null", "", "null"}, unchanged: true, wantCalls: 2, wantErrors: 2, wantSkipped: 1, wantThird: 1, wantStored: [3]bool{false, true, false}},
		{name: "404 resets streak", details: [3]string{"null", "", "null"}, statuses: [3]int{200, 404, 200}, wantCalls: 3, wantErrors: 2, wantSkipped: 1, wantThird: 1},
		{name: "oversized resets streak", details: [3]string{"null", "", "null"}, statuses: [3]int{200, 413, 200}, wantCalls: 3, wantErrors: 3, wantThird: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				d := testDB(t)
				ids := []string{"22222222-2222-4222-8222-222222222223", "22222222-2222-4222-8222-222222222224", "22222222-2222-4222-8222-222222222225"}
				if tt.unchanged {
					_, err := SyncClaudeAI(t.Context(), d, func(ctx context.Context, path string) (ClaudeAIResponse, error) {
						if strings.Contains(path, "/chat_conversations/") {
							return ClaudeAIResponse{Status: 200, Body: []byte(strings.ReplaceAll(syncDetail, "22222222-2222-4222-8222-222222222222", ids[1]))}, nil
						}
						return syncOneFetch(t, strings.ReplaceAll(syncSummary, "22222222-2222-4222-8222-222222222222", ids[1]), nil)(ctx, path)
					}, nil)
					require.NoError(t, err)
				}
				var summaries []string
				for _, id := range ids {
					summaries = append(summaries, strings.ReplaceAll(syncSummary, "22222222-2222-4222-8222-222222222222", id))
				}
				calls, third := 0, 0
				var progress ImportStats
				fetch := syncOneFetch(t, strings.Join(summaries, ","), func() (ClaudeAIResponse, error) {
					require.FailNow(t, "detail must use its own response")
					return ClaudeAIResponse{}, nil
				})
				var store interface {
					db.Store
					IsSessionTrashed(context.Context, string) bool
					IsSessionExcluded(context.Context, string) bool
					GetProviderStatHash(context.Context, parser.AgentType, string) (uint64, bool, error)
					UpsertProviderStatHash(context.Context, parser.AgentType, string, uint64) error
				} = d
				if tt.failWrite {
					store = failedSyncStore{d}
				}
				stats, err := SyncClaudeAI(t.Context(), store, func(ctx context.Context, path string) (ClaudeAIResponse, error) {
					for i, id := range ids {
						if strings.Contains(path, "/chat_conversations/"+id+"?") {
							calls++
							if i == 2 {
								third++
							}
							status := tt.statuses[i]
							if status == 0 {
								status = 200
							}
							detail := tt.details[i]
							if detail == "" {
								detail = strings.ReplaceAll(syncDetail, "22222222-2222-4222-8222-222222222222", id)
							}
							return ClaudeAIResponse{Status: status, Body: []byte(detail), RetryAfter: "1"}, nil
						}
					}
					return fetch(ctx, path)
				}, &ImportCallbacks{OnProgress: func(stats ImportStats) { progress = stats }})
				if tt.wantStop {
					require.Error(t, err)
					if tt.wantError != "" {
						require.EqualError(t, err, tt.wantError)
					}
				} else {
					require.NoError(t, err)
				}
				assert.Equal(t, tt.wantCalls, calls)
				assert.Len(t, stats.Refusals, tt.wantErrors)
				assert.Equal(t, tt.wantErrors, progress.Errors)
				assert.Equal(t, tt.wantThird, third)
				assert.Equal(t, tt.wantErrors, stats.Errors)
				assert.Equal(t, tt.wantImport, stats.Imported)
				assert.Equal(t, tt.wantSkipped, stats.Skipped)
				for i, id := range ids {
					messages, err := d.GetAllMessages(t.Context(), "claude-ai:"+id)
					require.NoError(t, err)
					if tt.wantStored[i] {
						assert.Equal(t, []string{"Hello", "Chosen reply"}, messageContents(messages))
					} else {
						assert.Empty(t, messages)
					}
				}
			})
		})
	}
}

func TestSyncClaudeAIMalformedRetries(t *testing.T) {
	for _, detail := range []string{"null", `{"uuid":"22222222-2222-4222-8222-222222222222"}`, strings.ReplaceAll(syncDetail, "10:05:00.123456Z", "bad")} {
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
			session, err := d.GetSession(t.Context(), "claude-ai:22222222-2222-4222-8222-222222222222")
			require.NoError(t, err)
			assert.Nil(t, session)
		}
		assert.Equal(t, 2, calls)
	}
}

func TestSyncClaudeAIDetailNewerThanListNotCached(t *testing.T) {
	d := testDB(t)
	changed := regexp.MustCompile(`"updated_at":\s*"[^"]*"`).ReplaceAllString(syncDetail, `"updated_at":"2099-01-01T00:00:00Z"`)
	calls := 0
	fetch := syncOneFetch(t, syncSummary, func() (ClaudeAIResponse, error) {
		calls++
		return ClaudeAIResponse{Status: 200, Body: []byte(changed)}, nil
	})
	_, err := SyncClaudeAI(t.Context(), d, fetch, nil)
	require.NoError(t, err)
	_, err = SyncClaudeAI(t.Context(), d, fetch, nil)
	require.NoError(t, err)
	assert.Equal(t, 2, calls)
}

func TestSyncClaudeAIUsageArchiveShorterBranchReplacesCounts(t *testing.T) {
	const id = "claude-ai:22222222-2222-4222-8222-222222222222"
	d, err := db.OpenWithArchiveContent(t.Context(), filepath.Join(t.TempDir(), "archive.db"), config.ArchiveContentUsage)
	require.NoError(t, err)
	t.Cleanup(func() { d.Close() })
	selected := "q2"
	details := 0
	fetch := func(ctx context.Context, path string) (ClaudeAIResponse, error) {
		detail := strings.Replace(strings.TrimSuffix(syncDetail, "]}")+`,{"uuid":"q2","parent_message_uuid":"reply","sender":"human","text":"More"}]}`, `"current_leaf_message_uuid":"reply"`, `"current_leaf_message_uuid":"`+selected+`"`, 1)
		summary := strings.Replace(syncSummary, "reply", selected, 1)
		if selected == "reply" {
			detail = strings.Replace(detail, "2026-03-01T10:05:00.123456Z", "2026-03-02T10:05:00Z", 1)
			summary = strings.Replace(summary, "2026-03-01T10:05:00.123456Z", "2026-03-02T10:05:00Z", 1)
		}
		return syncOneFetch(t, summary, func() (ClaudeAIResponse, error) {
			details++
			return ClaudeAIResponse{Status: 200, Body: []byte(detail)}, nil
		})(ctx, path)
	}
	_, err = SyncClaudeAI(t.Context(), d, fetch, nil)
	require.NoError(t, err)
	selected = "reply"
	stats, err := SyncClaudeAI(t.Context(), d, fetch, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Updated)
	assert.Zero(t, stats.Errors)
	assert.Empty(t, stats.Refusals)
	session, err := d.GetSessionFull(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, 2, session.MessageCount)
	assert.Equal(t, 1, session.UserMessageCount)
	assert.Equal(t, strPtr("2026-03-02T10:05:00Z"), session.EndedAt)
	stats, err = SyncClaudeAI(t.Context(), d, fetch, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Skipped)
	assert.Zero(t, stats.Errors)
	assert.Equal(t, 2, details)
	session, err = d.GetSessionFull(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, 2, session.MessageCount)
}

type cancelNewSyncStore struct {
	*db.DB
	cancel context.CancelFunc
}

func (s cancelNewSyncStore) UpsertSession(ctx context.Context, session db.Session) error {
	err := s.DB.UpsertSession(ctx, session)
	s.cancel()
	return err
}

func (s cancelNewSyncStore) WriteSessionBatchAtomic(ctx context.Context, writes []db.SessionBatchWrite, _ ...func() error) (db.SessionBatchResult, error) {
	return s.DB.WriteSessionBatchAtomic(ctx, writes, func() error {
		s.cancel()
		return ctx.Err()
	})
}

func TestSyncClaudeAINewChatCancellationIsAtomic(t *testing.T) {
	d := testDB(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stats, err := SyncClaudeAI(ctx, cancelNewSyncStore{d, cancel}, syncOneFetch(t, syncSummary, func() (ClaudeAIResponse, error) {
		return ClaudeAIResponse{Status: 200, Body: []byte(syncDetail)}, nil
	}), nil)
	require.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, stats.Imported+stats.Updated)
	session, err := d.GetSession(t.Context(), "claude-ai:22222222-2222-4222-8222-222222222222")
	require.NoError(t, err)
	assert.Nil(t, session)
	messages, err := d.GetAllMessages(t.Context(), "claude-ai:22222222-2222-4222-8222-222222222222")
	require.NoError(t, err)
	assert.Empty(t, messages)
}

type failSyncFreshnessStore struct {
	*db.DB
	cancel context.CancelFunc
}

func (s failSyncFreshnessStore) UpsertProviderStatHash(ctx context.Context, _ parser.AgentType, _ string, _ uint64) error {
	if s.cancel != nil {
		s.cancel()
		return ctx.Err()
	}
	return errors.New("freshness write failed")
}

func TestSyncClaudeAICommittedContentCountsOnFreshnessFailure(t *testing.T) {
	for _, updated := range []bool{false, true} {
		for _, canceled := range []bool{false, true} {
			t.Run("updated="+strconv.FormatBool(updated)+"/canceled="+strconv.FormatBool(canceled), func(t *testing.T) {
				d := testDB(t)
				if updated {
					seed := strings.Replace(syncDetail, "Chosen reply", "Old reply", 1)
					_, err := ImportClaudeAI(t.Context(), d, strings.NewReader("["+seed+"]"), nil)
					require.NoError(t, err)
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				store := failSyncFreshnessStore{DB: d}
				if canceled {
					store.cancel = cancel
				}
				var progress ImportStats
				stats, err := SyncClaudeAI(ctx, store, syncOneFetch(t, syncSummary, func() (ClaudeAIResponse, error) {
					return ClaudeAIResponse{Status: 200, Body: []byte(syncDetail)}, nil
				}), &ImportCallbacks{OnProgress: func(stats ImportStats) { progress = stats }})
				if canceled {
					require.ErrorIs(t, err, context.Canceled)
				} else {
					require.NoError(t, err)
				}
				if updated {
					assert.Equal(t, 1, stats.Updated)
					assert.Zero(t, stats.Imported)
				} else {
					assert.Equal(t, 1, stats.Imported)
					assert.Zero(t, stats.Updated)
				}
				assert.Equal(t, 1, stats.Errors)
				assert.Equal(t, stats.Imported, progress.Imported)
				assert.Equal(t, stats.Updated, progress.Updated)
				assert.Equal(t, 1, progress.Errors)
				messages, err := d.GetAllMessages(t.Context(), "claude-ai:22222222-2222-4222-8222-222222222222")
				require.NoError(t, err)
				assert.Equal(t, []string{"Hello", "Chosen reply"}, messageContents(messages))
			})
		}
	}
}

func TestSyncClaudeAIZipFreshness(t *testing.T) {
	for _, tt := range []struct{ name, exported string }{
		{"zip changes stored count", strings.TrimSuffix(syncDetail, "]}") + `,{"sender":"human","text":"Other branch"}]}`},
		{"zip changes content at same count", strings.Replace(syncDetail, "Chosen reply", "Zip reply", 1)},
		{"zip changes title", strings.Replace(syncDetail, `"name":"Chat"`, `"name":"Old title"`, 1)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDB(t)
			calls := 0
			fetch := syncOneFetch(t, syncSummary, func() (ClaudeAIResponse, error) {
				calls++
				return ClaudeAIResponse{Status: 200, Body: []byte(syncDetail)}, nil
			})
			_, err := SyncClaudeAI(t.Context(), d, fetch, nil)
			require.NoError(t, err)
			stats, err := ImportClaudeAI(t.Context(), d, strings.NewReader("["+tt.exported+"]"), nil)
			require.NoError(t, err)
			if tt.name != "zip changes title" {
				require.Equal(t, 1, stats.Updated)
			}
			if tt.name == "zip changes content at same count" {
				messages, err := d.GetAllMessages(t.Context(), "claude-ai:22222222-2222-4222-8222-222222222222")
				require.NoError(t, err)
				require.Equal(t, []string{"Hello", "Zip reply"}, messageContents(messages))
			}
			stats, err = SyncClaudeAI(t.Context(), d, fetch, nil)
			require.NoError(t, err)
			if tt.name == "zip changes title" {
				assert.Equal(t, 2, calls)
				session, err := d.GetSessionFull(t.Context(), "claude-ai:22222222-2222-4222-8222-222222222222")
				require.NoError(t, err)
				require.NotNil(t, session.SessionName)
				assert.Equal(t, "Chat", *session.SessionName)
				return
			}
			assert.Equal(t, 1, stats.Updated)
			stats, err = SyncClaudeAI(t.Context(), d, fetch, nil)
			require.NoError(t, err)
			assert.Equal(t, 1, stats.Skipped)
			assert.Equal(t, 2, calls)
			messages, err := d.GetAllMessages(t.Context(), "claude-ai:22222222-2222-4222-8222-222222222222")
			require.NoError(t, err)
			assert.Equal(t, []string{"Hello", "Chosen reply"}, messageContents(messages))
		})
	}
	const shorter = `{"uuid":"22222222-2222-4222-8222-222222222222","current_leaf_message_uuid":"reply","created_at":"2026-03-01T10:00:00Z","updated_at":"2026-03-01T10:05:00.123456Z","chat_messages":[{"uuid":"reply","parent_message_uuid":"00000000-0000-4000-8000-000000000000","sender":"human","text":"Archived relay message","created_at":"2026-03-01T10:00:00Z"}]}`
	for _, tt := range []struct {
		name, seed, detail string
		updated            bool
		want               []string
	}{
		{"same updated_at", syncDetail, syncDetail, false, nil},
		{"older updated_at", strings.ReplaceAll(syncDetail, "10:05:00.123456Z", "10:04:00Z"), syncDetail, true, []string{"Hello", "Chosen reply"}},
		{"shorter result updates in place", strings.ReplaceAll(syncDetail, "10:05:00.123456Z", "10:04:00Z"), shorter, true, []string{"Archived relay message"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDB(t)
			stats, err := ImportClaudeAI(t.Context(), d, strings.NewReader("["+tt.seed+"]"), nil)
			require.NoError(t, err)
			require.Equal(t, 1, stats.Imported)
			require.NoError(t, d.ReplaceSessionSecretFindings(t.Context(), "claude-ai:22222222-2222-4222-8222-222222222222", []db.SecretFinding{{RuleName: "test-secret", Confidence: "high", LocationKind: "message", MessageOrdinal: 0, MatchStart: 0, MatchEnd: 5, RedactedMatch: "[redacted]"}}, 1, "test-rules"))
			findings, err := d.SessionSecretFindings(t.Context(), "claude-ai:22222222-2222-4222-8222-222222222222")
			require.NoError(t, err)
			require.Len(t, findings, 1)
			before, err := d.GetAllMessages(t.Context(), "claude-ai:22222222-2222-4222-8222-222222222222")
			require.NoError(t, err)
			initial, err := d.GetSessionFull(t.Context(), "claude-ai:22222222-2222-4222-8222-222222222222")
			require.NoError(t, err)
			require.NotNil(t, initial)
			require.NotNil(t, initial.TranscriptRevision)
			calls := 0
			fetch := syncOneFetch(t, syncSummary, func() (ClaudeAIResponse, error) {
				calls++
				return ClaudeAIResponse{Status: 200, Body: []byte(tt.detail)}, nil
			})
			for run := range 2 {
				stats, err = SyncClaudeAI(t.Context(), d, fetch, nil)
				require.NoError(t, err)
				if run == 0 && tt.updated {
					assert.Equal(t, 1, stats.Updated)
				} else {
					assert.Equal(t, 1, stats.Skipped)
					assert.Zero(t, stats.Updated)
				}
			}
			assert.Equal(t, 1, calls)
			after, err := d.GetAllMessages(t.Context(), "claude-ai:22222222-2222-4222-8222-222222222222")
			require.NoError(t, err)
			if !tt.updated {
				for _, row := range after {
					assert.Empty(t, row.SourceUUID)
				}
				assert.Equal(t, before, after)
				afterFindings, err := d.SessionSecretFindings(t.Context(), "claude-ai:22222222-2222-4222-8222-222222222222")
				require.NoError(t, err)
				require.Len(t, afterFindings, 1)
				assert.Equal(t, findings, afterFindings)
			} else {
				for _, row := range after {
					assert.Empty(t, row.SourceUUID)
				}
				assert.Equal(t, tt.want, messageContents(after))
			}
			assert.Empty(t, replacedCopies(t, d, "claude-ai:22222222-2222-4222-8222-222222222222"))
			marked, err := d.GetSessionFull(t.Context(), "claude-ai:22222222-2222-4222-8222-222222222222")
			require.NoError(t, err)
			require.NotNil(t, marked)
			require.NotNil(t, marked.TranscriptRevision)
			if !tt.updated {
				assert.Equal(t, *initial.TranscriptRevision, *marked.TranscriptRevision)
			}
			_, err = ImportClaudeAI(t.Context(), d, strings.NewReader("["+tt.detail+"]"), nil)
			require.NoError(t, err)
			session, err := d.GetSessionFull(t.Context(), "claude-ai:22222222-2222-4222-8222-222222222222")
			require.NoError(t, err)
			require.NotNil(t, session)
			assert.Nil(t, session.LastEntryUUID)
			for range 2 {
				stats, err = SyncClaudeAI(t.Context(), d, fetch, nil)
				require.NoError(t, err)
				assert.Equal(t, 1, stats.Skipped)
			}
			assert.Equal(t, 1, calls)
			require.NotNil(t, session.EndedAt)
			assert.Equal(t, "2026-03-01T10:05:00.123456Z", *session.EndedAt)
		})
	}
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
			before, err := d.GetAllMessages(t.Context(), "claude-ai:22222222-2222-4222-8222-222222222222")
			require.NoError(t, err)
			require.Len(t, before, 4)
			for _, message := range []db.Message{before[1], before[3]} {
				_, err := d.PinMessage(t.Context(), "claude-ai:22222222-2222-4222-8222-222222222222", message.ID, nil)
				require.NoError(t, err)
			}
			for flip := range 2 {
				selected = leaf
				if flip%2 == 1 {
					selected = "a2"
				}
				stats, err = SyncClaudeAI(t.Context(), d, fetch, nil)
				require.NoError(t, err)
				assert.Equal(t, 1, stats.Updated)
				assert.Zero(t, stats.Errors)
				assert.Empty(t, stats.Refusals)
				messages, err := d.GetAllMessages(t.Context(), "claude-ai:22222222-2222-4222-8222-222222222222")
				require.NoError(t, err)
				want := []string{"Hello", "Chosen reply"}
				switch selected {
				case "a2":
					want = append(want, "More", "Answer")
				case "b2":
					want = append(want, "Edited question", "Edited answer")
				}
				assert.Equal(t, want, messageContents(messages))
				for _, row := range messages {
					assert.Empty(t, row.SourceUUID)
				}
			}
			copies := replacedCopies(t, d, "claude-ai:22222222-2222-4222-8222-222222222222")
			require.Len(t, copies, 1)
		})
	}
}

func TestSyncClaudeAIInvalidListLeaf(t *testing.T) {
	for _, tt := range []struct {
		name, field string
		skipped     int
		existing    bool
	}{
		{"absent", "", 0, false},
		{"empty", `"current_leaf_message_uuid":"",`, 0, false},
		{"number", `"current_leaf_message_uuid":42,`, 0, false},
		{"null without archive", `"current_leaf_message_uuid":null,`, 1, false},
		{"null with archive", `"current_leaf_message_uuid":null,`, 1, true},
		{"root without archive", `"current_leaf_message_uuid":"00000000-0000-4000-8000-000000000000",`, 1, false},
		{"root with archive", `"current_leaf_message_uuid":"00000000-0000-4000-8000-000000000000",`, 1, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDB(t)
			const id = "claude-ai:22222222-2222-4222-8222-222222222222"
			if tt.existing {
				_, err := ImportClaudeAI(t.Context(), d, strings.NewReader("["+syncDetail+"]"), nil)
				require.NoError(t, err)
			}
			before, err := d.GetAllMessages(t.Context(), id)
			require.NoError(t, err)
			summary := strings.Replace(syncSummary, `"current_leaf_message_uuid":"reply",`, tt.field, 1)
			fetch := syncOneFetch(t, summary, func() (ClaudeAIResponse, error) {
				require.FailNow(t, "invalid list leaf fetched its detail")
				return ClaudeAIResponse{}, nil
			})
			for range 2 {
				stats, err := SyncClaudeAI(t.Context(), d, fetch, nil)
				require.NoError(t, err)
				assert.Equal(t, tt.skipped, stats.Skipped)
				assert.Equal(t, 1-tt.skipped, stats.Errors)
				assert.Zero(t, stats.Imported+stats.Updated)
			}
			after, err := d.GetAllMessages(t.Context(), id)
			require.NoError(t, err)
			assert.Equal(t, before, after)
			assert.Empty(t, replacedCopies(t, d, id))
			session, err := d.GetSession(t.Context(), id)
			require.NoError(t, err)
			if tt.existing {
				require.NotNil(t, session)
				require.NotNil(t, session.EndedAt)
				assert.Equal(t, "2026-03-01T10:05:00.123456Z", *session.EndedAt)
			} else {
				assert.Nil(t, session)
			}
		})
	}
}

func TestSyncClaudeAIConsecutiveInvalidSummariesStop(t *testing.T) {
	for _, summary := range []string{
		`{"uuid":42}`,
	} {
		t.Run(summary, func(t *testing.T) {
			details := 0
			stats, err := SyncClaudeAI(t.Context(), testDB(t), syncOneFetch(t, summary+","+summary+","+syncSummary, func() (ClaudeAIResponse, error) {
				details++
				return ClaudeAIResponse{Status: 200, Body: []byte(syncDetail)}, nil
			}), nil)
			require.ErrorContains(t, err, "invalid claude conversation")
			assert.Equal(t, 2, stats.Errors)
			assert.Zero(t, details)
		})
	}
}

type unchangedSyncReplacementStore struct{ *db.DB }

func (s unchangedSyncReplacementStore) ReplaceSessionKeepingTrashedCopy(ctx context.Context, write db.SessionBatchWrite) (string, error) {
	write.ReplaceMessages = true
	_, err := s.WriteSessionBatchAtomic(ctx, []db.SessionBatchWrite{write})
	if err != nil {
		return "", err
	}
	return s.DB.ReplaceSessionKeepingTrashedCopy(ctx, write)
}

func TestSyncClaudeAIConcurrentUnchangedReplacementIsSkipped(t *testing.T) {
	d := testDB(t)
	_, err := ImportClaudeAI(t.Context(), d, strings.NewReader("["+strings.Replace(syncDetail, "Chosen reply", "Previous reply", 1)+"]"), nil)
	require.NoError(t, err)
	details := 0
	fetch := syncOneFetch(t, syncSummary, func() (ClaudeAIResponse, error) {
		details++
		return ClaudeAIResponse{Status: 200, Body: []byte(syncDetail)}, nil
	})
	for range 2 {
		stats, err := SyncClaudeAI(t.Context(), unchangedSyncReplacementStore{d}, fetch, nil)
		require.NoError(t, err)
		assert.Equal(t, 1, stats.Skipped)
		assert.Zero(t, stats.Errors)
		assert.Empty(t, stats.Refusals)
	}
	assert.Equal(t, 1, details)
	assert.Empty(t, replacedCopies(t, d, "claude-ai:22222222-2222-4222-8222-222222222222"))
}

func TestSyncClaudeAIInvalidOrganizations(t *testing.T) {
	for _, body := range []string{"null", "{}"} {
		t.Run(body, func(t *testing.T) {
			d := testDB(t)
			calls := 0
			stats, err := SyncClaudeAI(t.Context(), d, func(ctx context.Context, path string) (ClaudeAIResponse, error) {
				calls++
				require.Equal(t, "/api/organizations", path)
				return ClaudeAIResponse{Status: 200, Body: []byte(body)}, nil
			}, nil)
			require.Error(t, err)
			if body == "null" {
				require.EqualError(t, err, "claude organizations must be an array")
			}
			assert.Equal(t, 1, calls)
			assert.Zero(t, stats.Imported+stats.Updated+stats.Skipped+stats.Errors)
			session, err := d.GetSession(t.Context(), "claude-ai:22222222-2222-4222-8222-222222222222")
			require.NoError(t, err)
			assert.Nil(t, session)
		})
	}
}
