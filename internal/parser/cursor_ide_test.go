// ABOUTME: Tests for the Cursor IDE (GUI) parser: cursorDiskKV composer/bubble
// ABOUTME: decoding, message ordering, tool calls, and provider source methods.
package parser

import (
	"context"
	"database/sql"
	"encoding/json"
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

	_, err = db.Exec(
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
		_, err = db.Exec(
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
			_, err = db.Exec(
				`INSERT INTO cursorDiskKV (key, value) VALUES (?, ?)`,
				cursorIDEBubbleKeyPrefix+c.id+":"+b.id, raw,
			)
			require.NoError(t, err)
		}
	}
	return dbPath
}

func TestCursorIDEProviderCapabilities(t *testing.T) {
	factory, ok := ProviderFactoryByType(AgentCursorIDE)
	require.True(t, ok)
	caps := factory.Capabilities()
	assert.Equal(t, CapabilitySupported, caps.Content.ToolCalls)
	assert.Equal(t, CapabilitySupported, caps.Content.ToolResults)

	provider, ok := NewProvider(AgentCursorIDE, ProviderConfig{
		Roots: []string{t.TempDir()}, Machine: "devbox",
	})
	require.True(t, ok)
	require.NotNil(t, provider)
}

func TestCursorIDEProviderDiscoverAndParse(t *testing.T) {
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
						Result:     `{"directories":[{"absPath":"/Users/alice/dev/dark-factory","files":[]}]}`,
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
	require.True(t, ok)

	plan, err := provider.WatchPlan(context.Background())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 1)
	assert.Equal(t, root, plan.Roots[0].Path)
	assert.False(t, plan.Roots[0].Recursive)
	assert.Equal(t, []string{"state.vscdb", "state.vscdb-*"}, plan.Roots[0].IncludeGlobs)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, AgentCursorIDE, discovered[0].Provider)
	assert.Equal(t, dbPath, discovered[0].DisplayPath)

	fingerprint, err := provider.Fingerprint(context.Background(), discovered[0])
	require.NoError(t, err)
	require.NotZero(t, fingerprint.MTimeNS)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source: discovered[0], Machine: "devbox", Fingerprint: fingerprint,
	})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1, "the empty draft composer must not surface as a session")

	sess := outcome.Results[0].Result.Session
	messages := outcome.Results[0].Result.Messages
	assert.Equal(t, "cursor-ide:0cd7922b-f080-4672-b26f-2d521feee055", sess.ID)
	assert.Equal(t, AgentCursorIDE, sess.Agent)
	assert.Equal(t, "Factory course status", sess.SessionName)
	assert.Equal(t, "/Users/alice/dev/dark-factory", sess.Cwd)
	assert.Equal(t, "dark_factory", sess.Project)
	assert.Equal(t, "dev", sess.GitBranch)
	assert.Equal(t, 1, sess.UserMessageCount)
	assert.Equal(t, "give me an overview on the development status", sess.FirstMessage)

	// composerData createdAt/lastUpdatedAt are epoch milliseconds; the parser
	// must not confuse them with the bubbles' ISO-8601 createdAt encoding.
	// This fixture's lastUpdatedAt (07:26:31.522Z) lags the final bubble, so
	// EndedAt comes from the latest message timestamp instead.
	assert.Equal(t, time.UnixMilli(1782026756842).UTC(), sess.StartedAt)
	assert.Equal(t,
		time.Date(2026, 6, 21, 7, 27, 32, 0, time.UTC), sess.EndedAt)
	// Both encodings describe the same real conversation, so they must land
	// within the same window rather than merely both parsing without error.
	assert.WithinDuration(t, sess.StartedAt, sess.EndedAt, 2*time.Minute)

	require.Len(t, messages, 3)
	assert.Equal(t, RoleUser, messages[0].Role)
	assert.Equal(t, "give me an overview on the development status", messages[0].Content)
	assert.Equal(t, RoleAssistant, messages[1].Role)
	assert.Equal(t, "I'll inspect the factory-course package structure.", messages[1].Content)
	assert.Equal(t, RoleAssistant, messages[2].Role)
	assert.True(t, messages[2].HasToolUse)
	require.Len(t, messages[2].ToolCalls, 1)
	assert.Equal(t, "glob_file_search", messages[2].ToolCalls[0].ToolName)
	assert.Contains(t, messages[2].ToolCalls[0].InputJSON, "globPattern")
	require.Len(t, messages[2].ToolResults, 1)
	assert.Contains(t, messages[2].ToolResults[0].ContentRaw, "absPath")

	// Ordering must come from fullConversationHeadersOnly, not from a
	// lexicographic bubble-key scan.
	for i, m := range messages {
		assert.Equal(t, i, m.Ordinal)
	}
}

