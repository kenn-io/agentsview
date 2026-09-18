// ABOUTME: Tests for the Cursor IDE (GUI) parser: cursorDiskKV composer/bubble
// ABOUTME: decoding, message ordering, tool calls, and provider source methods.
package parser

import (
	"database/sql"
	"encoding/json"
	"encoding/json/jsontext"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cursorIDETestBubble is one synthetic cursorDiskKV bubble row.
type cursorIDETestBubble struct {
	id         string
	bubbleType int
	text       string
	createdAt  string
	tool       *cursorIDEToolFormerData
	raw        []byte
}

// cursorIDETestComposer is one synthetic composerData document plus its
// bubbles, keyed under the same composer ID.
type cursorIDETestComposer struct {
	id        string
	name      string
	createdAt int64
	updatedAt int64
	cwd       string
	repoPath  string
	branch    string
	bubbles   []cursorIDETestBubble
	// omitBubbleIDs skips writing these bubble IDs to cursorDiskKV even
	// though they are still listed in fullConversationHeadersOnly, so the
	// parser must tolerate a header pointing at a row Cursor never wrote or
	// later wiped.
	omitBubbleIDs map[string]bool
}

func createCursorIDEDB(t *testing.T, composers []cursorIDETestComposer) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), CursorIDEDBRelPath)
	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	defer db.Close()

	_, err = db.ExecContext(t.Context(),
		`CREATE TABLE cursorDiskKV (key TEXT UNIQUE ON CONFLICT REPLACE, value BLOB)`,
	)
	require.NoError(t, err)

	for _, c := range composers {
		headers := make([]cursorIDEComposerHeader, 0, len(c.bubbles))
		for _, b := range c.bubbles {
			headers = append(headers, cursorIDEComposerHeader{
				BubbleID: b.id, Type: b.bubbleType,
			})
		}
		doc := cursorIDEComposerDoc{
			Headers:       headers,
			Name:          c.name,
			CreatedAt:     c.createdAt,
			LastUpdatedAt: c.updatedAt,
		}
		doc.WorkspaceIdentifier.URI.FSPath = c.cwd
		if c.repoPath != "" {
			doc.TrackedGitRepos = []cursorIDEGitRepo{{
				RepoPath: c.repoPath,
				Branches: []cursorIDEGitBranch{{
					BranchName: c.branch, LastInteractionAt: c.updatedAt,
				}},
			}}
		}
		raw, err := json.Marshal(doc)
		require.NoError(t, err)
		_, err = db.ExecContext(t.Context(),
			`INSERT INTO cursorDiskKV (key, value) VALUES (?, ?)`,
			cursorIDEComposerKeyPrefix+c.id, raw,
		)
		require.NoError(t, err)

		for _, b := range c.bubbles {
			if c.omitBubbleIDs[b.id] {
				continue
			}
			bubble := cursorIDEBubble{
				Type: b.bubbleType, Text: b.text, CreatedAt: b.createdAt,
				ToolFormerData: b.tool,
			}
			raw, err := json.Marshal(bubble)
			require.NoError(t, err)
			if b.raw != nil {
				raw = b.raw
			}
			_, err = db.ExecContext(t.Context(),
				`INSERT INTO cursorDiskKV (key, value) VALUES (?, ?)`,
				cursorIDEBubbleKeyPrefix+c.id+":"+b.id, raw,
			)
			require.NoError(t, err)
		}
	}
	return dbPath
}

// assertCursorDiskKVStoredShape pins the on-disk premise the null/empty-blob
// subtests rest on: a SQL NULL value reports typeof "null", while a stored
// zero-length BLOB reports typeof "blob" with length 0. Without this check,
// a driver whose bind path folded []byte{} into NULL (the repo carries a
// second SQLite driver, modernc.org/sqlite, so this is not hypothetical)
// would silently collapse the empty-blob subtests into copies of their null
// siblings and still pass.
func assertCursorDiskKVStoredShape(t *testing.T, dbPath, key string, wantNull bool) {
	t.Helper()
	conn, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	defer conn.Close()
	var typ string
	var length sql.NullInt64
	require.NoError(t, conn.QueryRowContext(t.Context(),
		`SELECT typeof(value), length(value) FROM cursorDiskKV WHERE key = ?`,
		key,
	).Scan(&typ, &length))
	if wantNull {
		assert.Equal(t, "null", typ)
		return
	}
	assert.Equal(t, "blob", typ)
	require.True(t, length.Valid)
	assert.Zero(t, length.Int64)
}

func cursorIDEStringResult(t *testing.T, text string) jsontext.Value {
	t.Helper()
	raw, err := json.Marshal(text)
	require.NoError(t, err)
	return jsontext.Value(raw)
}

func TestCursorIDEProviderCapabilities(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	factory, ok := ProviderFactoryByType(AgentCursorIDE)
	require.True(ok)
	caps := factory.Capabilities()
	assert.Equal(CapabilitySupported, caps.Content.ToolCalls)
	assert.Equal(CapabilitySupported, caps.Content.ToolResults)

	provider, ok := NewProvider(AgentCursorIDE, ProviderConfig{
		Roots: []string{t.TempDir()}, Machine: "devbox",
	})
	require.True(ok)
	require.NotNil(provider)
}

