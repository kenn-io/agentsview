package db

import (
	"bytes"
	"database/sql"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestContainsCJK(t *testing.T) {
	assert := assert.New(t)

	t.Parallel()
	assert.True(containsCJK("SQLite 中文搜索"))
	assert.True(containsCJK("日本語"))
	assert.True(containsCJK("한국어"))
	assert.False(containsCJK("get_views error-401"))
}

func TestDiscoverSimpleFTSRuntimeFrom(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dir := t.TempDir()
	require.NoError(os.WriteFile(
		filepath.Join(dir, "libsimple.so"), []byte("library"), 0o600,
	))
	dictDir := filepath.Join(dir, "dict")
	require.NoError(os.Mkdir(dictDir, 0o700))
	for _, name := range []string{
		"hmm_model.utf8",
		"idf.utf8",
		"jieba.dict.utf8",
		"stop_words.utf8",
		"user.dict.utf8",
	} {
		require.NoError(os.WriteFile(
			filepath.Join(dictDir, name), []byte(name), 0o600,
		))
	}

	got, err := discoverSimpleFTSRuntimeFrom(
		filepath.Join(t.TempDir(), "agentsview"), dir, "linux",
	)
	require.NoError(err)
	assert.Equal(filepath.Join(dir, "libsimple.so"), got.libraryPath)
	assert.Equal(dictDir, got.dictionaryPath)
	assert.NotEmpty(got.fingerprint)

	originalFingerprint := got.fingerprint
	require.NoError(os.WriteFile(
		filepath.Join(dictDir, "user.dict.utf8"), []byte("changed"), 0o600,
	))
	changed, err := discoverSimpleFTSRuntimeFrom(
		filepath.Join(t.TempDir(), "agentsview"), dir, "linux",
	)
	require.NoError(err)
	assert.NotEqual(originalFingerprint, changed.fingerprint)
}

func TestDiscoverSimpleFTSRuntimeUnsupportedPlatformIsOptional(t *testing.T) {
	got, err := discoverSimpleFTSRuntimeFrom(
		filepath.Join(t.TempDir(), "agentsview"), "", "freebsd",
	)
	require.NoError(t, err)
	assert.False(t, got.available())

	_, err = discoverSimpleFTSRuntimeFrom(
		filepath.Join(t.TempDir(), "agentsview"), t.TempDir(), "freebsd",
	)
	require.Error(t, err)
}

func TestDiscoverSimpleFTSRuntimeExplicitDirIsValidated(t *testing.T) {
	_, err := discoverSimpleFTSRuntimeFrom(
		filepath.Join(t.TempDir(), "agentsview"), t.TempDir(), "linux",
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), simpleFTSDirEnv)
}

