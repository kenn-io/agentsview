package db

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestContainsCJK(t *testing.T) {
	t.Parallel()
	assert.True(t, containsCJK("SQLite 中文搜索"))
	assert.True(t, containsCJK("日本語"))
	assert.True(t, containsCJK("한국어"))
	assert.False(t, containsCJK("get_views error-401"))
}

func TestDiscoverSimpleFTSRuntimeFrom(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "libsimple.so"), []byte("library"), 0o600,
	))
	dictDir := filepath.Join(dir, "dict")
	require.NoError(t, os.Mkdir(dictDir, 0o700))
	for _, name := range []string{
		"hmm_model.utf8",
		"idf.utf8",
		"jieba.dict.utf8",
		"stop_words.utf8",
		"user.dict.utf8",
	} {
		require.NoError(t, os.WriteFile(
			filepath.Join(dictDir, name), []byte(name), 0o600,
		))
	}

	got, err := discoverSimpleFTSRuntimeFrom(
		filepath.Join(t.TempDir(), "agentsview"), dir, "linux",
	)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "libsimple.so"), got.libraryPath)
	assert.Equal(t, dictDir, got.dictionaryPath)
	assert.NotEmpty(t, got.fingerprint)

	originalFingerprint := got.fingerprint
	require.NoError(t, os.WriteFile(
		filepath.Join(dictDir, "user.dict.utf8"), []byte("changed"), 0o600,
	))
	changed, err := discoverSimpleFTSRuntimeFrom(
		filepath.Join(t.TempDir(), "agentsview"), dir, "linux",
	)
	require.NoError(t, err)
	assert.NotEqual(t, originalFingerprint, changed.fingerprint)
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