func TestCursorIDEProviderDiscoverAndParse(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dbPath := createCursorIDEDB(t, []cursorIDETestComposer{
		{
			id:        "0cd7922b-f080-4672-b26f-2d521feee055",
			name:      "Factory course status",
			createdAt: 1782026756842, // 2026-06-21T07:25:56.842Z
			updatedAt: 1782026791522, // 2026-06-21T07:26:31.522Z
			cwd:       "/Users/alice/dev/dark-factory",
			repoPath:  "/Users/alice/dev/dark-factory",
			branch:    "dev",
			bubbles: []cursorIDETestBubble{
				{
					id: "8572a71d-1e3b-4602-bcbb-b76830401b89", bubbleType: cursorIDEBubbleTypeUser,
					text:      "give me an overview on the development status",
					createdAt: "2026-06-21T07:27:29.606Z",
				},
				{
					id: "7337a4cc-dd16-42d3-961e-18c15afba4ca", bubbleType: cursorIDEBubbleTypeAssistant,
					text:      "I'll inspect the factory-course package structure.",
					createdAt: "2026-06-21T07:27:31.522Z",
				},
				{
					id: "25b40601-e097-42e9-b695-d55aa242e920", bubbleType: cursorIDEBubbleTypeAssistant,
					createdAt: "2026-06-21T07:27:32.000Z",
					tool: &cursorIDEToolFormerData{
						ToolCallID: "tool_d4d61399",
						Name:       "glob_file_search",
						RawArgs:    `{"targetDirectory":"/Users/alice/dev/dark-factory","globPattern":"**/*"}`,
						Result:     cursorIDEStringResult(t, `{"directories":[{"absPath":"/Users/alice/dev/dark-factory","files":[]}]}`),
					},
				},
			},
		},
		{
			// An empty draft composer: no headers, so it must not surface as
			// a session at all.
			id:        "empty-draft-0000-0000-0000-000000000000",
			name:      "",
			createdAt: 1782026756000,
			updatedAt: 1782026756000,
		},
	})
	root := filepath.Dir(dbPath)

	provider, ok := NewProvider(AgentCursorIDE, ProviderConfig{
		Roots: []string{root}, Machine: "devbox",
	})
	require.True(ok)

	plan, err := provider.WatchPlan(t.Context())
	require.NoError(err)
	require.Len(plan.Roots, 1)
	assert.Equal(root, plan.Roots[0].Path)
	assert.False(plan.Roots[0].Recursive)
	assert.Equal([]string{"state.vscdb", "state.vscdb-*"}, plan.Roots[0].IncludeGlobs)

	discovered, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(discovered, 1)
	assert.Equal(AgentCursorIDE, discovered[0].Provider)
	assert.Equal(dbPath, discovered[0].DisplayPath)

	fingerprint, err := provider.Fingerprint(t.Context(), discovered[0])
	require.NoError(err)
	require.NotZero(fingerprint.MTimeNS)

	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source: discovered[0], Machine: "devbox", Fingerprint: fingerprint,
	})
	require.NoError(err)
	require.Len(outcome.Results, 1, "the empty draft composer must not surface as a session")

	sess := outcome.Results[0].Result.Session
	messages := outcome.Results[0].Result.Messages
	assert.Equal("cursor-ide:0cd7922b-f080-4672-b26f-2d521feee055", sess.ID)
	assert.Equal(AgentCursorIDE, sess.Agent)
	assert.Equal("Factory course status", sess.SessionName)
	assert.Equal("/Users/alice/dev/dark-factory", sess.Cwd)
	assert.Equal("dark_factory", sess.Project)
	assert.Equal("dev", sess.GitBranch)
	assert.Equal(1, sess.UserMessageCount)
	assert.Equal("give me an overview on the development status", sess.FirstMessage)

	// composerData createdAt/lastUpdatedAt are epoch milliseconds; the parser
	// must not confuse them with the bubbles' ISO-8601 createdAt encoding.
	// This fixture's lastUpdatedAt (07:26:31.522Z) lags the final bubble, so
	// EndedAt comes from the latest message timestamp instead.
	assert.Equal(time.UnixMilli(1782026756842).UTC(), sess.StartedAt)
	assert.Equal(time.Date(2026, 6, 21, 7, 27, 32, 0, time.UTC), sess.EndedAt)
	// Both encodings describe the same real conversation, so they must land
	// within the same window rather than merely both parsing without error.
	assert.WithinDuration(sess.StartedAt, sess.EndedAt, 2*time.Minute)

	require.Len(messages, 3)
	assert.Equal(RoleUser, messages[0].Role)
	assert.Equal("give me an overview on the development status", messages[0].Content)
	assert.Equal(RoleAssistant, messages[1].Role)
	assert.Equal("I'll inspect the factory-course package structure.", messages[1].Content)
	assert.Equal(RoleAssistant, messages[2].Role)
	assert.True(messages[2].HasToolUse)
	require.Len(messages[2].ToolCalls, 1)
	assert.Equal("glob_file_search", messages[2].ToolCalls[0].ToolName)
	assert.Contains(messages[2].ToolCalls[0].InputJSON, "globPattern")
	require.Len(messages[2].ToolResults, 1)
	assert.Contains(messages[2].ToolResults[0].ContentRaw, "absPath")

	// Ordering must come from fullConversationHeadersOnly, not from a
	// lexicographic bubble-key scan.
	for i, m := range messages {
		assert.Equal(i, m.Ordinal)
	}
}