func TestCJKFTSChineseSearch(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	if !simpleFTSRuntimeConfig.available() {
		t.Skip("simple FTS5 runtime is not installed for this test process")
	}
	d := testDB(t)
	seedSearchSession(t, d, "chinese", "proj", [][2]string{
		{"user", "请验证这段长句中的中文搜索功能，并检查 SQLite 集成。"},
		{"assistant", "发现一个错误，随后修复。"},
	})
	seedSearchSession(t, d, "france", "proj", [][2]string{
		{"user", "法国的首都是巴黎。"},
	})
	seedSearchSession(t, d, "reverse", "proj", [][2]string{
		{"user", "这份材料讨论国法体系。"},
	})
	seedSearchSession(t, d, "english", "proj", [][2]string{
		{"user", "The runner is running get_views after error-401."},
	})

	var pending int
	require.NoError(d.getReader().QueryRow(
		"SELECT count(*) FROM messages_cjk_fts_pending_sessions",
	).Scan(&pending))
	assert.Zero(pending)

	var pinyinMatch string
	simpleFTSJiebaMu.Lock()
	err := d.getReader().QueryRow(
		"SELECT jieba_query(?, 0)", "zhong",
	).Scan(&pinyinMatch)
	simpleFTSJiebaMu.Unlock()
	require.NoError(err)
	var pinyinHits int
	require.NoError(d.getReader().QueryRow(
		`SELECT count(*) FROM messages_cjk_fts
		 WHERE messages_cjk_fts MATCH ?`, pinyinMatch,
	).Scan(&pinyinHits))
	assert.Zero(pinyinHits)

	for _, query := range []string{"中文搜索", "搜索", "错", "SQLite 中文搜索"} {
		page, err := d.SearchContent(t.Context(), ContentSearchFilter{
			Pattern: query,
			Mode:    "fts",
			Sources: []string{"messages"},
			Limit:   20,
		})
		require.NoError(err, "query %q", query)
		require.NotEmpty(page.Matches, "query %q", query)
		assert.Equal("chinese", page.Matches[0].SessionID, "query %q", query)
	}

	seedSearchSession(t, d, "separated", "proj", [][2]string{
		{"user", "中文和搜索之间插入了额外内容。"},
	})

	andQuery, err := d.SearchContent(t.Context(), ContentSearchFilter{
		Pattern: "中文 搜索",
		Mode:    "fts",
		Sources: []string{"messages"},
		Limit:   20,
	})
	require.NoError(err)
	andIDs := make(map[string]bool)
	for _, match := range andQuery.Matches {
		andIDs[match.SessionID] = true
	}
	assert.True(andIDs["chinese"])
	assert.True(andIDs["separated"])

	phrase, err := d.SearchContent(t.Context(), ContentSearchFilter{
		Pattern: `"中文 搜索"`,
		Mode:    "fts",
		Sources: []string{"messages"},
		Limit:   20,
	})
	require.NoError(err)
	require.Len(phrase.Matches, 1)
	assert.Equal("chinese", phrase.Matches[0].SessionID)

	expression, err := d.prepareMessageFTSQuery(
		t.Context(), `"中文" OR "国法"`,
	)
	require.NoError(err)
	assert.Equal(`"中文" OR "国法"`, expression.match)

	orQuery, err := d.SearchContent(t.Context(), ContentSearchFilter{
		Pattern: `"中文" OR "国法"`,
		Mode:    "fts",
		Sources: []string{"messages"},
		Limit:   20,
	})
	require.NoError(err)
	orIDs := make(map[string]bool)
	for _, match := range orQuery.Matches {
		orIDs[match.SessionID] = true
	}
	assert.True(orIDs["chinese"])
	assert.True(orIDs["reverse"])

	ordered, err := d.SearchContent(t.Context(), ContentSearchFilter{
		Pattern: "法国",
		Mode:    "fts",
		Sources: []string{"messages"},
		Limit:   20,
	})
	require.NoError(err)
	require.Len(ordered.Matches, 1)
	assert.Equal("france", ordered.Matches[0].SessionID)

	grouped, err := d.Search(t.Context(), SearchFilter{
		Query: "中文搜索",
		Limit: 20,
	})
	require.NoError(err)
	require.NotEmpty(grouped.Results)
	assert.Equal("chinese", grouped.Results[0].SessionID)

	// ASCII-only queries continue through the existing Porter-tokenized index.
	english, err := d.SearchContent(t.Context(), ContentSearchFilter{
		Pattern: "run",
		Mode:    "fts",
		Sources: []string{"messages"},
		Limit:   20,
	})
	require.NoError(err)
	require.NotEmpty(english.Matches)
	assert.Equal("english", english.Matches[0].SessionID)

	var storedFingerprint string
	require.NoError(d.getReader().QueryRow(
		"SELECT CAST(value AS TEXT) FROM stats WHERE key = ?",
		cjkFTSFingerprintStatsKey,
	).Scan(&storedFingerprint))
	assert.Equal(simpleFTSRuntimeConfig.fingerprint, storedFingerprint)

	// Simulate a pre-fix partial build: the table exists without the atomic
	// completion fingerprint. Reopen must replace and backfill it.
	_, err = d.getWriter().Exec(`
		DROP TRIGGER IF EXISTS messages_cjk_ai;
		DROP TRIGGER IF EXISTS messages_cjk_ad;
		DROP TRIGGER IF EXISTS messages_cjk_au;
		DROP TABLE messages_cjk_fts;
		CREATE VIRTUAL TABLE messages_cjk_fts USING fts5(
			content,
			content='messages',
			content_rowid='id',
			tokenize='simple'
		);
		DELETE FROM stats WHERE key = '` + cjkFTSFingerprintStatsKey + `'`)
	require.NoError(err)
	require.NoError(d.Reopen())
	repaired, err := d.SearchContent(t.Context(), ContentSearchFilter{
		Pattern: "中文搜索",
		Mode:    "fts",
		Sources: []string{"messages"},
		Limit:   20,
	})
	require.NoError(err)
	require.NotEmpty(repaired.Matches)
	assert.Equal("chinese", repaired.Matches[0].SessionID)

	_, err = d.getWriter().Exec(
		"UPDATE stats SET value = 'stale' WHERE key = ?",
		cjkFTSFingerprintStatsKey,
	)
	require.NoError(err)
	assert.False(d.HasCJKFTS())
	require.NoError(d.Reopen())
	assert.True(d.HasCJKFTS())

	require.NoError(d.CloseWriter())
	require.NoError(d.ReopenWriter())
	seedSearchSession(t, d, "reopened", "proj", [][2]string{
		{"user", "重新打开写连接以后仍然可以搜索新增中文。"},
	})
	reopened, err := d.SearchContent(t.Context(), ContentSearchFilter{
		Pattern: "新增中文",
		Mode:    "fts",
		Sources: []string{"messages"},
		Limit:   20,
	})
	require.NoError(err)
	require.NotEmpty(reopened.Matches)
	assert.Equal("reopened", reopened.Matches[0].SessionID)

	require.NoError(d.Reopen())
	seedSearchSession(t, d, "swapped", "proj", [][2]string{
		{"user", "完整重开数据库以后继续索引中文消息。"},
	})
	swapped, err := d.SearchContent(t.Context(), ContentSearchFilter{
		Pattern: "索引中文",
		Mode:    "fts",
		Sources: []string{"messages"},
		Limit:   20,
	})
	require.NoError(err)
	require.NotEmpty(swapped.Matches)
	assert.Equal("swapped", swapped.Matches[0].SessionID)
}