func TestChineseFTSSearch(t *testing.T) {
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
	require.NoError(t, d.getReader().QueryRow(
		"SELECT count(*) FROM messages_chinese_fts_pending_sessions",
	).Scan(&pending))
	assert.Zero(t, pending)

	var pinyinMatch string
	simpleFTSJiebaMu.Lock()
	err := d.getReader().QueryRow(
		"SELECT jieba_query(?, 0)", "zhong",
	).Scan(&pinyinMatch)
	simpleFTSJiebaMu.Unlock()
	require.NoError(t, err)
	var pinyinHits int
	require.NoError(t, d.getReader().QueryRow(
		`SELECT count(*) FROM messages_chinese_fts
		 WHERE messages_chinese_fts MATCH ?`, pinyinMatch,
	).Scan(&pinyinHits))
	assert.Zero(t, pinyinHits)

	for _, query := range []string{"中文搜索", "搜索", "错", "SQLite 中文搜索"} {
		page, err := d.SearchContent(context.Background(), ContentSearchFilter{
			Pattern: query,
			Mode:    "fts",
			Sources: []string{"messages"},
			Limit:   20,
		})
		require.NoError(t, err, "query %q", query)
		require.NotEmpty(t, page.Matches, "query %q", query)
		assert.Equal(t, "chinese", page.Matches[0].SessionID, "query %q", query)
	}

	seedSearchSession(t, d, "separated", "proj", [][2]string{
		{"user", "中文和搜索之间插入了额外内容。"},
	})

	andQuery, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "中文 搜索",
		Mode:    "fts",
		Sources: []string{"messages"},
		Limit:   20,
	})
	require.NoError(t, err)
	andIDs := make(map[string]bool)
	for _, match := range andQuery.Matches {
		andIDs[match.SessionID] = true
	}
	assert.True(t, andIDs["chinese"])
	assert.True(t, andIDs["separated"])

	phrase, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: `"中文 搜索"`,
		Mode:    "fts",
		Sources: []string{"messages"},
		Limit:   20,
	})
	require.NoError(t, err)
	require.Len(t, phrase.Matches, 1)
	assert.Equal(t, "chinese", phrase.Matches[0].SessionID)

	expression, err := d.prepareMessageFTSQuery(
		context.Background(), `"中文" OR "国法"`,
	)
	require.NoError(t, err)
	assert.Equal(t, `"中文" OR "国法"`, expression.match)

	orQuery, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: `"中文" OR "国法"`,
		Mode:    "fts",
		Sources: []string{"messages"},
		Limit:   20,
	})
	require.NoError(t, err)
	orIDs := make(map[string]bool)
	for _, match := range orQuery.Matches {
		orIDs[match.SessionID] = true
	}
	assert.True(t, orIDs["chinese"])
	assert.True(t, orIDs["reverse"])

	ordered, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "法国",
		Mode:    "fts",
		Sources: []string{"messages"},
		Limit:   20,
	})
	require.NoError(t, err)
	require.Len(t, ordered.Matches, 1)
	assert.Equal(t, "france", ordered.Matches[0].SessionID)

	grouped, err := d.Search(context.Background(), SearchFilter{
		Query: "中文搜索",
		Limit: 20,
	})
	require.NoError(t, err)
	require.NotEmpty(t, grouped.Results)
	assert.Equal(t, "chinese", grouped.Results[0].SessionID)

	// ASCII-only queries continue through the existing Porter-tokenized index.
	english, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "run",
		Mode:    "fts",
		Sources: []string{"messages"},
		Limit:   20,
	})
	require.NoError(t, err)
	require.NotEmpty(t, english.Matches)
	assert.Equal(t, "english", english.Matches[0].SessionID)

	var storedFingerprint string
	require.NoError(t, d.getReader().QueryRow(
		"SELECT CAST(value AS TEXT) FROM stats WHERE key = ?",
		chineseFTSFingerprintStatsKey,
	).Scan(&storedFingerprint))
	assert.Equal(t, simpleFTSRuntimeConfig.fingerprint, storedFingerprint)

	// Simulate a pre-fix partial build: the table exists without the atomic
	// completion fingerprint. Reopen must replace and backfill it.
	_, err = d.getWriter().Exec(`
		DROP TRIGGER IF EXISTS messages_chinese_ai;
		DROP TRIGGER IF EXISTS messages_chinese_ad;
		DROP TRIGGER IF EXISTS messages_chinese_au;
		DROP TABLE messages_chinese_fts;
		CREATE VIRTUAL TABLE messages_chinese_fts USING fts5(
			content,
			content='messages',
			content_rowid='id',
			tokenize='simple'
		);
		DELETE FROM stats WHERE key = '` + chineseFTSFingerprintStatsKey + `'`)
	require.NoError(t, err)
	require.NoError(t, d.Reopen())
	repaired, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "中文搜索",
		Mode:    "fts",
		Sources: []string{"messages"},
		Limit:   20,
	})
	require.NoError(t, err)
	require.NotEmpty(t, repaired.Matches)
	assert.Equal(t, "chinese", repaired.Matches[0].SessionID)

	_, err = d.getWriter().Exec(
		"UPDATE stats SET value = 'stale' WHERE key = ?",
		chineseFTSFingerprintStatsKey,
	)
	require.NoError(t, err)
	assert.False(t, d.HasChineseFTS())
	require.NoError(t, d.Reopen())
	assert.True(t, d.HasChineseFTS())

	require.NoError(t, d.CloseWriter())
	require.NoError(t, d.ReopenWriter())
	seedSearchSession(t, d, "reopened", "proj", [][2]string{
		{"user", "重新打开写连接以后仍然可以搜索新增中文。"},
	})
	reopened, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "新增中文",
		Mode:    "fts",
		Sources: []string{"messages"},
		Limit:   20,
	})
	require.NoError(t, err)
	require.NotEmpty(t, reopened.Matches)
	assert.Equal(t, "reopened", reopened.Matches[0].SessionID)

	require.NoError(t, d.Reopen())
	seedSearchSession(t, d, "swapped", "proj", [][2]string{
		{"user", "完整重开数据库以后继续索引中文消息。"},
	})
	swapped, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "索引中文",
		Mode:    "fts",
		Sources: []string{"messages"},
		Limit:   20,
	})
	require.NoError(t, err)
	require.NotEmpty(t, swapped.Matches)
	assert.Equal(t, "swapped", swapped.Matches[0].SessionID)
}

func TestChineseFTSTableCanBeDroppedWithoutExtension(t *testing.T) {
	if !simpleFTSRuntimeConfig.available() {
		t.Skip("simple FTS5 runtime is not installed for this test process")
	}
	path := filepath.Join(t.TempDir(), "drop-without-extension.db")
	d, err := Open(path)
	require.NoError(t, err)
	require.NoError(t, d.Close())

	raw, err := sql.Open("sqlite3", makeDSN(path, false))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	_, err = raw.Exec("DROP TABLE messages_chinese_fts")
	require.NoError(t, err)
}