func TestCursorIDEProviderObjectToolResult(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	raw, err := os.ReadFile("testdata/cursor-ide-object-tool-result.json")
	require.NoError(err)
	dbPath := createCursorIDEDB(t, []cursorIDETestComposer{
		{id: "a-healthy", bubbles: []cursorIDETestBubble{{id: "before", bubbleType: 1, text: "before"}}},
		{id: "object-result", bubbles: []cursorIDETestBubble{{id: "tool-bubble", bubbleType: 2, raw: raw}}},
		{id: "z-healthy", bubbles: []cursorIDETestBubble{{id: "after", bubbleType: 1, text: "after"}}},
	})
	provider, ok := NewProvider(AgentCursorIDE, ProviderConfig{Roots: []string{filepath.Dir(dbPath)}, Machine: "test"})
	require.True(ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 1)
	fingerprint, err := provider.Fingerprint(t.Context(), sources[0])
	require.NoError(err)
	outcome, err := provider.Parse(t.Context(), ParseRequest{Source: sources[0], Machine: "test", Fingerprint: fingerprint})
	t.Logf("container results=%d error=%v", len(outcome.Results), err)
	require.NoError(err)
	require.Len(outcome.Results, 3)
	var ids []string
	for _, entry := range outcome.Results {
		result := entry.Result
		ids = append(ids, result.Session.ID)
		assert.False(result.Session.IsTruncated)
		require.Len(result.Messages, 1)
		msg := result.Messages[0]
		switch result.Session.ID {
		case "cursor-ide:a-healthy":
			assert.Equal("before", msg.Content)
			assert.Equal("before", msg.SourceUUID)
		case "cursor-ide:z-healthy":
			assert.Equal("after", msg.Content)
			assert.Equal("after", msg.SourceUUID)
		case "cursor-ide:object-result":
			assert.Equal("tool-bubble", msg.SourceUUID)
			assert.Equal(RoleAssistant, msg.Role)
			assert.True(msg.HasToolUse)
			require.Len(msg.ToolCalls, 1)
			assert.Equal("todo_write", msg.ToolCalls[0].ToolName)
			assert.Equal("00000000-0000-4000-8000-000000000001", msg.ToolCalls[0].ToolUseID)
			assert.Equal(`{"todos":[{"id":"sample-task","content":"sample-task","status":"TODO_STATUS_IN_PROGRESS","createdAt":"1782026756842","updatedAt":"1782026756842","dependencies":[]}],"merge":true}`, msg.ToolCalls[0].InputJSON)
			require.Len(msg.ToolResults, 1)
			want := `{"success":true,"readyTaskIds":[],"needsInProgressTodos":false,"finalTodos":[{"content":"sample-task","status":"in_progress","id":"sample-task","dependencies":[]}],"initialTodos":[{"content":"sample-task","status":"completed","id":"sample-task","dependencies":[]}],"wasMerge":true}`
			tr := msg.ToolResults[0]
			assert.Equal("00000000-0000-4000-8000-000000000001", tr.ToolUseID)
			assert.Equal(want, DecodeContent(tr.ContentRaw))
			assert.Equal(len(want), tr.ContentLength)
			var text string
			require.NoError(json.Unmarshal([]byte(tr.ContentRaw), &text))
			assert.Equal(want, text)
		}
	}
	assert.ElementsMatch([]string{"cursor-ide:a-healthy", "cursor-ide:object-result", "cursor-ide:z-healthy"}, ids)
}

func TestParseCursorIDEComposer_ResultValues(t *testing.T) {
	for _, tc := range []struct {
		name, field, want, wantRaw string
	}{
		{"string", `,"result":"  café\n\"ok\"\\  "`, "  café\n\"ok\"\\  ", `"  café\n\"ok\"\\  "`},
		{"object", `,"result":{ "ok": true }`, `{ "ok": true }`, `"{ \"ok\": true }"`},
		{"array", `,"result":[1, "two"]`, `[1, "two"]`, `"[1, \"two\"]"`},
		{"number-42", `,"result":42`, "42", `"42"`},
		{"boolean", `,"result":true`, "true", `"true"`},
		{"null", `,"result":null`, "", ""},
		{"absent", "", "", ""},
		{"empty-string", `,"result":""`, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			raw := []byte(`{"type":2,"text":"visible","toolFormerData":{"toolCallId":"call","name":"sample_tool","rawArgs":"{}"` + tc.field + `}}`)
			dbPath := createCursorIDEDB(t, []cursorIDETestComposer{{id: "result-values", bubbles: []cursorIDETestBubble{
				{id: "tool", bubbleType: 2, raw: raw},
				{id: "visible", bubbleType: 2, text: "answer"},
				{id: "bookkeeping", bubbleType: 3},
			}}})
			conn, err := openCursorIDEDB(dbPath)
			require.NoError(err)
			defer conn.Close()
			info, err := os.Stat(dbPath)
			require.NoError(err)
			result, err := parseCursorIDEComposer(t.Context(), conn, dbPath, "result-values", "test", info)
			require.NoError(err)
			require.NotNil(result)
			assert.False(result.Session.IsTruncated)
			require.Len(result.Messages, 2)
			msg := result.Messages[0]
			assert.Equal("tool", msg.SourceUUID)
			assert.Equal("visible", msg.Content)
			assert.Equal(RoleAssistant, msg.Role)
			assert.True(msg.HasToolUse)
			require.Len(msg.ToolCalls, 1)
			assert.Equal("call", msg.ToolCalls[0].ToolUseID)
			assert.Equal("sample_tool", msg.ToolCalls[0].ToolName)
			assert.Equal("{}", msg.ToolCalls[0].InputJSON)
			assert.Equal("answer", result.Messages[1].Content)
			assert.Equal("visible", result.Messages[1].SourceUUID)
			assert.Empty(result.Messages[1].ToolResults)
			if tc.want == "" {
				assert.Empty(msg.ToolResults)
				return
			}
			require.Len(msg.ToolResults, 1)
			tr := msg.ToolResults[0]
			assert.Equal("call", tr.ToolUseID)
			assert.Equal(tc.wantRaw, tr.ContentRaw)
			assert.Equal(tc.want, DecodeContent(tr.ContentRaw))
			assert.Equal(len(tc.want), tr.ContentLength)
			t.Logf("preserved text=%s ContentRaw=%s bytes=%d", DecodeContent(tr.ContentRaw), tr.ContentRaw, tr.ContentLength)
		})
	}
}