func TestCursorIDEFindSourceAndFingerprint(t *testing.T) {
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
	require.True(t, ok)

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		FullSessionID: "devbox~cursor-ide:" + composerID,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, virtualPath, found.DisplayPath)

	fingerprint, err := provider.Fingerprint(context.Background(), found)
	require.NoError(t, err)
	assert.Equal(t, virtualPath, fingerprint.Key)
	assert.Positive(t, fingerprint.Size)
	assert.NotZero(t, fingerprint.MTimeNS)
	assert.NotEmpty(t, fingerprint.Hash)

	// A vanished composer (row deleted, DB file still present) fingerprints
	// as keyed-empty rather than erroring, so Parse runs and force-replaces
	// the deleted session out of the archive.
	ghost := found
	if src, ok := ghost.Opaque.(multiSessionSource); ok {
		src.MemberID = "does-not-exist"
		src.Path = VirtualSourcePath(dbPath, "does-not-exist")
		ghost.Opaque = src
	}
	ghostFingerprint, err := provider.Fingerprint(context.Background(), ghost)
	require.NoError(t, err)
	assert.Empty(t, ghostFingerprint.Hash)
}

func TestParseCursorIDEComposer_ToleratesMissingBubbleRow(t *testing.T) {
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
	require.NoError(t, err)
	defer conn.Close()
	info, err := os.Stat(dbPath)
	require.NoError(t, err)

	result, err := parseCursorIDEComposer(
		context.Background(), conn, dbPath,
		"gap-0000-0000-0000-000000000000", "devbox", info,
	)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Len(t, result.Messages, 1)
	assert.Equal(t, "hello", result.Messages[0].Content)
	assert.True(t, result.Session.IsTruncated,
		"a transcript with a missing bubble row must be flagged truncated")
}

func TestParseCursorIDEComposer_EmptyComposerSkipped(t *testing.T) {
	dbPath := createCursorIDEDB(t, []cursorIDETestComposer{{
		id: "empty-0000-0000-0000-000000000000",
	}})
	conn, err := openCursorIDEDB(dbPath)
	require.NoError(t, err)
	defer conn.Close()
	info, err := os.Stat(dbPath)
	require.NoError(t, err)

	result, err := parseCursorIDEComposer(
		context.Background(), conn, dbPath,
		"empty-0000-0000-0000-000000000000", "devbox", info,
	)
	require.NoError(t, err)
	assert.Nil(t, result)
}

func TestParseCursorIDEComposer_MalformedBubbleFailsInsteadOfTruncating(t *testing.T) {
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
	require.NoError(t, err)
	_, err = writer.Exec(
		`UPDATE cursorDiskKV SET value = ? WHERE key = ?`,
		[]byte(`{"type": "not-an-int"`),
		"bubbleId:corrupt-bubble-0000-0000-000000000000:b2",
	)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	conn, err := openCursorIDEDB(dbPath)
	require.NoError(t, err)
	defer conn.Close()
	info, err := os.Stat(dbPath)
	require.NoError(t, err)

	result, err := parseCursorIDEComposer(
		context.Background(), conn, dbPath,
		"corrupt-bubble-0000-0000-000000000000", "devbox", info,
	)
	require.Error(t, err,
		"a stored bubble that no longer decodes must fail, not truncate the transcript")
	assert.Nil(t, result)
}