func TestCJKFTSContentSnippetCentersOnMatch(t *testing.T) {
	if !simpleFTSRuntimeConfig.available() {
		t.Skip("simple FTS5 runtime is not installed for this test process")
	}
	for _, tc := range []struct {
		name, query, opening, match string
	}{
		{"separated words", "全文搜索", "", "全文的搜索"},
		{"mixed language", "SQLite全文搜索", "", "SQLite 的全文的搜索"},
		{"contiguous phrase preferred", "全文搜索", "先讨论全文。", "全文搜索"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			body := tc.opening + strings.Repeat("开场说明。", 100) + tc.match + "实现说明。"
			seedSearchSession(t, d, "long-chinese", "proj", [][2]string{{"user", body}})

			page, err := d.SearchContent(t.Context(), ContentSearchFilter{
				Pattern: tc.query,
				Mode:    "fts",
				Sources: []string{"messages"},
				Limit:   20,
			})
			require.NoError(t, err)
			require.Len(t, page.Matches, 1)
			assert.Contains(t, page.Matches[0].Snippet, tc.match)
		})
	}
}

func TestCJKFTSTableCanBeDroppedWithoutExtension(t *testing.T) {
	require := require.New(t)

	if !simpleFTSRuntimeConfig.available() {
		t.Skip("simple FTS5 runtime is not installed for this test process")
	}
	path := filepath.Join(t.TempDir(), "drop-without-extension.db")
	d, err := Open(path)
	require.NoError(err)
	require.NoError(d.Close())

	raw, err := sql.Open("sqlite3", makeDSN(path, false))
	require.NoError(err)
	t.Cleanup(func() { require.NoError(raw.Close()) })
	_, err = raw.ExecContext(t.Context(), "DROP TABLE messages_cjk_fts")
	require.NoError(err)
}