func TestParseCursorIDEComposer_MalformedResult(t *testing.T) {
	require := require.New(t)

	const raw = `{"type":2,"toolFormerData":{"result":{"success":true}`
	dbPath := createCursorIDEDB(t, []cursorIDETestComposer{{id: "malformed-result", bubbles: []cursorIDETestBubble{
		{id: "before", bubbleType: 1, text: "before"},
		{id: "broken", bubbleType: 2, raw: []byte(raw)},
	}}})
	conn, err := openCursorIDEDB(dbPath)
	require.NoError(err)
	defer conn.Close()
	info, err := os.Stat(dbPath)
	require.NoError(err)
	result, err := parseCursorIDEComposer(t.Context(), conn, dbPath, "malformed-result", "test", info)
	require.Error(err)
	assert.Nil(t, result)
	t.Logf("input=%s error=%v", raw, err)
}

func TestCursorIDEFindSourceAndFingerprint(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dbPath := createCursorIDEDB(t, []cursorIDETestComposer{{
		id:        "142856b4-34d8-4950-ba25-b45fe1c47941",
		name:      "Thread",
		createdAt: 1782026756842,
		updatedAt: 1782026791522,
		bubbles: []cursorIDETestBubble{{
			id: "b1", bubbleType: cursorIDEBubbleTypeUser,
			text: "hi", createdAt: "2026-06-21T07:27:29.606Z",
		}},
	}})
	root := filepath.Dir(dbPath)
	composerID := "142856b4-34d8-4950-ba25-b45fe1c47941"
	virtualPath := VirtualSourcePath(dbPath, composerID)

	provider, ok := NewProvider(AgentCursorIDE, ProviderConfig{
		Roots: []string{root}, Machine: "devbox",
	})
	require.True(ok)

	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		FullSessionID: "devbox~cursor-ide:" + composerID,
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(virtualPath, found.DisplayPath)

	fingerprint, err := provider.Fingerprint(t.Context(), found)
	require.NoError(err)
	assert.Equal(virtualPath, fingerprint.Key)
	assert.Positive(fingerprint.Size)
	assert.NotZero(fingerprint.MTimeNS)
	assert.NotEmpty(fingerprint.Hash)

	// A vanished composer (row deleted, DB file still present) fingerprints
	// as keyed-empty rather than erroring, so Parse runs and force-replaces
	// the deleted session out of the archive.
	ghost := found
	if src, ok := ghost.Opaque.(multiSessionSource); ok {
		src.MemberID = "does-not-exist"
		src.Path = VirtualSourcePath(dbPath, "does-not-exist")
		ghost.Opaque = src
	}
	ghostFingerprint, err := provider.Fingerprint(t.Context(), ghost)
	require.NoError(err)
	assert.Empty(ghostFingerprint.Hash)
}

func TestParseCursorIDEComposer_ToleratesMissingBubbleRow(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dbPath := createCursorIDEDB(t, []cursorIDETestComposer{{
		id:        "gap-0000-0000-0000-000000000000",
		name:      "Gappy thread",
		createdAt: 1782026756842,
		updatedAt: 1782026791522,
		bubbles: []cursorIDETestBubble{
			{id: "missing", bubbleType: cursorIDEBubbleTypeUser, text: "never written"},
			{id: "b2", bubbleType: cursorIDEBubbleTypeUser, text: "hello", createdAt: "2026-06-21T07:27:29.606Z"},
		},
		omitBubbleIDs: map[string]bool{"missing": true},
	}})

	conn, err := openCursorIDEDB(dbPath)
	require.NoError(err)
	defer conn.Close()
	info, err := os.Stat(dbPath)
	require.NoError(err)

	result, err := parseCursorIDEComposer(
		t.Context(), conn, dbPath,
		"gap-0000-0000-0000-000000000000", "devbox", info,
	)
	require.NoError(err)
	require.NotNil(result)
	require.Len(result.Messages, 1)
	assert.Equal("hello", result.Messages[0].Content)
	assert.True(result.Session.IsTruncated,
		"a transcript with a missing bubble row must be flagged truncated")
}

func TestParseCursorIDEComposer_EmptyComposerSkipped(t *testing.T) {
	require := require.New(t)

	dbPath := createCursorIDEDB(t, []cursorIDETestComposer{{
		id: "empty-0000-0000-0000-000000000000",
	}})
	conn, err := openCursorIDEDB(dbPath)
	require.NoError(err)
	defer conn.Close()
	info, err := os.Stat(dbPath)
	require.NoError(err)

	result, err := parseCursorIDEComposer(
		t.Context(), conn, dbPath,
		"empty-0000-0000-0000-000000000000", "devbox", info,
	)
	require.NoError(err)
	assert.Nil(t, result)
}

