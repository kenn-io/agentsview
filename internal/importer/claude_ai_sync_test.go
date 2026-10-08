package importer

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json/jsontext"
	"encoding/json/v2"
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

var syncSummary = func() string {
	var page struct {
		Data []jsontext.Value `json:"data"`
	}
	if err := json.Unmarshal([]byte(syncActiveList), &page); err != nil {
		panic(err)
	}
	return string(page.Data[0])
}()

//go:embed testdata/claude_ai_live/detail.json
var syncDetail string

//go:embed testdata/claude_ai_live/organizations.json
var syncOrgs string

//go:embed testdata/claude_ai_live/list_active.json
var syncActiveList string

//go:embed testdata/claude_ai_live/list_archived.json
var syncArchivedList string

//go:embed testdata/claude_ai_live/list_all.json
var syncAllList string

func TestSyncClaudeAIArchivePolicySwitch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.db")
	d, err := db.OpenWithArchiveContent(t.Context(), path, config.ArchiveContentUsage)
	require.NoError(t, err)
	t.Cleanup(func() { d.Close() })
	details := 0
	fetch := syncOneFetch(t, syncSummary, func() (ClaudeAIResponse, error) {
		details++
		return ClaudeAIResponse{Status: 200, Body: []byte(syncDetail)}, nil
	})
	for range 2 {
		_, err = SyncClaudeAI(t.Context(), d, fetch, nil)
		require.NoError(t, err)
	}
	assert.Equal(t, 1, details)
	session, err := d.GetSessionFull(t.Context(), "claude-ai:22222222-2222-4222-8222-222222222222")
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, strPtr("claude-ai:v1:usage:reply"), session.LastEntryUUID)
	before, err := d.GetAllMessages(t.Context(), "claude-ai:22222222-2222-4222-8222-222222222222")
	require.NoError(t, err)
	require.Len(t, before, 1)
	assert.Empty(t, before[0].Content)
	require.NoError(t, d.Close())
	d, err = db.OpenWithArchiveContent(t.Context(), path, config.ArchiveContentFull)
	require.NoError(t, err)
	for range 2 {
		_, err = SyncClaudeAI(t.Context(), d, fetch, nil)
		require.NoError(t, err)
	}
	assert.Equal(t, 2, details)
	after, err := d.GetAllMessages(t.Context(), "claude-ai:22222222-2222-4222-8222-222222222222")
	require.NoError(t, err)
	assert.Equal(t, []string{"Hello", "Chosen reply"}, messageContents(after))
	session, err = d.GetSessionFull(t.Context(), "claude-ai:22222222-2222-4222-8222-222222222222")
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, strPtr("claude-ai:v1:full:reply"), session.LastEntryUUID)
}

func TestSyncClaudeAIZipPreservesPromptPin(t *testing.T) {
	d := testDB(t)
	_, err := SyncClaudeAI(t.Context(), d, syncOneFetch(t, syncSummary, func() (ClaudeAIResponse, error) {
		return ClaudeAIResponse{Status: 200, Body: []byte(syncDetail)}, nil
	}), nil)
	require.NoError(t, err)
	before, err := d.GetAllMessages(t.Context(), "claude-ai:22222222-2222-4222-8222-222222222222")
	require.NoError(t, err)
	require.Len(t, before, 2)
	_, err = d.PinMessage(t.Context(), "claude-ai:22222222-2222-4222-8222-222222222222", before[0].ID, strPtr("Keep this prompt"))
	require.NoError(t, err)
	zipPath := createTestZip(t, map[string]string{"conversations.json": "[" + strings.Replace(syncDetail, "Chosen reply", "Export reply", 1) + "]"})
	dir, cleanup, err := ExtractZip(zipPath)
	require.NoError(t, err)
	defer cleanup()
	reader, err := os.Open(filepath.Join(dir, "conversations.json"))
	require.NoError(t, err)
	defer reader.Close()
	stats, err := ImportClaudeAI(t.Context(), d, reader, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Updated)
	after, err := d.GetAllMessages(t.Context(), "claude-ai:22222222-2222-4222-8222-222222222222")
	require.NoError(t, err)
	require.Len(t, after, 2)
	assert.Equal(t, []string{"Hello", "Export reply"}, messageContents(after))
	assert.Empty(t, after[0].SourceUUID)
	pins, err := d.ListPinnedMessages(t.Context(), "claude-ai:22222222-2222-4222-8222-222222222222", "")
	require.NoError(t, err)
	require.Len(t, pins, 1)
	assert.Equal(t, after[0].ID, pins[0].MessageID)
	assert.Equal(t, strPtr("Keep this prompt"), pins[0].Note)
}