func TestCJKFTSRebuildsAfterLegacyWriter(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	if !simpleFTSRuntimeConfig.available() {
		t.Skip("simple FTS5 runtime is not installed for this test process")
	}
	path := filepath.Join(t.TempDir(), "legacy-writer.db")
	d, err := Open(path)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(d.Close()) })
	seedSearchSession(t, d, "legacy", "proj", [][2]string{
		{"user", "原始中文内容。"},
	})

	raw, err := sql.Open("sqlite3", makeDSN(path, false))
	require.NoError(err)
	tx, err := raw.BeginTx(t.Context(), nil)
	require.NoError(err)
	_, err = tx.ExecContext(t.Context(),
		"UPDATE messages SET content = ? WHERE session_id = ?",
		"旧版本写入的新内容可以在重开后检索。", "legacy",
	)
	require.NoError(err)
	_, err = tx.ExecContext(t.Context(), `
		UPDATE sessions
		SET transcript_revision = COALESCE(transcript_revision, 0) + 1
		WHERE id = ?`, "legacy")
	require.NoError(err)
	require.NoError(tx.Commit())

	var pending int
	require.NoError(raw.QueryRowContext(t.Context(),
		"SELECT count(*) FROM messages_cjk_fts_pending_sessions",
	).Scan(&pending))
	assert.Equal(1, pending)
	require.NoError(raw.Close())
	require.False(d.HasCJKFTS())

	// A local metadata update does not repair the other writer's message index.
	name := "Renamed session"
	require.NoError(d.RenameSession("legacy", &name))
	require.False(d.HasCJKFTS(), "renaming must preserve the stale marker")
	insertSession(t, d, "legacy", "proj", func(s *Session) {
		s.UserMessageCount = 2
	})
	require.False(d.HasCJKFTS(), "upserts must preserve the stale marker")
	require.NoError(d.InsertMessages([]Message{{
		SessionID: "legacy", Ordinal: 1, Role: "assistant",
		Content: "本地追加的消息。",
	}}))
	require.False(d.HasCJKFTS(), "appends do not repair earlier stale content")

	var output bytes.Buffer
	previousWriter := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(previousWriter) })
	require.NoError(d.Reopen())
	assert.Contains(output.String(), "rebuilding CJK FTS index; startup waits")
	page, err := d.SearchContent(t.Context(), ContentSearchFilter{
		Pattern: "旧版本写入",
		Mode:    "fts",
		Sources: []string{"messages"},
		Limit:   20,
	})
	require.NoError(err)
	require.NotEmpty(page.Matches)
	assert.Equal("legacy", page.Matches[0].SessionID)

	require.NoError(d.getReader().QueryRow(
		"SELECT count(*) FROM messages_cjk_fts_pending_sessions",
	).Scan(&pending))
	assert.Zero(pending)
}

func TestCJKFTSForeignFingerprintDefersMaintenance(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	if !simpleFTSRuntimeConfig.available() {
		t.Skip("simple FTS5 runtime is not installed for this test process")
	}
	d := testDB(t)
	var output bytes.Buffer
	previousWriter := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(previousWriter) })
	seedSearchSession(t, d, "foreign-runtime", "proj", [][2]string{
		{"user", "原始中文内容。"},
	})

	_, err := d.getWriter().Exec(
		"UPDATE stats SET value = 'foreign-runtime' WHERE key = ?",
		cjkFTSFingerprintStatsKey,
	)
	require.NoError(err)
	assert.False(d.HasCJKFTS())
	assert.False(d.HasCJKFTS())
	assert.Equal(1, strings.Count(output.String(), "CJK FTS unavailable or stale"))

	require.NoError(d.Update(func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(t.Context(),
			"UPDATE messages SET content = ? WHERE session_id = ?",
			"跨版本写入的新中文内容。", "foreign-runtime",
		); err != nil {
			return err
		}
		_, err := tx.ExecContext(t.Context(), `
			UPDATE sessions
			SET transcript_revision = CAST(
				CAST(transcript_revision AS INTEGER) + 1 AS TEXT
			)
			WHERE id = ?`, "foreign-runtime")
		return err
	}))

	var pending int
	require.NoError(d.getReader().QueryRow(
		"SELECT count(*) FROM messages_cjk_fts_pending_sessions",
	).Scan(&pending))
	assert.Equal(1, pending)

	var match string
	simpleFTSJiebaMu.Lock()
	err = d.getReader().QueryRow(
		"SELECT jieba_query(?, 0)", "跨版本写入",
	).Scan(&match)
	simpleFTSJiebaMu.Unlock()
	require.NoError(err)

	var staleMatches int
	require.NoError(d.getReader().QueryRow(
		`SELECT count(*) FROM messages_cjk_fts
		 WHERE messages_cjk_fts MATCH ?`, match,
	).Scan(&staleMatches))
	assert.Zero(staleMatches)

	require.NoError(d.Reopen())
	assert.True(d.HasCJKFTS())
	page, err := d.SearchContent(t.Context(), ContentSearchFilter{
		Pattern: "跨版本写入",
		Mode:    "fts",
		Sources: []string{"messages"},
		Limit:   20,
	})
	require.NoError(err)
	require.NotEmpty(page.Matches)
	assert.Equal("foreign-runtime", page.Matches[0].SessionID)
	require.NoError(d.getReader().QueryRow(
		"SELECT count(*) FROM messages_cjk_fts_pending_sessions",
	).Scan(&pending))
	assert.Zero(pending)
}