func TestParseCursorIDEComposer_MalformedBubbleFailsInsteadOfTruncating(t *testing.T) {
	require := require.New(t)

	dbPath := createCursorIDEDB(t, []cursorIDETestComposer{{
		id:        "corrupt-bubble-0000-0000-000000000000",
		name:      "Corrupt bubble",
		createdAt: 1782026756842,
		updatedAt: 1782026791522,
		bubbles: []cursorIDETestBubble{
			{id: "b1", bubbleType: cursorIDEBubbleTypeUser, text: "kept", createdAt: "2026-06-21T07:27:29.606Z"},
			{id: "b2", bubbleType: cursorIDEBubbleTypeAssistant, text: "reply", createdAt: "2026-06-21T07:27:31.522Z"},
		},
	}})
	writer, err := sql.Open("sqlite3", dbPath)
	require.NoError(err)
	_, err = writer.ExecContext(t.Context(),
		`UPDATE cursorDiskKV SET value = ? WHERE key = ?`,
		[]byte(`{"type": "not-an-int"`),
		"bubbleId:corrupt-bubble-0000-0000-000000000000:b2",
	)
	require.NoError(err)
	require.NoError(writer.Close())

	conn, err := openCursorIDEDB(dbPath)
	require.NoError(err)
	defer conn.Close()
	info, err := os.Stat(dbPath)
	require.NoError(err)

	result, err := parseCursorIDEComposer(
		t.Context(), conn, dbPath,
		"corrupt-bubble-0000-0000-000000000000", "devbox", info,
	)
	require.Error(err,
		"a stored bubble that no longer decodes must fail, not truncate the transcript")
	assert.Nil(t, result)
}

func TestParseCursorIDEComposer_MalformedComposerFailsInsteadOfRetiring(t *testing.T) {
	require := require.New(t)

	dbPath := createCursorIDEDB(t, []cursorIDETestComposer{{
		id:        "corrupt-doc-0000-0000-000000000000",
		name:      "Corrupt doc",
		createdAt: 1782026756842,
		updatedAt: 1782026791522,
		bubbles: []cursorIDETestBubble{{
			id: "b1", bubbleType: cursorIDEBubbleTypeUser, text: "hi",
			createdAt: "2026-06-21T07:27:29.606Z",
		}},
	}})
	writer, err := sql.Open("sqlite3", dbPath)
	require.NoError(err)
	_, err = writer.ExecContext(t.Context(),
		`UPDATE cursorDiskKV SET value = ? WHERE key = ?`,
		[]byte(`{"fullConversationHeadersOnly": [truncated`),
		"composerData:corrupt-doc-0000-0000-000000000000",
	)
	require.NoError(err)
	require.NoError(writer.Close())

	conn, err := openCursorIDEDB(dbPath)
	require.NoError(err)
	defer conn.Close()
	info, err := os.Stat(dbPath)
	require.NoError(err)

	result, err := parseCursorIDEComposer(
		t.Context(), conn, dbPath,
		"corrupt-doc-0000-0000-000000000000", "devbox", info,
	)
	require.Error(err,
		"a composer row that no longer decodes must fail, not read as a clean no-session")
	assert.Nil(t, result)

	_, _, err = loadCursorIDEComposerMeta(
		t.Context(), conn, "corrupt-doc-0000-0000-000000000000",
	)
	require.Error(err,
		"the freshness meta load must not fingerprint a malformed composer as vanished")
}

func TestParseCursorIDEComposer_EndedAtNotBeforeLastMessage(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dbPath := createCursorIDEDB(t, []cursorIDETestComposer{{
		id:        "stale-stamp-0000-0000-000000000000",
		name:      "Stale stamp",
		createdAt: 1782026756842, // 2026-06-21T07:25:56.842Z
		updatedAt: 1782026791522, // 2026-06-21T07:26:31.522Z, before the bubbles
		bubbles: []cursorIDETestBubble{
			{id: "b1", bubbleType: cursorIDEBubbleTypeUser, text: "hi", createdAt: "2026-06-21T07:27:29.606Z"},
			{id: "b2", bubbleType: cursorIDEBubbleTypeAssistant, text: "reply", createdAt: "2026-06-21T07:28:05.000Z"},
		},
	}})
	conn, err := openCursorIDEDB(dbPath)
	require.NoError(err)
	defer conn.Close()
	info, err := os.Stat(dbPath)
	require.NoError(err)

	result, err := parseCursorIDEComposer(
		t.Context(), conn, dbPath,
		"stale-stamp-0000-0000-000000000000", "devbox", info,
	)
	require.NoError(err)
	require.NotNil(result)
	assert.Equal(time.Date(2026, 6, 21, 7, 28, 5, 0, time.UTC), result.Session.EndedAt,
		"a stale lastUpdatedAt must not place EndedAt before the final message")
	assert.False(result.Session.EndedAt.Before(result.Session.StartedAt))
}