func TestSyncClaudeAIZipRecallAnchors(t *testing.T) {
	d := testDB(t)
	const id = "claude-ai:22222222-2222-4222-8222-222222222222"
	zipPath := createTestZip(t, map[string]string{"conversations.json": "[" + syncDetail + "]"})
	dir, cleanup, err := ExtractZip(zipPath)
	require.NoError(t, err)
	defer cleanup()
	reader, err := os.Open(filepath.Join(dir, "conversations.json"))
	require.NoError(t, err)
	defer reader.Close()
	_, err = ImportClaudeAI(t.Context(), d, reader, nil)
	require.NoError(t, err)
	window, err := d.BuildRecallEvidenceWindow(t.Context(), id, 0, 1)
	require.NoError(t, err)
	metadata, err := window.BindSelection(db.RecallEvidenceSelection{MessageStartOrdinal: 0, MessageEndOrdinal: 1})
	require.NoError(t, err)
	assert.Empty(t, metadata.MessageStartSourceUUID)
	_, err = d.InsertRecallEntry(t.Context(), db.RecallEntry{
		ID: "zip-evidence", Type: "fact", Scope: "project", Status: "accepted",
		Title: "Original turn", Body: "Evidence from the exported branch", SourceSessionID: id,
		ProvenanceOK: true, Transferable: true,
		Evidence: []db.RecallEvidence{{SessionID: id, MessageStartOrdinal: 0, MessageEndOrdinal: 1, ContentDigest: metadata.ContentDigest}},
	})
	require.NoError(t, err)
	_, err = SyncClaudeAI(t.Context(), d, syncOneFetch(t, syncSummary, func() (ClaudeAIResponse, error) {
		return ClaudeAIResponse{Status: 200, Body: []byte(syncDetail)}, nil
	}), nil)
	require.NoError(t, err)
	_, err = SyncClaudeAI(t.Context(), d, syncOneFetch(t, strings.Replace(syncSummary, "reply", "other-reply", 1), func() (ClaudeAIResponse, error) {
		return ClaudeAIResponse{Status: 200, Body: []byte(strings.ReplaceAll(syncDetail, "reply", "other-reply"))}, nil
	}), nil)
	require.NoError(t, err)
	entry, err := d.GetRecallEntry(t.Context(), "zip-evidence")
	require.NoError(t, err)
	require.NotNil(t, entry)
	assert.False(t, entry.ProvenanceOK)
}

func TestSyncClaudeAIKeepsTrashOnlyWhenPinsLost(t *testing.T) {
	d := testDB(t)
	const id = "claude-ai:22222222-2222-4222-8222-222222222222"
	branchA := strings.TrimSuffix(strings.Replace(syncDetail, `"current_leaf_message_uuid":"reply"`, `"current_leaf_message_uuid":"a2"`, 1), "]}") + `,{"uuid":"q2","parent_message_uuid":"reply","sender":"human","text":"More"},{"uuid":"a2","parent_message_uuid":"q2","sender":"assistant","text":"Answer"}]}`
	branchB := strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(branchA, "root", "edited-root"), "reply", "edited-reply"), "q2", "edited-q2"), "a2", "b2")
	branchB = strings.Replace(branchB, "Hello", "Edited prompt", 1)
	syncBranch := func(detail, leaf string) {
		t.Helper()
		stats, err := SyncClaudeAI(t.Context(), d, syncOneFetch(t, strings.Replace(syncSummary, "reply", leaf, 1), func() (ClaudeAIResponse, error) {
			return ClaudeAIResponse{Status: 200, Body: []byte(detail)}, nil
		}), nil)
		require.NoError(t, err)
		require.Zero(t, stats.Errors)
	}
	syncBranch(branchA, "a2")
	before, err := d.GetAllMessages(t.Context(), id)
	require.NoError(t, err)
	require.Len(t, before, 4)
	_, err = d.PinMessage(t.Context(), id, before[3].ID, strPtr("Keep turn four"))
	require.NoError(t, err)
	syncBranch(branchB, "b2")
	trash, err := d.ListTrashedSessions(t.Context())
	require.NoError(t, err)
	require.Len(t, trash, 1)
	copyID := trash[0].ID
	old, err := d.GetAllMessages(t.Context(), copyID)
	require.NoError(t, err)
	assert.Equal(t, messageContents(before), messageContents(old))
	pins, err := d.ListPinnedMessages(t.Context(), copyID, "")
	require.NoError(t, err)
	require.Len(t, pins, 1)
	assert.Equal(t, 3, pins[0].Ordinal)
	assert.Equal(t, strPtr("Keep turn four"), pins[0].Note)
	for i := range 5 {
		if i%2 == 0 {
			syncBranch(branchA, "a2")
		} else {
			syncBranch(branchB, "b2")
		}
	}
	trash, err = d.ListTrashedSessions(t.Context())
	require.NoError(t, err)
	require.Len(t, trash, 1)
	assert.Equal(t, copyID, trash[0].ID)
	live, err := d.GetAllMessages(t.Context(), id)
	require.NoError(t, err)
	pins, err = d.ListPinnedMessages(t.Context(), id, "")
	require.NoError(t, err)
	assert.Empty(t, pins)
	_, err = d.PinMessage(t.Context(), id, live[3].ID, strPtr("A new note"))
	require.NoError(t, err)
	syncBranch(branchB, "b2")
	trash, err = d.ListTrashedSessions(t.Context())
	require.NoError(t, err)
	require.Len(t, trash, 2)
	assert.NotEqual(t, trash[0].ID, trash[1].ID)
	for _, copy := range trash {
		if copy.ID != copyID {
			pins, err = d.ListPinnedMessages(t.Context(), copy.ID, "")
			require.NoError(t, err)
			require.Len(t, pins, 1)
			assert.Equal(t, strPtr("A new note"), pins[0].Note)
		}
	}
}

