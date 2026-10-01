package parser

import (
	"context"
	"database/sql"
	"encoding/json"
	"hash/fnv"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func fnv64aHex(raw []byte) string {
	h := fnv.New64a()
	_, _ = h.Write(raw)
	return strconv.FormatUint(h.Sum64(), 16)
}

func execCursorIDEDB(t *testing.T, dbPath, query string, args ...any) {
	t.Helper()
	conn, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.ExecContext(t.Context(), query, args...)
	require.NoError(t, err)
}

func readCursorIDEValue(t *testing.T, dbPath, key string) []byte {
	t.Helper()
	conn, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	defer conn.Close()
	var raw []byte
	require.NoError(t, conn.QueryRowContext(t.Context(),
		`SELECT value FROM cursorDiskKV WHERE key = ?`, key,
	).Scan(&raw))
	return raw
}

type listedCursorIDEToken struct {
	id    string
	token string
}

func listCursorIDETokensForTest(t *testing.T, dbPath string) []listedCursorIDEToken {
	t.Helper()
	conn, err := openCursorIDEDB(dbPath)
	require.NoError(t, err)
	defer conn.Close()
	var listed []listedCursorIDEToken
	require.NoError(t, listCursorIDEComposerTokens(t.Context(), conn,
		func(id, token string) error {
			listed = append(listed, listedCursorIDEToken{id: id, token: token})
			return nil
		}))
	return listed
}

func TestListCursorIDEComposerTokens(t *testing.T) {
	dbPath := createCursorIDEDB(t, []cursorIDETestComposer{{
		id:        "a-blob",
		updatedAt: 1782026791522,
		bubbles: []cursorIDETestBubble{
			{id: "b1", bubbleType: cursorIDEBubbleTypeUser, text: "hello"},
		},
	}})
	textDoc := `{"fullConversationHeadersOnly":[{"bubbleId":"b1","type":1}],"name":"no stamp"}`
	lastDoc := `{"fullConversationHeadersOnly":[{"bubbleId":"b2","type":2}],"lastUpdatedAt":5}`
	execCursorIDEDB(t, dbPath, `INSERT INTO cursorDiskKV (key, value) VALUES
		('composerData:c-text', ?),
		('composerData:d-empty', '{"fullConversationHeadersOnly":[]}'),
		('composerData:e-null', NULL),
		('composerData:f-bad', '{not json'),
		('composerData:g bad!', ?),
		('composerData:h-last', ?)`,
		textDoc, textDoc, lastDoc,
	)
	blobRaw := readCursorIDEValue(t, dbPath, "composerData:a-blob")

	parses, digests := CursorIDEComposerParses(), CursorIDEComposerDigests()
	listed := listCursorIDETokensForTest(t, dbPath)
	assert.Equal(t, []listedCursorIDEToken{
		{id: "a-blob", token: fnv64aHex(blobRaw)},
		{id: "c-text", token: fnv64aHex([]byte(textDoc))},
		{id: "h-last", token: fnv64aHex([]byte(lastDoc))},
	}, listed)
	assert.Equal(t, parses, CursorIDEComposerParses())
	assert.Equal(t, digests, CursorIDEComposerDigests())
}

func TestCursorIDEComposerHashFormat(t *testing.T) {
	composerID := "hash-0000-0000-0000-000000000000"
	dbPath := createCursorIDEDB(t, []cursorIDETestComposer{{
		id:        composerID,
		updatedAt: 1782026791522,
		bubbles: []cursorIDETestBubble{
			{id: "b1", bubbleType: cursorIDEBubbleTypeUser, text: "hello"},
		},
	}})
	provider, ok := NewProvider(AgentCursorIDE, ProviderConfig{
		Roots: []string{filepath.Dir(dbPath)}, Machine: "test",
	})
	require.True(t, ok)
	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		FullSessionID: "test~cursor-ide:" + composerID,
	})
	require.NoError(t, err)
	require.True(t, ok)
	fingerprint, err := provider.Fingerprint(t.Context(), found)
	require.NoError(t, err)
	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source: found, Machine: "test", Fingerprint: fingerprint,
	})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	hash := outcome.Results[0].Result.Session.File.Hash

	parts := strings.Split(hash, ":")
	require.Len(t, parts, 3)
	assert.Equal(t, "cide1", parts[0])
	assert.NotEmpty(t, parts[2])
	listed := listCursorIDETokensForTest(t, dbPath)
	require.Len(t, listed, 1)
	assert.Equal(t, listed[0].token, parts[1])
	assert.Equal(t, hash, fingerprint.Hash)

	token, ok := cursorIDEStoredComposerToken(hash)
	assert.True(t, ok)
	assert.Equal(t, parts[1], token)
	for _, bad := range []string{"0123abcd", "cide2:a:b", "cide1::b", "cide1:a", ""} {
		_, ok := cursorIDEStoredComposerToken(bad)
		assert.False(t, ok, bad)
	}
}