func TestChineseFTSRebuildsAfterLegacyWriter(t *testing.T) {
	if !simpleFTSRuntimeConfig.available() {
		t.Skip("simple FTS5 runtime is not installed for this test process")
	}
	path := filepath.Join(t.TempDir(), "legacy-writer.db")
	d, err := Open(path)
	require.NoError(t, err)
	seedSearchSession(t, d, "legacy", "proj", [][2]string{
		{"user", "原始中文内容。"},
	})
	require.NoError(t, d.Close())

	raw, err := sql.Open("sqlite3", makeDSN(path, false))
	require.NoError(t, err)
	tx, err := raw.Begin()
	require.NoError(t, err)
	_, err = tx.Exec(
		"UPDATE messages SET content = ? WHERE session_id = ?",
		"旧版本写入的新内容可以在重开后检索。", "legacy",
	)
	require.NoError(t, err)
	_, err = tx.Exec(`
		UPDATE sessions
		SET transcript_revision = COALESCE(transcript_revision, 0) + 1
		WHERE id = ?`, "legacy")
	require.NoError(t, err)
	require.NoError(t, tx.Commit())

	var pending int
	require.NoError(t, raw.QueryRow(
		"SELECT count(*) FROM messages_chinese_fts_pending_sessions",
	).Scan(&pending))
	assert.Equal(t, 1, pending)
	require.NoError(t, raw.Close())

	d, err = Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, d.Close()) })
	page, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "旧版本写入",
		Mode:    "fts",
		Sources: []string{"messages"},
		Limit:   20,
	})
	require.NoError(t, err)
	require.NotEmpty(t, page.Matches)
	assert.Equal(t, "legacy", page.Matches[0].SessionID)

	require.NoError(t, d.getReader().QueryRow(
		"SELECT count(*) FROM messages_chinese_fts_pending_sessions",
	).Scan(&pending))
	assert.Zero(t, pending)
}

func TestChineseFTSForeignFingerprintDefersMaintenance(t *testing.T) {
	if !simpleFTSRuntimeConfig.available() {
		t.Skip("simple FTS5 runtime is not installed for this test process")
	}
	d := testDB(t)
	seedSearchSession(t, d, "foreign-runtime", "proj", [][2]string{
		{"user", "原始中文内容。"},
	})

	_, err := d.getWriter().Exec(
		"UPDATE stats SET value = 'foreign-runtime' WHERE key = ?",
		chineseFTSFingerprintStatsKey,
	)
	require.NoError(t, err)
	assert.False(t, d.HasChineseFTS())

	require.NoError(t, d.Update(func(tx *sql.Tx) error {
		if _, err := tx.Exec(
			"UPDATE messages SET content = ? WHERE session_id = ?",
			"跨版本写入的新中文内容。", "foreign-runtime",
		); err != nil {
			return err
		}
		_, err := tx.Exec(`
			UPDATE sessions
			SET transcript_revision = CAST(
				CAST(transcript_revision AS INTEGER) + 1 AS TEXT
			)
			WHERE id = ?`, "foreign-runtime")
		return err
	}))

	var pending int
	require.NoError(t, d.getReader().QueryRow(
		"SELECT count(*) FROM messages_chinese_fts_pending_sessions",
	).Scan(&pending))
	assert.Equal(t, 1, pending)

	var match string
	simpleFTSJiebaMu.Lock()
	err = d.getReader().QueryRow(
		"SELECT jieba_query(?, 0)", "跨版本写入",
	).Scan(&match)
	simpleFTSJiebaMu.Unlock()
	require.NoError(t, err)

	var staleMatches int
	require.NoError(t, d.getReader().QueryRow(
		`SELECT count(*) FROM messages_chinese_fts
		 WHERE messages_chinese_fts MATCH ?`, match,
	).Scan(&staleMatches))
	assert.Zero(t, staleMatches)

	require.NoError(t, d.Reopen())
	assert.True(t, d.HasChineseFTS())
	page, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "跨版本写入",
		Mode:    "fts",
		Sources: []string{"messages"},
		Limit:   20,
	})
	require.NoError(t, err)
	require.NotEmpty(t, page.Matches)
	assert.Equal(t, "foreign-runtime", page.Matches[0].SessionID)
	require.NoError(t, d.getReader().QueryRow(
		"SELECT count(*) FROM messages_chinese_fts_pending_sessions",
	).Scan(&pending))
	assert.Zero(t, pending)
}

func TestChineseFTSJiebaConfigurationSerializesWithQueries(t *testing.T) {
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
						err = conn.Ping()
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
					context.Background(), "并发中文搜索",
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