func TestCursorIDEParseContainerKeepsSiblingsPastNullComposer(t *testing.T) {
	// NULL and a stored zero-length BLOB both scan into a zero-length []byte,
	// but they are distinct SQLite storage shapes (sql.ErrNoRows never fires
	// for either). The guard is len(raw) == 0, not raw == nil, precisely so a
	// database holding either shape stops failing; both cases must be proven
	// separately so a regression narrowing the predicate to raw == nil would
	// be caught.
	cases := []struct {
		name  string
		value any
	}{
		{name: "null", value: nil},
		{name: "empty-blob", value: []byte{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			dbPath := createCursorIDEDB(t, []cursorIDETestComposer{
				{
					id:        "husk-0000-0000-0000-000000000000",
					name:      "Husk chat",
					createdAt: 1782026756842,
					updatedAt: 1782026791522,
					bubbles: []cursorIDETestBubble{{
						id: "b1", bubbleType: cursorIDEBubbleTypeUser,
						text: "will be husked", createdAt: "2026-06-21T07:27:29.606Z",
					}},
				},
				{
					id:        "sibling-one-0000-0000-000000000000",
					name:      "Sibling one",
					createdAt: 1782026756842,
					updatedAt: 1782026791522,
					bubbles: []cursorIDETestBubble{{
						id: "b1", bubbleType: cursorIDEBubbleTypeUser,
						text: "kept one", createdAt: "2026-06-21T07:27:29.606Z",
					}},
				},
				{
					id:        "sibling-two-0000-0000-000000000000",
					name:      "Sibling two",
					createdAt: 1782026756842,
					updatedAt: 1782026791522,
					bubbles: []cursorIDETestBubble{{
						id: "b1", bubbleType: cursorIDEBubbleTypeUser,
						text: "kept two", createdAt: "2026-06-21T07:27:29.606Z",
					}},
				},
			})
			writer, err := sql.Open("sqlite3", dbPath)
			require.NoError(err)
			_, err = writer.ExecContext(t.Context(),
				`UPDATE cursorDiskKV SET value = ? WHERE key = ?`,
				tc.value, "composerData:husk-0000-0000-0000-000000000000",
			)
			require.NoError(err)
			require.NoError(writer.Close())
			assertCursorDiskKVStoredShape(t, dbPath,
				"composerData:husk-0000-0000-0000-000000000000", tc.value == nil)

			root := filepath.Dir(dbPath)
			provider, ok := NewProvider(AgentCursorIDE, ProviderConfig{
				Roots: []string{root}, Machine: "devbox",
			})
			require.True(ok)

			discovered, err := provider.Discover(t.Context())
			require.NoError(err)
			require.Len(discovered, 1)

			fingerprint, err := provider.Fingerprint(t.Context(), discovered[0])
			require.NoError(err)

			outcome, err := provider.Parse(t.Context(), ParseRequest{
				Source: discovered[0], Machine: "devbox", Fingerprint: fingerprint,
			})
			require.NoError(err,
				"a husk composer must not fail the whole container fan-out")
			require.Len(outcome.Results, 2,
				"both healthy siblings must survive past the husk composer")

			var ids []string
			var siblingOne ParseResult
			for _, r := range outcome.Results {
				ids = append(ids, r.Result.Session.ID)
				if r.Result.Session.ID == "cursor-ide:sibling-one-0000-0000-000000000000" {
					siblingOne = r.Result
				}
			}
			assert.ElementsMatch([]string{
				"cursor-ide:sibling-one-0000-0000-000000000000",
				"cursor-ide:sibling-two-0000-0000-000000000000",
			}, ids)
			assert.NotContains(ids, "cursor-ide:husk-0000-0000-0000-000000000000")

			// P3: a surviving sibling's full parsed-session metadata contract
			// (not just its ID) is unaffected by a sibling husk elsewhere in the
			// same container.
			require.Len(siblingOne.Messages, 1)
			assert.Equal("kept one", siblingOne.Messages[0].Content)
			assert.Equal("b1", siblingOne.Messages[0].SourceUUID)
			assert.False(siblingOne.Session.IsTruncated)
			assert.Equal("Sibling one", siblingOne.Session.SessionName)
			assert.NotEmpty(siblingOne.Session.File.Hash)
			assert.False(siblingOne.Session.StartedAt.IsZero())
			assert.False(siblingOne.Session.EndedAt.IsZero())
			// P7: cursor-ide assigns none of these lineage fields; a sibling
			// surviving a husk composer must not pick any of them up either.
			assert.Empty(siblingOne.Session.RelationshipType)
			assert.Empty(siblingOne.Session.SourceVersion)
			assert.Empty(siblingOne.Session.ParentSessionID)
			assert.Empty(siblingOne.Session.TerminationStatus)
		})
	}
}