func TestParseCursorIDEComposer_MalformedComposerFailsInsteadOfRetiring(t *testing.T) {
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
	require.NoError(t, err)
	_, err = writer.Exec(
		`UPDATE cursorDiskKV SET value = ? WHERE key = ?`,
		[]byte(`{"fullConversationHeadersOnly": [truncated`),
		"composerData:corrupt-doc-0000-0000-000000000000",
	)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	conn, err := openCursorIDEDB(dbPath)
	require.NoError(t, err)
	defer conn.Close()
	info, err := os.Stat(dbPath)
	require.NoError(t, err)

	result, err := parseCursorIDEComposer(
		context.Background(), conn, dbPath,
		"corrupt-doc-0000-0000-000000000000", "devbox", info,
	)
	require.Error(t, err,
		"a composer row that no longer decodes must fail, not read as a clean no-session")
	assert.Nil(t, result)

	_, _, err = loadCursorIDEComposerMeta(
		context.Background(), conn, "corrupt-doc-0000-0000-000000000000",
	)
	require.Error(t, err,
		"the freshness meta load must not fingerprint a malformed composer as vanished")
}

func TestParseCursorIDEComposer_EndedAtNotBeforeLastMessage(t *testing.T) {
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
	require.NoError(t, err)
	defer conn.Close()
	info, err := os.Stat(dbPath)
	require.NoError(t, err)

	result, err := parseCursorIDEComposer(
		context.Background(), conn, dbPath,
		"stale-stamp-0000-0000-000000000000", "devbox", info,
	)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t,
		time.Date(2026, 6, 21, 7, 28, 5, 0, time.UTC), result.Session.EndedAt,
		"a stale lastUpdatedAt must not place EndedAt before the final message")
	assert.False(t, result.Session.EndedAt.Before(result.Session.StartedAt))
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
			require.NoError(t, err)
			_, err = writer.Exec(
				`UPDATE cursorDiskKV SET value = ? WHERE key = ?`,
				tc.value, "composerData:husk-0000-0000-0000-000000000000",
			)
			require.NoError(t, err)
			require.NoError(t, writer.Close())

			root := filepath.Dir(dbPath)
			provider, ok := NewProvider(AgentCursorIDE, ProviderConfig{
				Roots: []string{root}, Machine: "devbox",
			})
			require.True(t, ok)

			discovered, err := provider.Discover(context.Background())
			require.NoError(t, err)
			require.Len(t, discovered, 1)

			fingerprint, err := provider.Fingerprint(context.Background(), discovered[0])
			require.NoError(t, err)

			outcome, err := provider.Parse(context.Background(), ParseRequest{
				Source: discovered[0], Machine: "devbox", Fingerprint: fingerprint,
			})
			require.NoError(t, err,
				"a husk composer must not fail the whole container fan-out")
			require.Len(t, outcome.Results, 2,
				"both healthy siblings must survive past the husk composer")

			var ids []string
			var siblingOne ParseResult
			for _, r := range outcome.Results {
				ids = append(ids, r.Result.Session.ID)
				if r.Result.Session.ID == "cursor-ide:sibling-one-0000-0000-000000000000" {
					siblingOne = r.Result
				}
			}
			assert.ElementsMatch(t, []string{
				"cursor-ide:sibling-one-0000-0000-000000000000",
				"cursor-ide:sibling-two-0000-0000-000000000000",
			}, ids)
			assert.NotContains(t, ids, "cursor-ide:husk-0000-0000-0000-000000000000")

			// P3: a surviving sibling's full parsed-session metadata contract
			// (not just its ID) is unaffected by a sibling husk elsewhere in the
			// same container.
			require.Len(t, siblingOne.Messages, 1)
			assert.Equal(t, "kept one", siblingOne.Messages[0].Content)
			assert.Equal(t, "b1", siblingOne.Messages[0].SourceUUID)
			assert.False(t, siblingOne.Session.IsTruncated)
			assert.Equal(t, "Sibling one", siblingOne.Session.SessionName)
			assert.NotEmpty(t, siblingOne.Session.File.Hash)
			assert.False(t, siblingOne.Session.StartedAt.IsZero())
			assert.False(t, siblingOne.Session.EndedAt.IsZero())
			// P7: cursor-ide assigns none of these lineage fields; a sibling
			// surviving a husk composer must not pick any of them up either.
			assert.Empty(t, siblingOne.Session.RelationshipType)
			assert.Empty(t, siblingOne.Session.SourceVersion)
			assert.Empty(t, siblingOne.Session.ParentSessionID)
			assert.Empty(t, siblingOne.Session.TerminationStatus)
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
			dbPath := createCursorIDEDB(t, []cursorIDETestComposer{{
				id:        "null-bubble-0000-0000-000000000000",
				name:      "Null bubble thread",
				createdAt: 1782026756842,
				updatedAt: 1782026791522,
				bubbles: []cursorIDETestBubble{
					{id: "b1", bubbleType: cursorIDEBubbleTypeUser, text: "kept", createdAt: "2026-06-21T07:27:29.606Z"},
					{id: "b2", bubbleType: cursorIDEBubbleTypeAssistant, text: "will be husked", createdAt: "2026-06-21T07:27:31.522Z"},
				},
			}})
			writer, err := sql.Open("sqlite3", dbPath)
			require.NoError(t, err)
			_, err = writer.Exec(
				`UPDATE cursorDiskKV SET value = ? WHERE key = ?`,
				tc.value, "bubbleId:null-bubble-0000-0000-000000000000:b2",
			)
			require.NoError(t, err)
			require.NoError(t, writer.Close())

			conn, err := openCursorIDEDB(dbPath)
			require.NoError(t, err)
			defer conn.Close()
			info, err := os.Stat(dbPath)
			require.NoError(t, err)

			result, err := parseCursorIDEComposer(
				context.Background(), conn, dbPath,
				"null-bubble-0000-0000-000000000000", "devbox", info,
			)
			require.NoError(t, err,
				"a bubble whose value is NULL or empty must become a truncation gap, not a fatal error")
			require.NotNil(t, result)
			require.Len(t, result.Messages, 1)
			assert.Equal(t, "kept", result.Messages[0].Content)
			assert.True(t, result.Session.IsTruncated,
				"a transcript with a husked bubble row must be flagged truncated")
			assert.Zero(t, result.Session.MalformedLines,
				"this diff writes no malformed-line counter for a husk bubble")
			assert.Equal(t, "b1", result.Messages[0].SourceUUID,
				"the surviving message's SourceUUID must be unchanged")
		})
	}
}