func TestSyncClaudeAIInvalidAccountSession(t *testing.T) {
	body, err := os.ReadFile("testdata/claude_ai_live/signed_out.json")
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
				assert.EqualError(t, err, "claude.ai sign-in required")
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
		messages, err := d.GetAllMessages(t.Context(), "claude-ai:22222222-2222-4222-8222-222222222222")
		require.NoError(t, err)
		require.Len(t, messages, 2)
		assert.Equal(t, "Chosen reply", messages[1].Content)
		hits, err := d.SearchSession(t.Context(), "claude-ai:22222222-2222-4222-8222-222222222222", "Chosen")
		require.NoError(t, err)
		assert.Equal(t, []int{1}, hits)
		stats, err = SyncClaudeAI(t.Context(), d, fetch, nil)
		require.NoError(t, err)
		assert.Equal(t, 1, stats.Skipped)
		assert.Equal(t, 1, details)
		zipPath := createTestZip(t, map[string]string{"conversations.json": "[" + strings.ReplaceAll(syncDetail, "10:05:00.123456Z", "10:04:00Z") + "]"})
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
		session, err := d.GetSession(t.Context(), "claude-ai:22222222-2222-4222-8222-222222222222")
		require.NoError(t, err)
		assert.Equal(t, "2026-03-01T10:05:00.123456Z", *session.EndedAt)
	})
	t.Run("malformed details allow later chats", func(t *testing.T) {
		for _, tt := range []struct {
			name   string
			detail string
			errors int
		}{
			{name: "null", detail: "null", errors: 1},
			{name: "bad timestamp", detail: strings.ReplaceAll(syncDetail, "10:05:00.123456Z", "bad"), errors: 1},
		} {
			t.Run(tt.name, func(t *testing.T) {
				d := testDB(t)
				stats, err := SyncClaudeAI(t.Context(), d, func(ctx context.Context, path string) (ClaudeAIResponse, error) {
					switch path {
					case "/api/organizations":
						return ClaudeAIResponse{Status: 200, Body: []byte(syncOrgs)}, nil
					case "/api/organizations/11111111-1111-4111-8111-111111111111/chat_conversations_v2?limit=50&offset=0":
						return ClaudeAIResponse{Status: 200, Body: []byte(`{"data":[` + syncSummary + `,` + strings.ReplaceAll(syncSummary, "22222222-2222-4222-8222-222222222222", "22222222-2222-4222-8222-222222222223") + `],"has_more":false}`)}, nil
					case "/api/organizations/11111111-1111-4111-8111-111111111111/chat_conversations/22222222-2222-4222-8222-222222222222?tree=True&rendering_mode=messages&consistency=strong&render_all_tools=true&include_inline_comparison=true":
						return ClaudeAIResponse{Status: 200, Body: []byte(tt.detail)}, nil
					default:
						require.Equal(t, "/api/organizations/11111111-1111-4111-8111-111111111111/chat_conversations/22222222-2222-4222-8222-222222222223?tree=True&rendering_mode=messages&consistency=strong&render_all_tools=true&include_inline_comparison=true", path)
						return ClaudeAIResponse{Status: 200, Body: []byte(strings.ReplaceAll(syncDetail, "22222222-2222-4222-8222-222222222222", "22222222-2222-4222-8222-222222222223"))}, nil
					}
				}, nil)
				require.NoError(t, err)
				assert.Equal(t, 1, stats.Imported)
				assert.Zero(t, stats.Skipped)
				assert.Equal(t, tt.errors, stats.Errors)
				session, err := d.GetSession(t.Context(), "claude-ai:22222222-2222-4222-8222-222222222222")
				require.NoError(t, err)
				assert.Nil(t, session)
				messages, err := d.GetAllMessages(t.Context(), "claude-ai:22222222-2222-4222-8222-222222222223")
				require.NoError(t, err)
				require.Len(t, messages, 2)
				assert.Equal(t, "Chosen reply", messages[1].Content)
			})
		}
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
						if status == 401 {
							require.ErrorIs(t, err, ErrClaudeAIAuthRequired)
							require.EqualError(t, err, "claude.ai sign-in required")
						} else {
							require.ErrorIs(t, err, ErrClaudeAIAccessDenied)
							require.NotErrorIs(t, err, ErrClaudeAIAuthRequired)
							require.EqualError(t, err, "claude.ai access denied (HTTP 403)")
						}
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

type interruptedSyncStore struct {
	*db.DB
	cancel context.CancelFunc
}

func (s *interruptedSyncStore) WriteSessionBatchAtomic(ctx context.Context, writes []db.SessionBatchWrite, beforeCommit ...func() error) (db.SessionBatchResult, error) {
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
			_, err := tx.ExecContext(t.Context(), "UPDATE sessions SET local_modified_at = '2026-03-01T10:00:00Z' WHERE id = 'claude-ai:22222222-2222-4222-8222-222222222222'")
			return err
		}))
		before, err := d.GetSessionFull(t.Context(), "claude-ai:22222222-2222-4222-8222-222222222222")
		require.NoError(t, err)
		beforeMessages, err := d.GetAllMessages(t.Context(), before.ID)
		require.NoError(t, err)
		calls := 0
		fetch := syncOneFetch(t, strings.ReplaceAll(syncSummary, "10:05:00.123456Z", "10:06:00Z"), func() (ClaudeAIResponse, error) {
			calls++
			return ClaudeAIResponse{Status: 200, Body: []byte(strings.ReplaceAll(syncDetail, "10:05:00.123456Z", "10:06:00Z"))}, nil
		})
		stats, err := SyncClaudeAI(t.Context(), failFillStore{d}, fetch, nil)
		require.NoError(t, err)
		assert.Equal(t, 1, stats.Errors)
		after, err := d.GetSessionFull(t.Context(), "claude-ai:22222222-2222-4222-8222-222222222222")
		require.NoError(t, err)
		assert.Equal(t, before, after)
		afterMessages, err := d.GetAllMessages(t.Context(), before.ID)
		require.NoError(t, err)
		assert.Equal(t, beforeMessages, afterMessages)
		stats, err = SyncClaudeAI(t.Context(), d, fetch, nil)
		require.NoError(t, err)
		assert.Equal(t, 1, stats.Updated)
		after, err = d.GetSessionFull(t.Context(), "claude-ai:22222222-2222-4222-8222-222222222222")
		require.NoError(t, err)
		require.NotNil(t, after)
		assert.Equal(t, strPtr("2026-03-01T10:06:00Z"), after.EndedAt)
		assert.Equal(t, strPtr("claude-ai:v1:full:reply"), after.LastEntryUUID)
		assert.NotEqual(t, before.LocalModifiedAt, after.LocalModifiedAt)
		stats, err = SyncClaudeAI(t.Context(), d, fetch, nil)
		require.NoError(t, err)
		assert.Equal(t, 1, stats.Skipped)
		assert.Equal(t, 2, calls)
	})
	for _, committed := range []bool{false, true} {
		t.Run(strconv.FormatBool(committed), func(t *testing.T) {
			d := testDB(t)
			oldDetail := strings.Replace(syncDetail, "10:05:00.123456Z", "10:04:00Z", 1)
			_, err := SyncClaudeAI(t.Context(), d, syncOneFetch(t, strings.Replace(syncSummary, "10:05:00.123456Z", "10:04:00Z", 1), func() (ClaudeAIResponse, error) {
				return ClaudeAIResponse{Status: 200, Body: []byte(oldDetail)}, nil
			}), nil)
			require.NoError(t, err)
			beforeSession, err := d.GetSessionFull(t.Context(), "claude-ai:22222222-2222-4222-8222-222222222222")
			require.NoError(t, err)
			require.NotNil(t, beforeSession)
			beforeMessages, err := d.GetAllMessages(t.Context(), "claude-ai:22222222-2222-4222-8222-222222222222")
			require.NoError(t, err)
			detail := strings.TrimSuffix(strings.Replace(syncDetail, `"current_leaf_message_uuid":"reply"`, `"current_leaf_message_uuid":"a2"`, 1), "]}") + `,{"uuid":"q2","parent_message_uuid":"reply","sender":"human","text":"More"},{"uuid":"a2","parent_message_uuid":"q2","sender":"assistant","text":"Answer"}]}`
			details := 0
			fetch := syncOneFetch(t, strings.Replace(syncSummary, "reply", "a2", 1), func() (ClaudeAIResponse, error) {
				details++
				return ClaudeAIResponse{Status: 200, Body: []byte(detail)}, nil
			})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var store interface {
				db.Store
				IsSessionTrashed(context.Context, string) bool
				IsSessionExcluded(context.Context, string) bool
			} = failFillStore{d}
			if committed {
				store = &interruptedSyncStore{DB: d, cancel: cancel}
			}
			stats, err := SyncClaudeAI(ctx, store, fetch, nil)
			session, readErr := d.GetSessionFull(t.Context(), "claude-ai:22222222-2222-4222-8222-222222222222")
			require.NoError(t, readErr)
			require.NotNil(t, session)
			messages, readErr := d.GetAllMessages(t.Context(), session.ID)
			require.NoError(t, readErr)
			if committed {
				require.ErrorIs(t, err, context.Canceled)
				assert.Equal(t, 1, stats.Updated)
				assert.Zero(t, stats.Errors)
				assert.Equal(t, 4, session.MessageCount)
				assert.Equal(t, strPtr("2026-03-01T10:05:00.123456Z"), session.EndedAt)
				assert.Equal(t, strPtr("claude-ai:v1:full:a2"), session.LastEntryUUID)
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
		{name: "requested uuid mismatch", detail: strings.Replace(syncDetail, `"uuid":"22222222-2222-4222-8222-222222222222"`, `"uuid":"22222222-2222-4222-8222-222222222224"`, 1), failed: true},
		{name: "duplicate message uuid", detail: strings.Replace(syncDetail, `"uuid":"reply"`, `"uuid":"root"`, 1), failed: true},
		{name: "grown history preserves ids", detail: strings.TrimSuffix(strings.Replace(syncDetail, `"current_leaf_message_uuid":"reply"`, `"current_leaf_message_uuid":"a2"`, 1), "]}") + `,{"uuid":"q2","parent_message_uuid":"reply","sender":"human","text":"","content":[{"type":"text","text":"More"}]},{"uuid":"a2","parent_message_uuid":"q2","sender":"assistant","text":"","content":[{"type":"text","text":"Answer"}]}]}`, want: []string{"Hello", "Chosen reply", "More", "Answer"}},
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
			if tt.name == "grown history preserves ids" {
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
			for i, m := range before {
				assert.Equal(t, m.ID, after[i].ID)
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

func TestSyncClaudeAIEmptyListSkipsDetail(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(strconv.FormatBool(existing), func(t *testing.T) {
			d := testDB(t)
			if existing {
				_, err := ImportClaudeAI(t.Context(), d, strings.NewReader("["+syncDetail+"]"), nil)
				require.NoError(t, err)
			}
			before, err := d.GetAllMessages(t.Context(), "claude-ai:22222222-2222-4222-8222-222222222222")
			require.NoError(t, err)
			summary := strings.Replace(syncSummary, `"current_leaf_message_uuid":"reply"`, `"current_leaf_message_uuid":null`, 1)
			fetch := syncOneFetch(t, summary, func() (ClaudeAIResponse, error) {
				require.FailNow(t, "empty list item fetched its detail")
				return ClaudeAIResponse{}, nil
			})
			for range 2 {
				stats, err := SyncClaudeAI(t.Context(), d, fetch, nil)
				require.NoError(t, err)
				assert.Equal(t, 1, stats.Skipped)
				assert.Zero(t, stats.Imported+stats.Updated+stats.Errors)
			}
			after, err := d.GetAllMessages(t.Context(), "claude-ai:22222222-2222-4222-8222-222222222222")
			require.NoError(t, err)
			assert.Equal(t, before, after)
			assert.Empty(t, replacedCopies(t, d, "claude-ai:22222222-2222-4222-8222-222222222222"))
			session, err := d.GetSession(t.Context(), "claude-ai:22222222-2222-4222-8222-222222222222")
			require.NoError(t, err)
			if existing {
				require.NotNil(t, session)
				assert.Equal(t, "2026-03-01T10:05:00.123456Z", *session.EndedAt)
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
		{name: "bad request", status: 400, wantCalls: 1},
		{name: "not found", status: 404, wantCalls: 1},
		{name: "exhausted retries", status: 503, wantCalls: 5},
		{name: "oversize", err: ErrClaudeAIResponseTooLarge, wantCalls: 1},
		{name: "unauthorized", status: 401, wantCalls: 1},
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
				if tt.status == 401 || tt.status == 403 {
					if tt.status == 401 {
						require.ErrorIs(t, err, ErrClaudeAIAuthRequired)
					} else {
						require.ErrorIs(t, err, ErrClaudeAIAccessDenied)
						require.NotErrorIs(t, err, ErrClaudeAIAuthRequired)
					}
					assert.Zero(t, later)
					assert.Zero(t, stats.Errors+stats.Imported+stats.Skipped)
					return
				}
				if tt.status == 0 && !errors.Is(tt.err, ErrClaudeAIResponseTooLarge) {
					require.ErrorIs(t, err, tt.err)
					assert.Zero(t, later)
					assert.Zero(t, stats.Errors+stats.Imported+stats.Skipped)
					return
				}
				require.NoError(t, err)
				assert.Equal(t, 1, later)
				if tt.status == 404 {
					assert.Equal(t, 1, stats.Skipped)
					assert.Zero(t, stats.Errors)
				} else {
					assert.Equal(t, 1, stats.Errors)
					assert.Equal(t, []ImportRefusal{{SessionID: "claude-ai:22222222-2222-4222-8222-222222222222", Reason: RefusalTransient}}, stats.Refusals)
				}
				assert.Equal(t, 1, stats.Imported)
			})
		})
	}
}

func TestSyncClaudeAIMalformedRetries(t *testing.T) {
	for _, detail := range []string{"null", `{"uuid":"22222222-2222-4222-8222-222222222222"}`, strings.ReplaceAll(syncDetail, "10:05:00.123456Z", "bad"), strings.Replace(syncDetail, `"current_leaf_message_uuid":"reply"`, `"current_leaf_message_uuid":null`, 1)} {
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

func TestSyncClaudeAIZipFreshness(t *testing.T) {
	d := testDB(t)
	stats, err := ImportClaudeAI(t.Context(), d, strings.NewReader("["+syncDetail+"]"), nil)
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
		return ClaudeAIResponse{Status: 200, Body: []byte(syncDetail)}, nil
	})
	for range 2 {
		stats, err = SyncClaudeAI(t.Context(), d, fetch, nil)
		require.NoError(t, err)
		assert.Equal(t, 1, stats.Skipped)
		assert.Zero(t, stats.Updated)
	}
	assert.Equal(t, 1, calls)
	after, err := d.GetAllMessages(t.Context(), "claude-ai:22222222-2222-4222-8222-222222222222")
	require.NoError(t, err)
	for _, row := range after {
		assert.Empty(t, row.SourceUUID)
	}
	assert.Equal(t, before, after)
	afterFindings, err := d.SessionSecretFindings(t.Context(), "claude-ai:22222222-2222-4222-8222-222222222222")
	require.NoError(t, err)
	require.Len(t, afterFindings, 1)
	assert.Equal(t, findings, afterFindings)
	assert.Empty(t, replacedCopies(t, d, "claude-ai:22222222-2222-4222-8222-222222222222"))
	marked, err := d.GetSessionFull(t.Context(), "claude-ai:22222222-2222-4222-8222-222222222222")
	require.NoError(t, err)
	require.NotNil(t, marked)
	require.NotNil(t, marked.TranscriptRevision)
	assert.Equal(t, *initial.TranscriptRevision, *marked.TranscriptRevision)
	assert.Equal(t, strPtr("claude-ai:v1:full:reply"), marked.LastEntryUUID)
	_, err = ImportClaudeAI(t.Context(), d, strings.NewReader("["+syncDetail+"]"), nil)
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
			before, err := d.GetAllMessages(t.Context(), "claude-ai:22222222-2222-4222-8222-222222222222")
			require.NoError(t, err)
			require.Len(t, before, 4)
			for _, message := range []db.Message{before[1], before[3]} {
				_, err := d.PinMessage(t.Context(), "claude-ai:22222222-2222-4222-8222-222222222222", message.ID, nil)
				require.NoError(t, err)
			}
			for flip := range 5 {
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
				pins, err := d.ListPinnedMessages(t.Context(), "claude-ai:22222222-2222-4222-8222-222222222222", "")
				require.NoError(t, err)
				require.Len(t, pins, 1)
				assert.Equal(t, messages[1].ID, pins[0].MessageID)
				copies := replacedCopies(t, d, "claude-ai:22222222-2222-4222-8222-222222222222")
				require.Len(t, copies, 1)
				oldPins, err := d.ListPinnedMessages(t.Context(), copies[0].ID, "")
				require.NoError(t, err)
				require.Len(t, oldPins, 2)
				assert.ElementsMatch(t, []int{1, 3}, []int{oldPins[0].Ordinal, oldPins[1].Ordinal})
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
	stats, err := ImportClaudeAI(t.Context(), d, strings.NewReader(`[{"uuid":"22222222-2222-4222-8222-222222222222","created_at":"2026-03-01T10:00:00Z","updated_at":"2026-03-01T10:04:00Z","chat_messages":[{"sender":"human","text":"Hello"}]}]`), nil)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Errors)
	assert.Equal(t, []ImportRefusal{{SessionID: "claude-ai:22222222-2222-4222-8222-222222222222", Reason: RefusalShorterExport}}, stats.Refusals)
	marked, err := d.GetSessionFull(t.Context(), "claude-ai:22222222-2222-4222-8222-222222222222")
	require.NoError(t, err)
	require.NotNil(t, marked)
	assert.Equal(t, strPtr("claude-ai:v1:full:reply"), marked.LastEntryUUID)
}

func TestSyncClaudeAINewerMarkerError(t *testing.T) {
	for _, marker := range []string{"claude-ai:v2:full:future-leaf", "claude-ai:v2:opaque", "claude-ai:v3:"} {
		t.Run(marker, func(t *testing.T) {
			d := testDB(t)
			const id = "claude-ai:22222222-2222-4222-8222-222222222222"
			require.NoError(t, d.UpsertSession(t.Context(), db.Session{ID: id, Agent: "claude-ai", Project: "test", Machine: "test", LastEntryUUID: strPtr(marker)}))
			before, err := d.GetSessionFull(t.Context(), id)
			require.NoError(t, err)
			stats, err := SyncClaudeAI(t.Context(), d, syncOneFetch(t, syncSummary, func() (ClaudeAIResponse, error) {
				require.FailNow(t, "newer marker fetched its detail")
				return ClaudeAIResponse{}, nil
			}), nil)
			require.NoError(t, err)
			assert.Equal(t, 1, stats.Errors)
			assert.Equal(t, []ImportRefusal{{SessionID: id, Reason: RefusalNewerMarker}}, stats.Refusals)
			assert.Zero(t, stats.Skipped+stats.Updated+stats.Imported)
			after, err := d.GetSessionFull(t.Context(), id)
			require.NoError(t, err)
			assert.Equal(t, before, after)
		})
	}
}

func TestSyncClaudeAIInvalidListLeaf(t *testing.T) {
	for _, tt := range []struct {
		name, field string
	}{
		{"absent", ""},
		{"empty", `"current_leaf_message_uuid":"",`},
		{"number", `"current_leaf_message_uuid":42,`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDB(t)
			summary := strings.Replace(syncSummary, `"current_leaf_message_uuid":"reply",`, tt.field, 1)
			stats, err := SyncClaudeAI(t.Context(), d, syncOneFetch(t, summary, func() (ClaudeAIResponse, error) {
				require.FailNow(t, "invalid list leaf fetched its detail")
				return ClaudeAIResponse{}, nil
			}), nil)
			require.NoError(t, err)
			assert.Equal(t, 1, stats.Errors)
			assert.Zero(t, stats.Skipped+stats.Imported+stats.Updated)
		})
	}
}

func TestSyncClaudeAIZipAppendDuplicatePromptPin(t *testing.T) {
	d := testDB(t)
	const id = "claude-ai:22222222-2222-4222-8222-222222222222"
	short := strings.ReplaceAll(strings.ReplaceAll(syncDetail, "Hello", "go"), "Chosen reply", "ok")
	stats, err := ImportClaudeAI(t.Context(), d, strings.NewReader("["+short+"]"), nil)
	require.NoError(t, err)
	require.Equal(t, 1, stats.Imported)
	before, err := d.GetAllMessages(t.Context(), id)
	require.NoError(t, err)
	require.Len(t, before, 2)
	_, err = d.PinMessage(t.Context(), id, before[0].ID, strPtr("Keep the first prompt"))
	require.NoError(t, err)
	long := strings.TrimSuffix(strings.Replace(short, `"current_leaf_message_uuid":"reply"`, `"current_leaf_message_uuid":"a2"`, 1), "]}") + `,{"uuid":"q2","parent_message_uuid":"reply","sender":"human","text":"go","created_at":"2026-03-01T10:00:00Z"},{"uuid":"a2","parent_message_uuid":"q2","sender":"assistant","text":"ok2","created_at":"2026-03-01T10:02:00Z"}]}`
	stats, err = SyncClaudeAI(t.Context(), d, syncOneFetch(t, strings.Replace(syncSummary, "reply", "a2", 1), func() (ClaudeAIResponse, error) {
		return ClaudeAIResponse{Status: 200, Body: []byte(long)}, nil
	}), nil)
	require.NoError(t, err)
	require.Equal(t, 1, stats.Updated)
	stats, err = SyncClaudeAI(t.Context(), d, syncOneFetch(t, syncSummary, func() (ClaudeAIResponse, error) {
		return ClaudeAIResponse{Status: 200, Body: []byte(short)}, nil
	}), nil)
	require.NoError(t, err)
	require.Equal(t, 1, stats.Updated)
	pins, err := d.ListPinnedMessages(t.Context(), id, "")
	require.NoError(t, err)
	assert.Empty(t, pins)
	copies := replacedCopies(t, d, id)
	require.Len(t, copies, 1)
	pins, err = d.ListPinnedMessages(t.Context(), copies[0].ID, "")
	require.NoError(t, err)
	require.Len(t, pins, 1)
	assert.Equal(t, 0, pins[0].Ordinal)
	assert.Equal(t, strPtr("Keep the first prompt"), pins[0].Note)
}

func TestSyncClaudeAIEqualTextBranchIdentities(t *testing.T) {
	d := testDB(t)
	const id = "claude-ai:22222222-2222-4222-8222-222222222222"
	branchA := strings.TrimSuffix(strings.Replace(syncDetail, `"current_leaf_message_uuid":"reply"`, `"current_leaf_message_uuid":"a2"`, 1), "]}") + `,{"uuid":"q2","parent_message_uuid":"reply","sender":"human","text":"More"},{"uuid":"a2","parent_message_uuid":"q2","sender":"assistant","text":"Answer"}]}`
	branchB := strings.ReplaceAll(strings.ReplaceAll(branchA, "q2", "edit"), "a2", "b2")
	syncBranch := func(detail, leaf string) {
		t.Helper()
		stats, err := SyncClaudeAI(t.Context(), d, syncOneFetch(t, strings.Replace(syncSummary, "reply", leaf, 1), func() (ClaudeAIResponse, error) {
			return ClaudeAIResponse{Status: 200, Body: []byte(detail)}, nil
		}), nil)
		require.NoError(t, err)
		require.Zero(t, stats.Errors)
	}
	syncBranch(branchA, "a2")
	before, err := d.GetAllMessages(t.Context(), id)
	require.NoError(t, err)
	require.Len(t, before, 4)
	for _, ordinal := range []int{1, 2} {
		_, err = d.PinMessage(t.Context(), id, before[ordinal].ID, nil)
		require.NoError(t, err)
	}
	syncBranch(branchB, "b2")
	after, err := d.GetAllMessages(t.Context(), id)
	require.NoError(t, err)
	require.Len(t, after, 4)
	assert.Equal(t, messageContents(before), messageContents(after))
	for i, uuid := range []string{"root", "reply", "edit", "b2"} {
		assert.Equal(t, uuid, after[i].SourceUUID)
	}
	session, err := d.GetSessionFull(t.Context(), id)
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, strPtr("claude-ai:v1:full:b2"), session.LastEntryUUID)
	pins, err := d.ListPinnedMessages(t.Context(), id, "")
	require.NoError(t, err)
	require.Len(t, pins, 1)
	assert.Equal(t, after[1].ID, pins[0].MessageID)
	_, err = d.PinMessage(t.Context(), id, after[2].ID, nil)
	require.NoError(t, err)
	syncBranch(branchA, "a2")
	restored, err := d.GetAllMessages(t.Context(), id)
	require.NoError(t, err)
	require.Len(t, restored, 4)
	assert.Equal(t, "q2", restored[2].SourceUUID)
	pins, err = d.ListPinnedMessages(t.Context(), id, "")
	require.NoError(t, err)
	require.Len(t, pins, 1)
	assert.Equal(t, restored[1].ID, pins[0].MessageID)
}

func TestSyncClaudeAIZipDuplicatePromptPin(t *testing.T) {
	d := testDB(t)
	const id = "claude-ai:22222222-2222-4222-8222-222222222222"
	detail := strings.TrimSuffix(strings.Replace(syncDetail, `"current_leaf_message_uuid":"reply"`, `"current_leaf_message_uuid":"a2"`, 1), "]}") + `,{"uuid":"q2","parent_message_uuid":"reply","sender":"human","text":"Hello","created_at":"2026-03-01T10:00:00Z"},{"uuid":"a2","parent_message_uuid":"q2","sender":"assistant","text":"Second answer","created_at":"2026-03-01T10:02:00Z"}]}`
	zipPath := createTestZip(t, map[string]string{"conversations.json": "[" + detail + "]"})
	dir, cleanup, err := ExtractZip(zipPath)
	require.NoError(t, err)
	defer cleanup()
	reader, err := os.Open(filepath.Join(dir, "conversations.json"))
	require.NoError(t, err)
	defer reader.Close()
	_, err = ImportClaudeAI(t.Context(), d, reader, nil)
	require.NoError(t, err)
	before, err := d.GetAllMessages(t.Context(), id)
	require.NoError(t, err)
	require.Len(t, before, 4)
	_, err = d.PinMessage(t.Context(), id, before[0].ID, strPtr("Keep the first prompt"))
	require.NoError(t, err)
	summary := strings.Replace(syncSummary, "reply", "a2", 1)
	stats, err := SyncClaudeAI(t.Context(), d, syncOneFetch(t, summary, func() (ClaudeAIResponse, error) {
		return ClaudeAIResponse{Status: 200, Body: []byte(detail)}, nil
	}), nil)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Skipped)
	after, err := d.GetAllMessages(t.Context(), id)
	require.NoError(t, err)
	require.Len(t, after, 4)
	assert.Equal(t, before, after)
	stats, err = SyncClaudeAI(t.Context(), d, syncOneFetch(t, syncSummary, func() (ClaudeAIResponse, error) {
		return ClaudeAIResponse{Status: 200, Body: []byte(syncDetail)}, nil
	}), nil)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Updated)
	pins, err := d.ListPinnedMessages(t.Context(), id, "")
	require.NoError(t, err)
	assert.Empty(t, pins)
	copies := replacedCopies(t, d, id)
	require.Len(t, copies, 1)
	pins, err = d.ListPinnedMessages(t.Context(), copies[0].ID, "")
	require.NoError(t, err)
	require.Len(t, pins, 1)
	assert.Equal(t, 0, pins[0].Ordinal)
	assert.Equal(t, strPtr("Keep the first prompt"), pins[0].Note)
}