func TestParseCursorIDEComposer_NullBubbleValueBecomesGap(t *testing.T) {
	// NULL and a stored zero-length BLOB are distinct storage shapes but must
	// take the same absent path; see the guard-boundary note in
	// TestCursorIDEParseContainerKeepsSiblingsPastNullComposer.
	cases := []struct {
		name  string
		value any
	}{
		{name: "null", value: nil},
		{name: "empty-blob", value: []byte{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			composer := []cursorIDETestComposer{{
				id:        "null-bubble-0000-0000-000000000000",
				name:      "Null bubble thread",
				createdAt: 1782026756842,
				updatedAt: 1782026791522,
				bubbles: []cursorIDETestBubble{
					{id: "b1", bubbleType: cursorIDEBubbleTypeUser, text: "kept", createdAt: "2026-06-21T07:27:29.606Z"},
					{id: "b2", bubbleType: cursorIDEBubbleTypeAssistant, text: "will be husked", createdAt: "2026-06-21T07:27:31.522Z"},
				},
			}}

			// Reference digest: the same composer with bubble b2 intact. This
			// is the one input for which cursorIDEComposerDigest becomes newly
			// reachable by this diff: on base, loadCursorIDEBubble errored at
			// header b2 and parseCursorIDEComposer returned before the digest
			// call. P6 requires the digest to hash every stored bubble's key
			// and value bytes unconditionally, so husking b2 must change it.
			intactPath := createCursorIDEDB(t, composer)
			intactConn, err := openCursorIDEDB(intactPath)
			require.NoError(err)
			intactInfo, err := os.Stat(intactPath)
			require.NoError(err)
			intactResult, err := parseCursorIDEComposer(
				t.Context(), intactConn, intactPath,
				"null-bubble-0000-0000-000000000000", "devbox", intactInfo,
			)
			require.NoError(err)
			require.NotNil(intactResult)
			require.NotEmpty(intactResult.Session.File.Hash)
			require.NoError(intactConn.Close())

			dbPath := createCursorIDEDB(t, composer)
			writer, err := sql.Open("sqlite3", dbPath)
			require.NoError(err)
			_, err = writer.ExecContext(t.Context(),
				`UPDATE cursorDiskKV SET value = ? WHERE key = ?`,
				tc.value, "bubbleId:null-bubble-0000-0000-000000000000:b2",
			)
			require.NoError(err)
			require.NoError(writer.Close())
			assertCursorDiskKVStoredShape(t, dbPath,
				"bubbleId:null-bubble-0000-0000-000000000000:b2", tc.value == nil)

			conn, err := openCursorIDEDB(dbPath)
			require.NoError(err)
			defer conn.Close()
			info, err := os.Stat(dbPath)
			require.NoError(err)

			result, err := parseCursorIDEComposer(
				t.Context(), conn, dbPath,
				"null-bubble-0000-0000-000000000000", "devbox", info,
			)
			require.NoError(err,
				"a bubble whose value is NULL or empty must become a truncation gap, not a fatal error")
			require.NotNil(result)
			require.Len(result.Messages, 1)
			assert.Equal("kept", result.Messages[0].Content)
			assert.True(result.Session.IsTruncated,
				"a transcript with a husked bubble row must be flagged truncated")
			assert.Zero(result.Session.MalformedLines,
				"this diff writes no malformed-line counter for a husk bubble")
			assert.Equal("b1", result.Messages[0].SourceUUID,
				"the surviving message's SourceUUID must be unchanged")

			assert.NotEmpty(result.Session.File.Hash)
			assert.NotEqual(intactResult.Session.File.Hash, result.Session.File.Hash,
				"husking bubble b2 must change the composer digest")
		})
	}
}

func TestCursorIDEEmptyJSONObjectValuesTakeExistingPaths(t *testing.T) {
	t.Run("composer", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)

		dbPath := createCursorIDEDB(t, []cursorIDETestComposer{{
			id: "empty-object-composer-0000-00000000",
		}})
		writer, err := sql.Open("sqlite3", dbPath)
		require.NoError(err)
		_, err = writer.ExecContext(t.Context(),
			`UPDATE cursorDiskKV SET value = ? WHERE key = ?`,
			[]byte(`{}`), "composerData:empty-object-composer-0000-00000000",
		)
		require.NoError(err)
		require.NoError(writer.Close())

		conn, err := openCursorIDEDB(dbPath)
		require.NoError(err)
		defer conn.Close()
		info, err := os.Stat(dbPath)
		require.NoError(err)

		result, err := parseCursorIDEComposer(
			t.Context(), conn, dbPath,
			"empty-object-composer-0000-00000000", "devbox", info,
		)
		require.NoError(err,
			"a two-byte {} value decodes cleanly and must not take the absent path")
		assert.Nil(result,
			"a composer with zero headers must not surface as a session")

		// parseCursorIDEComposer's (nil, nil) return is identical whether the
		// guard fired or the value decoded to zero headers, so it cannot by
		// itself prove {} took the decode path rather than the absent path. The
		// discriminator lives one level down: loadCursorIDEComposerMeta reports
		// ok=true with a real digest for a value that decoded (this {} case),
		// and ok=false with a zero meta for a husk (TestLoadCursorIDEComposerMetaNullValueReportsNotFound).
		// A guard broadened to catch len(raw) <= 2 would flip this ok to false,
		// which is exactly the false-positive this row exists to catch.
		meta, ok, err := loadCursorIDEComposerMeta(
			t.Context(), conn, "empty-object-composer-0000-00000000",
		)
		require.NoError(err)
		assert.True(ok,
			"a decoded {} composer must report found, unlike a husk composer")
		assert.NotEmpty(meta.digest,
			"a decoded {} composer must fingerprint with a real digest")
	})

	t.Run("bubble", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)

		dbPath := createCursorIDEDB(t, []cursorIDETestComposer{{
			id:        "empty-object-bubble-0000-0000000000",
			name:      "Empty object bubble",
			createdAt: 1782026756842,
			updatedAt: 1782026791522,
			bubbles: []cursorIDETestBubble{
				{id: "b1", bubbleType: cursorIDEBubbleTypeUser, text: "kept", createdAt: "2026-06-21T07:27:29.606Z"},
				{id: "b2", bubbleType: cursorIDEBubbleTypeAssistant, text: "will be emptied", createdAt: "2026-06-21T07:27:31.522Z"},
			},
		}})
		writer, err := sql.Open("sqlite3", dbPath)
		require.NoError(err)
		_, err = writer.ExecContext(t.Context(),
			`UPDATE cursorDiskKV SET value = ? WHERE key = ?`,
			[]byte(`{}`), "bubbleId:empty-object-bubble-0000-0000000000:b2",
		)
		require.NoError(err)
		require.NoError(writer.Close())

		conn, err := openCursorIDEDB(dbPath)
		require.NoError(err)
		defer conn.Close()
		info, err := os.Stat(dbPath)
		require.NoError(err)

		result, err := parseCursorIDEComposer(
			t.Context(), conn, dbPath,
			"empty-object-bubble-0000-0000000000", "devbox", info,
		)
		require.NoError(err,
			"a two-byte {} bubble value decodes cleanly and must not take the absent path")
		require.NotNil(result)
		require.Len(result.Messages, 1)
		assert.Equal("kept", result.Messages[0].Content)
		assert.True(result.Session.IsTruncated,
			"a bubble that decodes but renders nothing still takes the truncation path")

		// parseCursorIDEComposer's outer result (one surviving message,
		// truncated=true) is identical whether the {} bubble decoded and then
		// rendered nothing, or the row was an absent husk, so it cannot by
		// itself prove {} reached the renderless-bubble branch rather than the
		// absent-bubble branch. The discriminator is one level down, at
		// loadCursorIDEBubble itself: a decoded {} returns a non-nil bubble
		// (this case), while a husk returns nil
		// (TestParseCursorIDEComposer_NullBubbleValueBecomesGap). A guard
		// broadened to catch len(raw) <= 2 would flip this to nil, which is
		// exactly the false-positive this row exists to catch.
		bubble, err := loadCursorIDEBubble(
			t.Context(), conn,
			"empty-object-bubble-0000-0000000000", "b2",
		)
		require.NoError(err)
		require.NotNil(bubble,
			"a decoded {} bubble must return non-nil, unlike a husk bubble")
		assert.Equal(cursorIDEBubble{}, *bubble,
			"a {} bubble decodes to a zero-valued struct, not an absence")
	})
}