func TestCJKFTSJiebaConfigurationSerializesWithQueries(t *testing.T) {
	if !simpleFTSRuntimeConfig.available() {
		t.Skip("simple FTS5 runtime is not installed for this test process")
	}
	path := filepath.Join(t.TempDir(), "jieba-concurrency.db")
	d, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, d.Close()) })
	seedSearchSession(t, d, "concurrent", "proj", [][2]string{
		{"user", "并发中文搜索。"},
	})

	const workers = 8
	const iterations = 4
	errs := make(chan error, workers*iterations)
	var wg sync.WaitGroup
	for worker := range workers {
		wg.Add(1)
		go func(openConnections bool) {
			defer wg.Done()
			for range iterations {
				if openConnections {
					conn, err := sql.Open(
						sqliteArchiveDriverName, makeDSN(path, true),
					)
					if err == nil {
						err = conn.PingContext(t.Context())
					}
					if conn != nil {
						if closeErr := conn.Close(); err == nil {
							err = closeErr
						}
					}
					if err != nil {
						errs <- err
					}
					continue
				}
				if _, err := d.prepareMessageFTSQuery(
					t.Context(), "并发中文搜索",
				); err != nil {
					errs <- err
				}
			}
		}(worker%2 == 0)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
}

// Session upserts and no-op recall inserts must leave a healthy index usable.
func TestCJKFTSSurvivesSessionResyncUpsert(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	if !simpleFTSRuntimeConfig.available() {
		t.Skip("simple FTS5 runtime is not installed for this test process")
	}
	d := testDB(t)
	seedSearchSession(t, d, "resync", "proj", [][2]string{
		{"user", "中文搜索必须在重新同步之后仍然可用。"},
	})
	require.True(d.HasCJKFTS(), "CJK FTS live after the first write")

	// Re-upsert the same session id, leaving transcript_revision alone. This
	// is the shape of every ordinary resync of an unchanged session.
	insertSession(t, d, "resync", "proj", func(s *Session) {
		s.Agent = "claude"
		s.UserMessageCount = 2
	})
	// Recall imports use this insert even when the session already exists.
	require.NoError(d.insertSessionIfAbsent(t.Context(), Session{
		ID: "resync", Project: "proj", Machine: "recall-import", Agent: "claude",
	}))

	var pending int
	require.NoError(d.getReader().QueryRow(
		"SELECT count(*) FROM messages_cjk_fts_pending_sessions",
	).Scan(&pending))
	assert.Zero(pending, "resync upsert must not strand a pending row")
	assert.True(d.HasCJKFTS(), "CJK FTS stays live across a resync")

	page, err := d.SearchContent(t.Context(), ContentSearchFilter{
		Pattern: "重新同步",
		Mode:    "fts",
		Sources: []string{"messages"},
		Limit:   20,
	})
	require.NoError(err)
	require.NotEmpty(page.Matches, "Chinese search still returns after resync")
	assert.Equal("resync", page.Matches[0].SessionID)
}

// TestCJKFTSSurvivesCompaction pins that staged archive compaction keeps
// the optional CJK index queryable. Compaction rebuilds the archive into a
// candidate file through maintenance connections that do not load the
// tokenizer sidecar, and then swaps that candidate in, so the index surviving
// the round trip is worth holding still.
func TestCJKFTSSurvivesCompaction(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	if !simpleFTSRuntimeConfig.available() {
		t.Skip("simple FTS5 runtime is not installed for this test process")
	}
	d := testDB(t)
	seedSearchSession(t, d, "compact-cn", "proj", [][2]string{
		{"user", "压缩之后中文索引必须继续可用。"},
	})
	require.True(d.HasCJKFTS(), "CJK FTS live before compaction")

	_, err := d.Compact(t.Context(), CompactOptions{
		StagingDir: t.TempDir(),
	})
	require.NoError(err, "compaction must not fail on an archive with a CJK index")

	assert.True(d.HasCJKFTS(), "CJK FTS still live after compaction")
	page, err := d.SearchContent(t.Context(), ContentSearchFilter{
		Pattern: "中文索引",
		Mode:    "fts",
		Sources: []string{"messages"},
		Limit:   20,
	})
	require.NoError(err)
	require.NotEmpty(page.Matches, "Chinese search still returns after compaction")
	assert.Equal("compact-cn", page.Matches[0].SessionID)
}