func TestSyncClaudeAIObservedLists(t *testing.T) {
	for _, tt := range []struct {
		name, page string
		want       int
	}{
		{"archived=false", syncActiveList, 1},
		{"archived=true", syncArchivedList, 1},
		{"no archived parameter", syncAllList, 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDB(t)
			details := 0
			fetch := func(ctx context.Context, path string) (ClaudeAIResponse, error) {
				switch path {
				case "/api/organizations":
					return ClaudeAIResponse{Status: 200, Body: []byte(syncOrgs)}, nil
				case "/api/organizations/11111111-1111-4111-8111-111111111111/chat_conversations_v2?limit=50&offset=0":
					return ClaudeAIResponse{Status: 200, Body: []byte(tt.page)}, nil
				default:
					details++
					detail := syncDetail
					if strings.Contains(path, "/22222222-2222-4222-8222-222222222223?") {
						detail = strings.ReplaceAll(detail, "22222222-2222-4222-8222-222222222222", "22222222-2222-4222-8222-222222222223")
					} else {
						require.Contains(t, path, "/chat_conversations/22222222-2222-4222-8222-222222222222?")
					}
					return ClaudeAIResponse{Status: 200, Body: []byte(detail)}, nil
				}
			}
			stats, err := SyncClaudeAI(t.Context(), d, fetch, nil)
			require.NoError(t, err)
			assert.Equal(t, tt.want, stats.Imported)
			stats, err = SyncClaudeAI(t.Context(), d, fetch, nil)
			require.NoError(t, err)
			assert.Equal(t, tt.want, stats.Skipped)
			assert.Equal(t, tt.want, details)
		})
	}
}