func TestLoadCursorIDEComposerMetaNullValueReportsNotFound(t *testing.T) {
	// NULL and a stored zero-length BLOB are distinct storage shapes but must
	// take the same absent path; see the guard-boundary note in
	// TestCursorIDEParseContainerKeepsSiblingsPastNullComposer.
	cases := []struct {
		name  string
		value any
	}{
		{name: "null", value: nil},
		{name: "empty-blob", value: []byte{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			dbPath := createCursorIDEDB(t, []cursorIDETestComposer{{
				id:        "null-meta-0000-0000-000000000000",
				name:      "Null meta chat",
				createdAt: 1782026756842,
				updatedAt: 1782026791522,
				bubbles: []cursorIDETestBubble{{
					id: "b1", bubbleType: cursorIDEBubbleTypeUser, text: "hi",
					createdAt: "2026-06-21T07:27:29.606Z",
				}},
			}})
			writer, err := sql.Open("sqlite3", dbPath)
			require.NoError(err)
			_, err = writer.ExecContext(t.Context(),
				`UPDATE cursorDiskKV SET value = ? WHERE key = ?`,
				tc.value, "composerData:null-meta-0000-0000-000000000000",
			)
			require.NoError(err)
			require.NoError(writer.Close())
			assertCursorDiskKVStoredShape(t, dbPath,
				"composerData:null-meta-0000-0000-000000000000", tc.value == nil)

			conn, err := openCursorIDEDB(dbPath)
			require.NoError(err)
			defer conn.Close()

			meta, ok, err := loadCursorIDEComposerMeta(
				t.Context(), conn, "null-meta-0000-0000-000000000000",
			)
			require.NoError(err,
				"a NULL or empty composer value must fingerprint as absent, not error")
			assert.False(ok)
			assert.Equal(cursorIDEComposerMeta{}, meta)

			assert.True(CursorIDEComposerExists(t.Context(), dbPath, "null-meta-0000-0000-000000000000"),
				"the key remains present in cursorDiskKV even though its value is NULL or empty")
		})
	}
}

func TestParseCursorIDEComposer_TypelessBubbleFallsBackToHeaderType(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dbPath := createCursorIDEDB(t, []cursorIDETestComposer{{
		id:        "typeless-0000-0000-0000-000000000000",
		name:      "Typeless bubble",
		createdAt: 1782026756842,
		updatedAt: 1782026791522,
		bubbles: []cursorIDETestBubble{
			{id: "b1", bubbleType: cursorIDEBubbleTypeUser, text: "ask", createdAt: "2026-06-21T07:27:29.606Z"},
			{id: "b2", bubbleType: cursorIDEBubbleTypeAssistant, text: "answer", createdAt: "2026-06-21T07:27:31.522Z"},
		},
	}})
	// Strip the type field from the assistant bubble's row, as a shrunk or
	// partially-written row would: the header still records the turn's role.
	writer, err := sql.Open("sqlite3", dbPath)
	require.NoError(err)
	_, err = writer.ExecContext(t.Context(),
		`UPDATE cursorDiskKV SET value = ? WHERE key = ?`,
		[]byte(`{"text": "answer", "createdAt": "2026-06-21T07:27:31.522Z"}`),
		"bubbleId:typeless-0000-0000-0000-000000000000:b2",
	)
	require.NoError(err)
	require.NoError(writer.Close())

	conn, err := openCursorIDEDB(dbPath)
	require.NoError(err)
	defer conn.Close()
	info, err := os.Stat(dbPath)
	require.NoError(err)

	result, err := parseCursorIDEComposer(
		t.Context(), conn, dbPath,
		"typeless-0000-0000-0000-000000000000", "devbox", info,
	)
	require.NoError(err)
	require.NotNil(result)
	require.Len(result.Messages, 2,
		"a bubble missing its type field must fall back to the header's role")
	assert.Equal(RoleUser, result.Messages[0].Role)
	assert.Equal(RoleAssistant, result.Messages[1].Role)
	assert.Equal("answer", result.Messages[1].Content)
}