func TestCursorIDEChangedPathListsOnlyChangedComposers(t *testing.T) {
	composers := []cursorIDETestComposer{
		{id: "one-0000", updatedAt: 1782026791522, bubbles: []cursorIDETestBubble{
			{id: "u1", bubbleType: cursorIDEBubbleTypeUser, text: "first"},
		}},
		{id: "two-0000", updatedAt: 1782026791522, bubbles: []cursorIDETestBubble{
			{id: "u1", bubbleType: cursorIDEBubbleTypeUser, text: "second"},
		}},
	}
	dbPath := createCursorIDEDB(t, composers)
	root := filepath.Dir(dbPath)
	provider, ok := NewProvider(AgentCursorIDE, ProviderConfig{
		Roots: []string{root}, Machine: "test",
	})
	require.True(t, ok)
	assert.Equal(t, CapabilitySupported,
		provider.Capabilities().Source.StoredMemberFreshnessListing)
	container, ok := ResolveStoredMemberFreshnessContainer(provider, dbPath)
	require.True(t, ok)
	assert.Equal(t, dbPath, container)

	discovered, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	fingerprint, err := provider.Fingerprint(t.Context(), discovered[0])
	require.NoError(t, err)
	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source: discovered[0], Machine: "test", Fingerprint: fingerprint,
	})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 2)
	var stored []StoredMemberFreshness
	for _, entry := range outcome.Results {
		file := entry.Result.Session.File
		stored = append(stored, StoredMemberFreshness{Path: file.Path, FingerprintHash: file.Hash})
	}
	sort.Slice(stored, func(i, j int) bool { return stored[i].Path < stored[j].Path })
	pager := func(
		_ context.Context, afterPath string, limit int,
	) ([]StoredMemberFreshness, bool, error) {
		var page []StoredMemberFreshness
		for _, row := range stored {
			if row.Path > afterPath && len(page) < limit {
				page = append(page, row)
			}
		}
		return page, true, nil
	}
	req := ChangedPathRequest{
		Path:                      dbPath,
		EventKind:                 "write",
		AllowWatermarkOnlySources: true,
		StoredMemberFreshnessPage: pager,
	}
	sources, err := provider.SourcesForChangedPath(t.Context(), req)
	require.NoError(t, err)
	assert.Empty(t, sources)

	// Append a bubble and its header, leaving lastUpdatedAt untouched.
	doc := cursorIDEComposerDoc{
		Headers: []cursorIDEComposerHeader{
			{BubbleID: "u1", Type: cursorIDEBubbleTypeUser},
			{BubbleID: "a2", Type: cursorIDEBubbleTypeAssistant},
		},
		LastUpdatedAt: 1782026791522,
	}
	raw, err := json.Marshal(doc)
	require.NoError(t, err)
	bubble, err := json.Marshal(cursorIDEBubble{Type: cursorIDEBubbleTypeAssistant, Text: "reply"})
	require.NoError(t, err)
	execCursorIDEDB(t, dbPath, `INSERT INTO cursorDiskKV (key, value) VALUES (?, ?), (?, ?)`,
		cursorIDEComposerKeyPrefix+"two-0000", raw,
		cursorIDEBubbleKeyPrefix+"two-0000:a2", bubble,
	)

	sources, err = provider.SourcesForChangedPath(t.Context(), req)
	require.NoError(t, err)
	assert.Equal(t, []string{VirtualSourcePath(dbPath, "two-0000")}, sourceKeys(sources))

	// A listing that cannot read the file keeps the whole-container source.
	t.Run("missing cursorDiskKV table", func(t *testing.T) {
		execCursorIDEDB(t, dbPath, `DROP TABLE cursorDiskKV`)
		sources, err := provider.SourcesForChangedPath(t.Context(), req)
		require.NoError(t, err)
		assert.Equal(t, []string{dbPath}, sourceKeys(sources))
	})
	t.Run("not a SQLite file", func(t *testing.T) {
		require.NoError(t, os.WriteFile(dbPath, []byte("not a database"), 0o644))
		sources, err := provider.SourcesForChangedPath(t.Context(), req)
		require.NoError(t, err)
		assert.Equal(t, []string{dbPath}, sourceKeys(sources))
	})
}
