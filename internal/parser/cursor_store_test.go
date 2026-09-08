package parser

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	cursorStoreTestAgentID = "ac2369fb-93cd-4e41-9df4-65dac43045c5"
	cursorStoreTestUserMS  = uint64(1788818588927)
	cursorStoreTestAsstMS  = uint64(1788818591931)
	cursorStoreTestThinkMS = uint64(1788818591930)
)

type cursorStoreFixture struct {
	ProjectsRoot string
	ChatsRoot    string
	Transcript   string
	StorePath    string
	RootID       string
	Writer       *sql.DB
	Provider     Provider
	Source       SourceRef
}

func cursorStoreBlobRef(id string) []byte {
	b, err := hex.DecodeString(id)
	if err != nil || len(b) != 32 {
		panic("cursor store test blob id must be 64 hex chars")
	}
	return b
}

func cursorStoreHashID(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(h[:])
}

func cursorStoreEncodeUser(text string, ms uint64) []byte {
	return encodePB([]pbField{
		{num: 1, wire: pbWireBytes, bytes: []byte(text)},
		{num: 2, wire: pbWireBytes, bytes: []byte("3a4f6ea0-cec3-4164-9224-53337c595581")},
		{num: 4, wire: pbWireVarint, varint: 1},
		{num: 25, wire: pbWireVarint, varint: ms},
		{num: 26, wire: pbWireVarint, varint: ms},
	})
}

func cursorStoreEncodeReasoning(text string, ms uint64) []byte {
	inner := encodePB([]pbField{
		{num: 1, wire: pbWireBytes, bytes: []byte(text)},
		{num: 2, wire: pbWireVarint, varint: 1439},
		{num: 3, wire: pbWireVarint, varint: ms - 1000},
		{num: 4, wire: pbWireVarint, varint: ms},
	})
	return encodePB([]pbField{
		{num: 3, wire: pbWireBytes, bytes: inner},
	})
}

func cursorStoreEncodeAssistant(text string, ms uint64) []byte {
	inner := encodePB([]pbField{
		{num: 1, wire: pbWireBytes, bytes: []byte(text)},
		{num: 2, wire: pbWireVarint, varint: ms},
	})
	return encodePB([]pbField{
		{num: 1, wire: pbWireBytes, bytes: inner},
	})
}

func cursorStoreEncodeTurn(userID, reasoningID, assistantID string) []byte {
	fields := []pbField{
		{num: 1, wire: pbWireBytes, bytes: cursorStoreBlobRef(userID)},
	}
	if reasoningID != "" {
		fields = append(fields, pbField{
			num: 2, wire: pbWireBytes, bytes: cursorStoreBlobRef(reasoningID),
		})
	}
	if assistantID != "" {
		fields = append(fields, pbField{
			num: 2, wire: pbWireBytes, bytes: cursorStoreBlobRef(assistantID),
		})
	}
	fields = append(fields,
		pbField{num: 3, wire: pbWireBytes, bytes: []byte("6a670ffe-168a-4a4e-b2e3-bcf1ad472814")},
		pbField{num: 5, wire: pbWireVarint, varint: 3},
	)
	return encodePB(fields)
}

func cursorStoreEncodeTurnIndex(turnBlobIDs ...string) []byte {
	var fields []pbField
	for _, id := range turnBlobIDs {
		fields = append(fields, pbField{
			num: 1, wire: pbWireBytes, bytes: cursorStoreBlobRef(id),
		})
	}
	return encodePB(fields)
}

func cursorStoreEncodeRoot(turnIndexID string, extra ...pbField) []byte {
	fields := []pbField{
		{num: 8, wire: pbWireBytes, bytes: cursorStoreBlobRef(turnIndexID)},
		{num: 9, wire: pbWireBytes, bytes: []byte("file:///workspace")},
		{num: 22, wire: pbWireBytes, bytes: []byte("cli")},
		{num: 26, wire: pbWireVarint, varint: cursorStoreTestUserMS},
	}
	fields = append(fields, extra...)
	return encodePB(fields)
}

func writeCursorStoreMeta(t *testing.T, db *sql.DB, agentID, rootID string) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"agentId":          agentID,
		"latestRootBlobId": rootID,
		"name":             "Composer Model",
		"mode":             "default",
	})
	require.NoError(t, err)
	_, err = db.Exec(
		`INSERT INTO meta(key, value) VALUES(?, ?)`,
		"0", hex.EncodeToString(payload),
	)
	require.NoError(t, err)
}