func TestCursorIDEEmptyJSONObjectValuesTakeExistingPaths(t *testing.T) {
	t.Run("composer", func(t *testing.T) {
		dbPath := createCursorIDEDB(t, []cursorIDETestComposer{{
			id: "empty-object-composer-0000-00000000",
		}})
		writer, err := sql.Open("sqlite3", dbPath)
		require.NoError(t, err)
		_, err = writer.Exec(
			`UPDATE cursorDiskKV SET value = ? WHERE key = ?`,
			[]byte(`{}`), "composerData:empty-object-composer-0000-00000000",
		)
		require.NoError(t, err)
		require.NoError(t, writer.Close())

		conn, err := openCursorIDEDB(dbPath)
		require.NoError(t, err)
		defer conn.Close()
		info, err := os.Stat(dbPath)
		require.NoError(t, err)

		result, err := parseCursorIDEComposer(
			context.Background(), conn, dbPath,
			"empty-object-composer-0000-00000000", "devbox", info,
		)
		require.NoError(t, err,
			"a two-byte {} value decodes cleanly and must not take the absent path")
		assert.Nil(t, result,
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
			context.Background(), conn, "empty-object-composer-0000-00000000",
		)
		require.NoError(t, err)
		assert.True(t, ok,
			"a decoded {} composer must report found, unlike a husk composer")
		assert.NotEmpty(t, meta.digest,
			"a decoded {} composer must fingerprint with a real digest")
	})

	t.Run("bubble", func(t *testing.T) {
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
		require.NoError(t, err)
		_, err = writer.Exec(
			`UPDATE cursorDiskKV SET value = ? WHERE key = ?`,
			[]byte(`{}`), "bubbleId:empty-object-bubble-0000-0000000000:b2",
		)
		require.NoError(t, err)
		require.NoError(t, writer.Close())

		conn, err := openCursorIDEDB(dbPath)
		require.NoError(t, err)
		defer conn.Close()
		info, err := os.Stat(dbPath)
		require.NoError(t, err)

		result, err := parseCursorIDEComposer(
			context.Background(), conn, dbPath,
			"empty-object-bubble-0000-0000000000", "devbox", info,
		)
		require.NoError(t, err,
			"a two-byte {} bubble value decodes cleanly and must not take the absent path")
		require.NotNil(t, result)
		require.Len(t, result.Messages, 1)
		assert.Equal(t, "kept", result.Messages[0].Content)
		assert.True(t, result.Session.IsTruncated,
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
			context.Background(), conn,
			"empty-object-bubble-0000-0000000000", "b2",
		)
		require.NoError(t, err)
		require.NotNil(t, bubble,
			"a decoded {} bubble must return non-nil, unlike a husk bubble")
		assert.Equal(t, cursorIDEBubble{}, *bubble,
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
			require.NoError(t, err)
			_, err = writer.Exec(
				`UPDATE cursorDiskKV SET value = ? WHERE key = ?`,
				tc.value, "composerData:null-meta-0000-0000-000000000000",
			)
			require.NoError(t, err)
			require.NoError(t, writer.Close())

			conn, err := openCursorIDEDB(dbPath)
			require.NoError(t, err)
			defer conn.Close()

			meta, ok, err := loadCursorIDEComposerMeta(
				context.Background(), conn, "null-meta-0000-0000-000000000000",
			)
			require.NoError(t, err,
				"a NULL or empty composer value must fingerprint as absent, not error")
			assert.False(t, ok)
			assert.Equal(t, cursorIDEComposerMeta{}, meta)

			assert.True(t,
				CursorIDEComposerExists(dbPath, "null-meta-0000-0000-000000000000"),
				"the key remains present in cursorDiskKV even though its value is NULL or empty")
		})
	}
}

func TestParseCursorIDEComposer_TypelessBubbleFallsBackToHeaderType(t *testing.T) {
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
	require.NoError(t, err)
	_, err = writer.Exec(
		`UPDATE cursorDiskKV SET value = ? WHERE key = ?`,
		[]byte(`{"text": "answer", "createdAt": "2026-06-21T07:27:31.522Z"}`),
		"bubbleId:typeless-0000-0000-0000-000000000000:b2",
	)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	conn, err := openCursorIDEDB(dbPath)
	require.NoError(t, err)
	defer conn.Close()
	info, err := os.Stat(dbPath)
	require.NoError(t, err)

	result, err := parseCursorIDEComposer(
		context.Background(), conn, dbPath,
		"typeless-0000-0000-0000-000000000000", "devbox", info,
	)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Len(t, result.Messages, 2,
		"a bubble missing its type field must fall back to the header's role")
	assert.Equal(t, RoleUser, result.Messages[0].Role)
	assert.Equal(t, RoleAssistant, result.Messages[1].Role)
	assert.Equal(t, "answer", result.Messages[1].Content)
}