func writeCursorStoreBlob(t *testing.T, db *sql.DB, id string, data []byte) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO blobs(id, data) VALUES(?, ?)`, id, data)
	require.NoError(t, err)
}

func openCursorStoreWriter(t *testing.T, storePath string) *sql.DB {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(storePath), 0o755))
	db, err := sql.Open("sqlite3", storePath+"?_journal_mode=WAL&_busy_timeout=3000")
	require.NoError(t, err)
	_, err = db.Exec(`PRAGMA journal_mode=WAL`)
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TABLE blobs (id TEXT PRIMARY KEY, data BLOB)`)
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT)`)
	require.NoError(t, err)
	return db
}

func cursorStoreTranscriptJSONL(user, assistant string) string {
	return strings.Join([]string{
		fmt.Sprintf(
			`{"role":"user","message":{"content":[{"type":"text","text":"<user_query>\n%s\n</user_query>"}]}}`,
			user,
		),
		fmt.Sprintf(
			`{"role":"assistant","message":{"content":[{"type":"text","text":%q}]}}`,
			assistant,
		),
		`{"type":"turn_ended","status":"success"}`,
	}, "\n") + "\n"
}

func setupCursorStoreFixture(t *testing.T, keepWriterOpen bool) cursorStoreFixture {
	t.Helper()
	home := t.TempDir()
	projects := filepath.Join(home, ".cursor", "projects")
	chats := filepath.Join(home, ".cursor", "chats")
	projectDir := "Users-demo-Code-app"
	agentID := cursorStoreTestAgentID
	transcriptDir := filepath.Join(projects, projectDir, "agent-transcripts", agentID)
	require.NoError(t, os.MkdirAll(transcriptDir, 0o755))
	transcript := filepath.Join(transcriptDir, agentID+".jsonl")
	userText := "is this composer? what model is this?"
	assistantText := "I'm Auto, an agent router designed by Cursor."
	require.NoError(t, os.WriteFile(
		transcript,
		[]byte(cursorStoreTranscriptJSONL(userText, assistantText)),
		0o644,
	))

	wsHash := "ac6bc72e07173e02c3524eb823d07ad5"
	storePath := filepath.Join(chats, wsHash, agentID, "store.db")
	writer := openCursorStoreWriter(t, storePath)

	userID := cursorStoreHashID("user", userText)
	reasoningID := cursorStoreHashID("reasoning", "think")
	assistantID := cursorStoreHashID("assistant", assistantText)
	turnID := cursorStoreHashID("turn", userID, reasoningID, assistantID)
	turnIndexID := cursorStoreHashID("turn-index", turnID)
	rootID := cursorStoreHashID("root", turnIndexID)
	orphanRootID := cursorStoreHashID("orphan-root")

	writeCursorStoreBlob(t, writer, userID, cursorStoreEncodeUser(userText, cursorStoreTestUserMS))
	writeCursorStoreBlob(t, writer, reasoningID, cursorStoreEncodeReasoning(
		"The user is asking who I am and what model I am.", cursorStoreTestThinkMS,
	))
	writeCursorStoreBlob(t, writer, assistantID, cursorStoreEncodeAssistant(
		assistantText, cursorStoreTestAsstMS,
	))
	writeCursorStoreBlob(t, writer, turnID, cursorStoreEncodeTurn(userID, reasoningID, assistantID))
	writeCursorStoreBlob(t, writer, turnIndexID, cursorStoreEncodeTurnIndex(turnID))
	writeCursorStoreBlob(t, writer, rootID, cursorStoreEncodeRoot(turnIndexID))
	// Superseded root carries a different assistant message that must not surface.
	orphanUser := cursorStoreHashID("orphan-user")
	orphanAsst := cursorStoreHashID("orphan-asst")
	orphanTurn := cursorStoreHashID("orphan-turn")
	orphanIndex := cursorStoreHashID("orphan-index")
	writeCursorStoreBlob(t, writer, orphanUser, cursorStoreEncodeUser("stale prompt", cursorStoreTestUserMS))
	writeCursorStoreBlob(t, writer, orphanAsst, cursorStoreEncodeAssistant(
		"STALE ROOT CONTENT MUST NOT APPEAR", cursorStoreTestAsstMS,
	))
	writeCursorStoreBlob(t, writer, orphanTurn, cursorStoreEncodeTurn(orphanUser, "", orphanAsst))
	writeCursorStoreBlob(t, writer, orphanIndex, cursorStoreEncodeTurnIndex(orphanTurn))
	writeCursorStoreBlob(t, writer, orphanRootID, cursorStoreEncodeRoot(orphanIndex))
	writeCursorStoreMeta(t, writer, agentID, rootID)

	if !keepWriterOpen {
		require.NoError(t, writer.Close())
		writer = nil
	} else {
		t.Cleanup(func() { _ = writer.Close() })
	}

	provider, ok := NewProvider(AgentCursor, ProviderConfig{
		Roots:   []string{projects},
		Machine: "devbox",
		MetadataDirs: map[string][]string{
			filepath.Clean(projects): {filepath.Clean(chats)},
		},
	})
	require.True(t, ok)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)

	return cursorStoreFixture{
		ProjectsRoot: projects,
		ChatsRoot:    chats,
		Transcript:   transcript,
		StorePath:    storePath,
		RootID:       rootID,
		Writer:       writer,
		Provider:     provider,
		Source:       sources[0],
	}
}

func TestCursorStoreParsesRowsHeldOnlyInWAL(t *testing.T) {
	fx := setupCursorStoreFixture(t, true)
	walInfo, err := os.Stat(fx.StorePath + "-wal")
	require.NoError(t, err)
	assert.Positive(t, walInfo.Size())

	outcome, err := fx.Provider.Parse(context.Background(), ParseRequest{Source: fx.Source})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	msgs := outcome.Results[0].Result.Messages
	require.Len(t, msgs, 2)
	assert.True(t, msgs[1].HasThinking)
	assert.Contains(t, msgs[1].ThinkingText, "asking who I am")
	assert.Equal(t, "cursor:"+cursorStoreTestAgentID, outcome.Results[0].Result.Session.ID)
}

func TestCursorStoreTraversesOnlyLatestRootBlobId(t *testing.T) {
	fx := setupCursorStoreFixture(t, false)
	outcome, err := fx.Provider.Parse(context.Background(), ParseRequest{Source: fx.Source})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	raw, err := json.Marshal(outcome.Results[0].Result.Messages)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "STALE ROOT CONTENT MUST NOT APPEAR")
	assert.Contains(t, outcome.Results[0].Result.Messages[1].Content, "Auto")
}

func TestCursorStoreEnrichesExistingTranscript(t *testing.T) {
	fx := setupCursorStoreFixture(t, false)
	discovered, err := fx.Provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, fx.Transcript, discovered[0].DisplayPath)

	outcome, err := fx.Provider.Parse(context.Background(), ParseRequest{Source: fx.Source})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	result := outcome.Results[0].Result
	assert.Equal(t, "cursor:"+cursorStoreTestAgentID, result.Session.ID)
	require.Len(t, result.Messages, 2)
	assert.Equal(t, RoleUser, result.Messages[0].Role)
	assert.Equal(t, RoleAssistant, result.Messages[1].Role)
	assert.True(t, result.Messages[1].HasThinking)
	assert.Equal(
		t,
		time.UnixMilli(int64(cursorStoreTestUserMS)).UTC(),
		result.Messages[0].Timestamp.UTC(),
	)
	assert.Equal(
		t,
		time.UnixMilli(int64(cursorStoreTestAsstMS)).UTC(),
		result.Messages[1].Timestamp.UTC(),
	)
	assert.Empty(t, result.Session.SourceVersion)
}

func TestCursorStoreUsesTurnIndexProjection(t *testing.T) {
	fx := setupCursorStoreFixture(t, false)
	outcome, err := fx.Provider.Parse(context.Background(), ParseRequest{Source: fx.Source})
	require.NoError(t, err)
	msgs := outcome.Results[0].Result.Messages
	require.Len(t, msgs, 2)
	assert.Equal(t, "is this composer? what model is this?", msgs[0].Content)
	assert.Contains(t, msgs[1].Content, "Auto")
	assert.Contains(t, msgs[1].ThinkingText, "asking who I am")
	assert.Equal(t, 2, outcome.Results[0].Result.Session.MessageCount)
	joined := msgs[0].Content + msgs[1].Content + msgs[1].ThinkingText
	assert.NotContains(t, joined, `"role":"system"`)
	assert.NotContains(t, strings.ToLower(joined), "model-context")
}

func TestCursorStoreSkipsUndecodableBlobWithoutDroppingSiblings(t *testing.T) {
	fx := setupCursorStoreFixture(t, false)
	db, err := sql.Open("sqlite3", fx.StorePath)
	require.NoError(t, err)
	defer db.Close()

	userText := "is this composer? what model is this?"
	assistantText := "I'm Auto, an agent router designed by Cursor."
	userID := cursorStoreHashID("user", userText)
	reasoningID := cursorStoreHashID("reasoning", "think")
	assistantID := cursorStoreHashID("assistant", assistantText)
	junkID := cursorStoreHashID("junk-undecodable")
	writeCursorStoreBlob(t, db, junkID, []byte{0xff, 0x00, 0x01, 0x02, 0x03})

	turnID := cursorStoreHashID("turn", userID, reasoningID, assistantID)
	turnData := encodePB([]pbField{
		{num: 1, wire: pbWireBytes, bytes: cursorStoreBlobRef(userID)},
		{num: 2, wire: pbWireBytes, bytes: cursorStoreBlobRef(reasoningID)},
		{num: 2, wire: pbWireBytes, bytes: cursorStoreBlobRef(junkID)},
		{num: 2, wire: pbWireBytes, bytes: cursorStoreBlobRef(assistantID)},
		{num: 3, wire: pbWireBytes, bytes: []byte("6a670ffe-168a-4a4e-b2e3-bcf1ad472814")},
		{num: 5, wire: pbWireVarint, varint: 3},
	})
	_, err = db.Exec(`UPDATE blobs SET data = ? WHERE id = ?`, turnData, turnID)
	require.NoError(t, err)

	outcome, err := fx.Provider.Parse(context.Background(), ParseRequest{Source: fx.Source})
	require.NoError(t, err)
	require.Len(t, outcome.Results[0].Result.Messages, 2)
	assert.True(t, outcome.Results[0].Result.Messages[1].HasThinking)
	assert.Contains(t, outcome.Results[0].Result.Messages[1].Content, "Auto")
}

func TestCursorStoreResolvesInline32ByteMessage(t *testing.T) {
	fx := setupCursorStoreFixture(t, false)
	db, err := openCursorIDEDB(fx.StorePath)
	require.NoError(t, err)
	defer db.Close()

	text := strings.Repeat("i", 28)
	message := encodePB([]pbField{{num: 1, wire: pbWireBytes, bytes: []byte(text)}})
	payload := encodePB([]pbField{{num: 1, wire: pbWireBytes, bytes: message}})
	require.Len(t, payload, 32)
	loader := newCursorStoreBlobLoader(
		context.Background(), db,
	)
	resolved, ok := cursorStoreResolveMessage(agProtoField{
		Wire: pbWireBytes, Bytes: payload,
	}, loader)
	require.True(t, ok)
	_, _, ok = decodeCursorStoreAssistantMessage(resolved)
	assert.True(t, ok)
}

func assertCursorStoreTranscriptOnly(t *testing.T, outcome ParseOutcome) {
	t.Helper()
	assert.Empty(t, outcome.SourceErrors)
	assert.True(t, outcome.ResultSetComplete)
	require.Len(t, outcome.Results, 1)
	result := outcome.Results[0].Result
	assert.Equal(t, "cursor:"+cursorStoreTestAgentID, result.Session.ID)
	require.Len(t, result.Messages, 2)
	assert.Equal(t, "is this composer? what model is this?", result.Messages[0].Content)
	assert.Equal(t, "I'm Auto, an agent router designed by Cursor.", result.Messages[1].Content)
	assert.False(t, result.Messages[1].HasThinking)
	assert.Empty(t, result.Messages[1].ThinkingText)
}

func TestCursorStoreMalformedMetaKeepsTranscript(t *testing.T) {
	fx := setupCursorStoreFixture(t, false)
	db, err := sql.Open("sqlite3", fx.StorePath)
	require.NoError(t, err)
	defer db.Close()
	_, err = db.Exec(`UPDATE meta SET value = ? WHERE key = '0'`, "not-hex")
	require.NoError(t, err)

	outcome, err := fx.Provider.Parse(context.Background(), ParseRequest{Source: fx.Source})
	require.NoError(t, err)
	assertCursorStoreTranscriptOnly(t, outcome)
}

func TestCursorStoreMissingMetadataKeepsTranscript(t *testing.T) {
	fx := setupCursorStoreFixture(t, true)
	_, err := fx.Writer.Exec(`DELETE FROM meta WHERE key = '0'`)
	require.NoError(t, err)

	outcome, err := fx.Provider.Parse(context.Background(), ParseRequest{Source: fx.Source})
	require.NoError(t, err)
	assertCursorStoreTranscriptOnly(t, outcome)
}

func TestCursorStoreIncompleteMetadataKeepsTranscript(t *testing.T) {
	fx := setupCursorStoreFixture(t, true)
	payload, err := json.Marshal(map[string]any{
		"agentId": cursorStoreTestAgentID,
	})
	require.NoError(t, err)
	_, err = fx.Writer.Exec(
		`UPDATE meta SET value = ? WHERE key = '0'`,
		hex.EncodeToString(payload),
	)
	require.NoError(t, err)

	outcome, err := fx.Provider.Parse(context.Background(), ParseRequest{Source: fx.Source})
	require.NoError(t, err)
	assertCursorStoreTranscriptOnly(t, outcome)
}

func TestCursorStoreMetadataAgentMismatchKeepsTranscript(t *testing.T) {
	fx := setupCursorStoreFixture(t, true)
	payload, err := json.Marshal(map[string]any{
		"agentId":          "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
		"latestRootBlobId": fx.RootID,
	})
	require.NoError(t, err)
	_, err = fx.Writer.Exec(
		`UPDATE meta SET value = ? WHERE key = '0'`,
		hex.EncodeToString(payload),
	)
	require.NoError(t, err)

	outcome, err := fx.Provider.Parse(context.Background(), ParseRequest{Source: fx.Source})
	require.NoError(t, err)
	assertCursorStoreTranscriptOnly(t, outcome)
}

func TestCursorStoreMissingSelectedRootKeepsTranscript(t *testing.T) {
	fx := setupCursorStoreFixture(t, true)
	missingRoot := strings.Repeat("f", 64)
	payload, err := json.Marshal(map[string]any{
		"agentId":          cursorStoreTestAgentID,
		"latestRootBlobId": missingRoot,
	})
	require.NoError(t, err)
	_, err = fx.Writer.Exec(
		`UPDATE meta SET value = ? WHERE key = '0'`,
		hex.EncodeToString(payload),
	)
	require.NoError(t, err)

	outcome, err := fx.Provider.Parse(context.Background(), ParseRequest{Source: fx.Source})
	require.NoError(t, err)
	assertCursorStoreTranscriptOnly(t, outcome)
}

func TestCursorStoreUnsupportedRootKeepsTranscript(t *testing.T) {
	for _, tt := range []struct {
		name string
		root []byte
	}{
		{name: "undecodable root", root: []byte{0xff}},
		{name: "missing turn index", root: encodePB([]pbField{
			{num: 22, wire: pbWireBytes, bytes: []byte("cli")},
		})},
		{name: "undecodable turn index", root: encodePB([]pbField{
			{num: 8, wire: pbWireBytes, bytes: []byte{0xff}},
		})},
		{name: "no decoded turns", root: encodePB([]pbField{
			{num: 8, wire: pbWireBytes, bytes: encodePB([]pbField{
				{num: 1, wire: pbWireBytes, bytes: []byte{0xff}},
			})},
		})},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fx := setupCursorStoreFixture(t, true)
			_, err := fx.Writer.Exec(`UPDATE blobs SET data = ? WHERE id = ?`, tt.root, fx.RootID)
			require.NoError(t, err)
			outcome, err := fx.Provider.Parse(t.Context(), ParseRequest{Source: fx.Source})
			require.NoError(t, err)
			assertCursorStoreTranscriptOnly(t, outcome)
		})
	}
}

func TestCursorStoreIndexFailureKeepsTranscripts(t *testing.T) {
	for _, failure := range []string{"chats is a file", "unreadable workspace"} {
		t.Run(failure, func(t *testing.T) {
			fx := setupCursorStoreFixture(t, false)
			switch failure {
			case "chats is a file":
				require.NoError(t, os.Rename(fx.ChatsRoot, fx.ChatsRoot+"-saved"))
				require.NoError(t, os.WriteFile(fx.ChatsRoot, []byte("file"), 0o644))
			case "unreadable workspace":
				workspace := filepath.Dir(filepath.Dir(fx.StorePath))
				require.NoError(t, os.Chmod(workspace, 0))
				t.Cleanup(func() { require.NoError(t, os.Chmod(workspace, 0o755)) })
				if _, err := os.ReadDir(workspace); err == nil {
					t.Skip("directory permissions are not enforced")
				}
			}
			provider := fx.Provider.(*cursorProvider)
			logs := captureLog(t)
			for _, operation := range []string{"Discover", "DiscoverEach", "Fingerprint", "Parse"} {
				t.Run(operation, func(t *testing.T) {
					provider.sources.storeIndex = newCursorStoreIndex()
					switch operation {
					case "Discover":
						sources, err := provider.Discover(t.Context())
						require.NoError(t, err)
						require.Len(t, sources, 1)
						assert.Equal(t, fx.Transcript, sources[0].DisplayPath)
					case "DiscoverEach":
						var paths []string
						err := provider.DiscoverEach(t.Context(), func(source SourceRef) error {
							paths = append(paths, source.DisplayPath)
							return nil
						})
						require.NoError(t, err)
						assert.Equal(t, []string{fx.Transcript}, paths)
					case "Fingerprint":
						fp, err := provider.Fingerprint(t.Context(), fx.Source)
						require.NoError(t, err)
						assert.NotEmpty(t, fp.Hash)
						assert.NotContains(t, fp.Hash, "|store:")
					case "Parse":
						outcome, err := provider.Parse(t.Context(), ParseRequest{Source: fx.Source})
						require.NoError(t, err)
						assertCursorStoreTranscriptOnly(t, outcome)
					}
				})
			}
			assertLogContains(t, logs, "warning", "Cursor store index")
		})
	}
}

func TestCursorStoreUnreadableMatchingStoreReturnsError(t *testing.T) {
	fx := setupCursorStoreFixture(t, false)
	require.NoError(t, os.WriteFile(fx.StorePath, []byte("not a sqlite database"), 0o644))
	for _, suffix := range []string{"-wal", "-shm"} {
		_ = os.Remove(fx.StorePath + suffix)
	}

	outcome, err := fx.Provider.Parse(context.Background(), ParseRequest{Source: fx.Source})
	require.NoError(t, err)
	assert.Empty(t, outcome.Results)
	require.Len(t, outcome.SourceErrors, 1)
	assert.True(t, outcome.ResultSetComplete)
	assert.True(t, outcome.SourceErrors[0].Retryable)
	assert.Contains(t, outcome.SourceErrors[0].Err.Error(), "cursor store")
}

func TestCursorStoreEnrichesSourceWithoutOpaque(t *testing.T) {
	fx := setupCursorStoreFixture(t, false)
	source := fx.Source
	source.Opaque = nil

	outcome, err := fx.Provider.Parse(context.Background(), ParseRequest{Source: source})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	assert.Empty(t, outcome.SourceErrors)
	assert.True(t, outcome.Results[0].Result.Messages[1].HasThinking)
}

func TestCursorStoreEnrichesTargetedLookupWithoutDiscovery(t *testing.T) {
	fx := setupCursorStoreFixture(t, false)
	provider, ok := NewProvider(AgentCursor, ProviderConfig{
		Roots: []string{fx.ProjectsRoot},
		MetadataDirs: map[string][]string{
			filepath.Clean(fx.ProjectsRoot): {filepath.Clean(fx.ChatsRoot)},
		},
	})
	require.True(t, ok)

	source, found, err := provider.FindSource(context.Background(), FindSourceRequest{
		StoredFilePath: fx.Transcript,
	})
	require.NoError(t, err)
	require.True(t, found)
	fingerprint, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source:      source,
		Fingerprint: fingerprint,
	})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	assert.True(t, outcome.Results[0].Result.Messages[1].HasThinking)
}

func TestCursorStoreSkipsTurnWithoutUserToPreserveAlignment(t *testing.T) {
	firstAssistant := time.UnixMilli(1_788_818_602_000).UTC()
	secondUser := time.UnixMilli(1_788_818_603_000).UTC()
	secondAssistant := time.UnixMilli(1_788_818_604_000).UTC()
	msgs := []ParsedMessage{
		{Role: RoleUser, Content: "first question"},
		{Role: RoleAssistant, Content: "first answer"},
		{Role: RoleUser, Content: "second question"},
		{Role: RoleAssistant, Content: "second answer"},
	}
	sess := ParsedSession{}
	applyCursorStoreTurns(&sess, msgs, []cursorStoreTurn{
		{
			AssistantTime: firstAssistant,
			AssistantText: "first answer",
			ReasoningText: "orphaned reasoning",
			ReasoningTime: firstAssistant,
		},
		{
			UserDecoded:      true,
			AssistantDecoded: true,
			UserText:         "second question",
			AssistantText:    "second answer",
			UserTime:         secondUser,
			AssistantTime:    secondAssistant,
			ReasoningText:    "aligned reasoning",
			ReasoningTime:    secondAssistant,
		},
	})

	assert.Empty(t, msgs[0].Timestamp)
	assert.Empty(t, msgs[1].Timestamp)
	assert.Empty(t, msgs[1].ThinkingText)
	assert.Equal(t, secondUser, msgs[2].Timestamp)
	assert.Equal(t, secondAssistant, msgs[3].Timestamp)
	assert.Equal(t, "aligned reasoning", msgs[3].ThinkingText)
}

func TestCursorStoreSkipsTurnWithoutAssistantToPreserveAlignment(t *testing.T) {
	firstUser := time.UnixMilli(1_788_818_601_000).UTC()
	secondUser := time.UnixMilli(1_788_818_603_000).UTC()
	secondAssistant := time.UnixMilli(1_788_818_604_000).UTC()
	msgs := []ParsedMessage{
		{Role: RoleUser, Content: "first question"},
		{Role: RoleAssistant, Content: "first answer"},
		{Role: RoleUser, Content: "second question"},
		{Role: RoleAssistant, Content: "second answer"},
	}
	sess := ParsedSession{}
	applyCursorStoreTurns(&sess, msgs, []cursorStoreTurn{
		{
			UserDecoded: true,
			UserText:    "first question",
			UserTime:    firstUser,
		},
		{
			UserDecoded:      true,
			AssistantDecoded: true,
			UserText:         "second question",
			AssistantText:    "second answer",
			UserTime:         secondUser,
			AssistantTime:    secondAssistant,
			ReasoningText:    "aligned reasoning",
			ReasoningTime:    secondAssistant,
		},
	})

	assert.Empty(t, msgs[0].Timestamp)
	assert.Empty(t, msgs[1].Timestamp)
	assert.Empty(t, msgs[1].ThinkingText)
	assert.Equal(t, secondUser, msgs[2].Timestamp)
	assert.Equal(t, secondAssistant, msgs[3].Timestamp)
	assert.Equal(t, "aligned reasoning", msgs[3].ThinkingText)
}

func TestCursorStoreLeavesLaterTurnUnmappedAfterUndecodableTurn(t *testing.T) {
	msgs := []ParsedMessage{
		{Role: RoleUser, Content: "first question"},
		{Role: RoleAssistant, Content: "first answer"},
		{Role: RoleUser, Content: "second question"},
		{Role: RoleAssistant, Content: "second answer"},
	}
	sess := ParsedSession{}
	applyCursorStoreTurns(&sess, msgs, []cursorStoreTurn{
		{},
		{
			UserDecoded:      true,
			AssistantDecoded: true,
			UserText:         "second question",
			AssistantText:    "second answer",
			UserTime:         time.UnixMilli(1_788_818_603_000).UTC(),
			AssistantTime:    time.UnixMilli(1_788_818_604_000).UTC(),
			ReasoningText:    "ambiguous reasoning",
		},
	})

	for _, msg := range msgs {
		assert.Empty(t, msg.Timestamp)
		assert.Empty(t, msg.ThinkingText)
	}
}

func TestCursorStoreMatchesFinalAssistantAfterIntermediateMessage(t *testing.T) {
	msgs := []ParsedMessage{
		{Role: RoleUser, Content: "first question"},
		{Role: RoleAssistant, Content: "intermediate note"},
		{Role: RoleAssistant, Content: "first answer"},
		{Role: RoleUser, Content: "second question"},
		{Role: RoleAssistant, Content: "second answer"},
	}
	sess := ParsedSession{}
	applyCursorStoreTurns(&sess, msgs, []cursorStoreTurn{
		{
			UserDecoded:      true,
			AssistantDecoded: true,
			UserText:         "first question",
			AssistantText:    "first answer",
			ReasoningText:    "first reasoning",
		},
		{
			UserDecoded:      true,
			AssistantDecoded: true,
			UserText:         "second question",
			AssistantText:    "second answer",
			ReasoningText:    "second reasoning",
		},
	})

	assert.Empty(t, msgs[1].ThinkingText)
	assert.Equal(t, "first reasoning", msgs[2].ThinkingText)
	assert.Equal(t, "second reasoning", msgs[4].ThinkingText)
}

func TestCursorStoreUsesProducerMillisecondTimes(t *testing.T) {
	fx := setupCursorStoreFixture(t, false)
	old := time.Now().Add(-48 * time.Hour)
	require.NoError(t, os.Chtimes(fx.Transcript, old, old))

	outcome, err := fx.Provider.Parse(context.Background(), ParseRequest{Source: fx.Source})
	require.NoError(t, err)
	sess := outcome.Results[0].Result.Session
	wantStart := time.UnixMilli(int64(cursorStoreTestUserMS)).UTC()
	wantEnd := time.UnixMilli(int64(cursorStoreTestAsstMS)).UTC()
	assert.Equal(t, wantStart, sess.StartedAt.UTC())
	assert.Equal(t, wantEnd, sess.EndedAt.UTC())
	assert.NotEqual(t, old.UTC().Truncate(time.Second), sess.StartedAt.UTC().Truncate(time.Second))
}

func TestCursorStorePartialTurnIndexRetainsTranscriptEndBound(t *testing.T) {
	fx := setupCursorStoreFixture(t, false)
	secondTranscript := cursorStoreTranscriptJSONL(
		"a later question", "a later answer",
	)
	require.NoError(t, os.WriteFile(
		fx.Transcript,
		[]byte(cursorStoreTranscriptJSONL(
			"is this composer? what model is this?",
			"I'm Auto, an agent router designed by Cursor.",
		)+secondTranscript),
		0o644,
	))
	transcriptTime := time.UnixMilli(int64(cursorStoreTestAsstMS) + 10_000).UTC()
	require.NoError(t, os.Chtimes(fx.Transcript, transcriptTime, transcriptTime))

	outcome, err := fx.Provider.Parse(context.Background(), ParseRequest{Source: fx.Source})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	sess := outcome.Results[0].Result.Session
	assert.Equal(t, transcriptTime, sess.EndedAt.UTC())
	assert.GreaterOrEqual(t, sess.EndedAt.UnixMilli(), int64(cursorStoreTestAsstMS))
}

func TestCursorStoreExtraAssistantRetainsTranscriptEndBound(t *testing.T) {
	fx := setupCursorStoreFixture(t, false)
	transcript := cursorStoreTranscriptJSONL(
		"is this composer? what model is this?",
		"I'm Auto, an agent router designed by Cursor.",
	)
	transcript += fmt.Sprintf(
		`{"role":"assistant","message":{"content":[{"type":"text","text":%q}]}}`+"\n",
		"a later assistant message",
	)
	require.NoError(t, os.WriteFile(fx.Transcript, []byte(transcript), 0o644))
	transcriptTime := time.UnixMilli(int64(cursorStoreTestAsstMS) + 30_000).UTC()
	require.NoError(t, os.Chtimes(fx.Transcript, transcriptTime, transcriptTime))

	outcome, err := fx.Provider.Parse(context.Background(), ParseRequest{Source: fx.Source})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	assert.Equal(t, transcriptTime, outcome.Results[0].Result.Session.EndedAt.UTC())
}

func TestCursorStoreIncompleteTurnTimesRetainTranscriptEndBound(t *testing.T) {
	fx := setupCursorStoreFixture(t, false)
	assistantID := cursorStoreHashID(
		"assistant", "I'm Auto, an agent router designed by Cursor.",
	)
	db, err := sql.Open("sqlite3", fx.StorePath)
	require.NoError(t, err)
	defer db.Close()
	_, err = db.Exec(
		`UPDATE blobs SET data = ? WHERE id = ?`,
		cursorStoreEncodeAssistant(
			"I'm Auto, an agent router designed by Cursor.", 0,
		), assistantID,
	)
	require.NoError(t, err)
	transcriptTime := time.UnixMilli(int64(cursorStoreTestAsstMS) + 20_000).UTC()
	require.NoError(t, os.Chtimes(fx.Transcript, transcriptTime, transcriptTime))

	outcome, err := fx.Provider.Parse(context.Background(), ParseRequest{Source: fx.Source})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	assert.Equal(t, transcriptTime, outcome.Results[0].Result.Session.EndedAt.UTC())
}

func TestCursorStoreDoesNotTreatUntimedFieldThreeAsReasoning(t *testing.T) {
	inner := encodePB([]pbField{{num: 1, wire: pbWireBytes, bytes: []byte("ambiguous")}})
	fields, err := agProtoParse(encodePB([]pbField{{
		num: 3, wire: pbWireBytes, bytes: inner,
	}}))
	require.NoError(t, err)
	text, _, ok := decodeCursorStoreReasoning(fields)
	assert.False(t, ok)
	assert.Empty(t, text)
}

func TestCursorStoreWatchPlanIgnoresSHM(t *testing.T) {
	fx := setupCursorStoreFixture(t, false)
	plan, err := fx.Provider.WatchPlan(context.Background())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 2)
	var chatsRoot WatchRoot
	found := false
	for _, root := range plan.Roots {
		if samePath(root.Path, fx.ChatsRoot) {
			chatsRoot = root
			found = true
		}
	}
	require.True(t, found)
	changed, err := fx.Provider.SourcesForChangedPath(context.Background(), ChangedPathRequest{
		Path: fx.StorePath + "-shm", EventKind: "write", WatchRoot: chatsRoot.Path,
	})
	require.NoError(t, err)
	assert.Empty(t, changed)
}

func TestCursorStoreChangedPathUsesTranscriptSource(t *testing.T) {
	fx := setupCursorStoreFixture(t, false)

	changed, err := fx.Provider.SourcesForChangedPath(context.Background(), ChangedPathRequest{
		Path: fx.StorePath, EventKind: "write", WatchRoot: fx.ChatsRoot,
	})
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, fx.Transcript, changed[0].DisplayPath)

	walChanged, err := fx.Provider.SourcesForChangedPath(context.Background(), ChangedPathRequest{
		Path: fx.StorePath + "-wal", EventKind: "write", WatchRoot: fx.ChatsRoot,
	})
	require.NoError(t, err)
	require.Len(t, walChanged, 1)
	assert.Equal(t, fx.Transcript, walChanged[0].DisplayPath)

	storeOnly := filepath.Join(fx.ChatsRoot, "deadbeefdeadbeefdeadbeefdeadbeef", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", "store.db")
	require.NoError(t, os.MkdirAll(filepath.Dir(storeOnly), 0o755))
	require.NoError(t, os.WriteFile(storeOnly, []byte("x"), 0o644))
	none, err := fx.Provider.SourcesForChangedPath(context.Background(), ChangedPathRequest{
		Path: storeOnly, EventKind: "write", WatchRoot: fx.ChatsRoot,
	})
	require.NoError(t, err)
	assert.Empty(t, none)
}

func TestCursorStoreChangedPathFindsTranscriptBeforeDiscovery(t *testing.T) {
	fx := setupCursorStoreFixture(t, false)
	provider, ok := NewProvider(AgentCursor, ProviderConfig{
		Roots: []string{fx.ProjectsRoot},
		MetadataDirs: map[string][]string{
			filepath.Clean(fx.ProjectsRoot): {filepath.Clean(fx.ChatsRoot)},
		},
	})
	require.True(t, ok)

	changed, err := provider.SourcesForChangedPath(context.Background(), ChangedPathRequest{
		Path: fx.StorePath, EventKind: "write", WatchRoot: fx.ChatsRoot,
	})
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, fx.Transcript, changed[0].DisplayPath)
}

func TestCursorStoreIndexKeepsSiblingStoresVisible(t *testing.T) {
	fx := setupCursorStoreFixture(t, false)
	otherAgentID := "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	provider := fx.Provider.(*cursorProvider)
	path := provider.sources.storePathForRawID(fx.ProjectsRoot, otherAgentID)
	assert.Empty(t, path)
	otherStore := filepath.Join(
		fx.ChatsRoot, "deadbeefdeadbeefdeadbeefdeadbeef", otherAgentID, "store.db",
	)
	require.NoError(t, os.MkdirAll(filepath.Dir(otherStore), 0o755))
	require.NoError(t, os.WriteFile(otherStore, []byte("store"), 0o644))
	provider.sources.storeIndex.refresh(fx.ChatsRoot)

	path = provider.sources.storePathForRawID(fx.ProjectsRoot, otherAgentID)
	assert.Equal(t, otherStore, path)
}

func TestCursorStoreIndexRetainsLastCompleteScanOnRefreshError(t *testing.T) {
	fx := setupCursorStoreFixture(t, false)
	workspace := filepath.Join(fx.ChatsRoot, "unreadable-workspace")
	require.NoError(t, os.Mkdir(workspace, 0))
	t.Cleanup(func() { require.NoError(t, os.Chmod(workspace, 0o755)) })
	if _, err := os.ReadDir(workspace); err == nil {
		t.Skip("directory permissions are not enforced")
	}

	sources, err := fx.Provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	outcome, err := fx.Provider.Parse(t.Context(), ParseRequest{Source: sources[0]})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	require.Len(t, outcome.Results[0].Result.Messages, 2)
	assert.Contains(t, outcome.Results[0].Result.Messages[1].ThinkingText, "asking who I am")
}

func TestCursorStoreRemovalKeepsTranscript(t *testing.T) {
	fx := setupCursorStoreFixture(t, false)
	require.NoError(t, os.Remove(fx.StorePath))
	_ = os.Remove(fx.StorePath + "-wal")
	_ = os.Remove(fx.StorePath + "-shm")

	discovered, err := fx.Provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, fx.Transcript, discovered[0].DisplayPath)

	outcome, err := fx.Provider.Parse(context.Background(), ParseRequest{Source: discovered[0]})
	require.NoError(t, err)
	require.Len(t, outcome.Results[0].Result.Messages, 2)
	assert.False(t, outcome.Results[0].Result.Messages[1].HasThinking)
	assert.Equal(t, "", outcome.Results[0].Result.Session.SourceVersion)
}

func TestCursorStoreOpensReadOnly(t *testing.T) {
	fx := setupCursorStoreFixture(t, false)
	conn, err := openCursorIDEDB(fx.StorePath)
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.Exec(`CREATE TABLE should_fail (id INTEGER)`)
	require.Error(t, err)
}

func TestCursorStoreDoesNotDiscoverStoreOnlySession(t *testing.T) {
	fx := setupCursorStoreFixture(t, false)
	storeOnlyID := "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	storeOnly := filepath.Join(fx.ChatsRoot, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", storeOnlyID, "store.db")
	require.NoError(t, os.MkdirAll(filepath.Dir(storeOnly), 0o755))
	require.NoError(t, os.WriteFile(storeOnly, []byte("not a real store"), 0o644))

	discovered, err := fx.Provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, fx.Transcript, discovered[0].DisplayPath)
	for _, src := range discovered {
		assert.NotContains(t, src.DisplayPath, storeOnlyID)
		assert.NotContains(t, src.Key, "store.db")
	}
}

func TestCursorStoreResolveMetadataDir(t *testing.T) {
	home := t.TempDir()
	projects := filepath.Join(home, ".cursor", "projects")
	chats := filepath.Join(home, ".cursor", "chats")
	require.NoError(t, os.MkdirAll(projects, 0o755))
	require.NoError(t, os.MkdirAll(chats, 0o755))
	factory, ok := ProviderFactoryByType(AgentCursor)
	require.True(t, ok)
	resolver, ok := factory.(interface {
		ResolveMetadataDir(string) (string, error)
	})
	require.True(t, ok)
	got, err := resolver.ResolveMetadataDir(projects)
	require.NoError(t, err)
	want, err := filepath.EvalSymlinks(chats)
	require.NoError(t, err)
	got, err = filepath.EvalSymlinks(got)
	require.NoError(t, err)
	assert.Equal(t, want, got)

	unrelated := filepath.Join(home, "other")
	require.NoError(t, os.MkdirAll(unrelated, 0o755))
	empty, err := resolver.ResolveMetadataDir(unrelated)
	require.NoError(t, err)
	assert.Empty(t, empty)
}

func TestCursorStoreFingerprintTracksWAL(t *testing.T) {
	fx := setupCursorStoreFixture(t, true)
	fp1, err := fx.Provider.Fingerprint(context.Background(), fx.Source)
	require.NoError(t, err)
	require.Contains(t, fp1.Hash, "|store:")

	_, err = fx.Writer.Exec(
		`INSERT INTO blobs(id, data) VALUES(?, ?)`,
		cursorStoreHashID("extra-wal-row"), []byte("x"),
	)
	require.NoError(t, err)
	fp2, err := fx.Provider.Fingerprint(context.Background(), fx.Source)
	require.NoError(t, err)
	assert.NotEqual(t, fp1.Hash, fp2.Hash)
}

func TestCursorStoreFingerprintSurvivesStoreRemoval(t *testing.T) {
	fx := setupCursorStoreFixture(t, false)
	_, err := fx.Provider.Fingerprint(context.Background(), fx.Source)
	require.NoError(t, err)
	require.NoError(t, os.Remove(fx.StorePath))
	_ = os.Remove(fx.StorePath + "-wal")
	_ = os.Remove(fx.StorePath + "-shm")

	fingerprint, err := fx.Provider.Fingerprint(context.Background(), fx.Source)
	require.NoError(t, err)
	assert.NotContains(t, fingerprint.Hash, "|store:")
}

func TestCursorStoreParseCarriesFingerprintMtime(t *testing.T) {
	fx := setupCursorStoreFixture(t, true)
	fingerprint, err := fx.Provider.Fingerprint(context.Background(), fx.Source)
	require.NoError(t, err)

	outcome, err := fx.Provider.Parse(context.Background(), ParseRequest{
		Source:      fx.Source,
		Fingerprint: fingerprint,
	})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	assert.Equal(t, fingerprint.MTimeNS, outcome.Results[0].Result.Session.File.Mtime)
	assert.Equal(t, fingerprint.Size, outcome.Results[0].Result.Session.File.Size)
}
