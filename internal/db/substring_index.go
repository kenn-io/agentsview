package db

import (
	"context"
	"fmt"
	"log"
	"regexp/syntax"
	"strings"
)

// The substring index lists, for each three-character sequence, the messages
// whose text contains it. A LIKE '%pattern%' search intersects the lists for
// the pattern's sequences instead of reading every message body. detail=none
// keeps only message IDs, about a fifth of the indexed text, and the search
// still applies its LIKE to each candidate, so the index only has to return
// a superset: it folds case for every script, while LIKE folds only ASCII.
// Dialogue text and thinking text get separate tables because a detail=none
// table cannot tell which column a sequence came from.
const schemaSubstringIndex = `
CREATE VIEW IF NOT EXISTS message_trigram_source AS
SELECT id, session_id, COALESCE(dialogue_text, content) AS content,
    thinking_text AS thinking FROM messages;
CREATE VIRTUAL TABLE IF NOT EXISTS message_trigram_fts USING fts5(
    content,
    content='message_trigram_source', content_rowid='id',
    tokenize='trigram', detail='none', columnsize=0
);
CREATE VIRTUAL TABLE IF NOT EXISTS thinking_trigram_fts USING fts5(
    thinking,
    content='message_trigram_source', content_rowid='id',
    tokenize='trigram', detail='none', columnsize=0
);
CREATE TRIGGER IF NOT EXISTS messages_trigram_ai AFTER INSERT ON messages BEGIN
    INSERT INTO message_trigram_fts(rowid, content)
        VALUES (new.id, COALESCE(new.dialogue_text, new.content));
    INSERT INTO thinking_trigram_fts(rowid, thinking) VALUES (new.id, new.thinking_text);
END;
` + messagesTrigramADTriggerDDL + `
CREATE TRIGGER IF NOT EXISTS messages_trigram_au
AFTER UPDATE OF content, dialogue_text, thinking_text ON messages BEGIN
    INSERT INTO message_trigram_fts(message_trigram_fts, rowid, content)
        SELECT 'delete', old.id, COALESCE(old.dialogue_text, old.content)
        WHERE COALESCE(old.dialogue_text, old.content) IS NOT COALESCE(new.dialogue_text, new.content);
    INSERT INTO message_trigram_fts(rowid, content)
        SELECT new.id, COALESCE(new.dialogue_text, new.content)
        WHERE COALESCE(old.dialogue_text, old.content) IS NOT COALESCE(new.dialogue_text, new.content);
    INSERT INTO thinking_trigram_fts(thinking_trigram_fts, rowid, thinking)
        SELECT 'delete', old.id, old.thinking_text WHERE old.thinking_text IS NOT new.thinking_text;
    INSERT INTO thinking_trigram_fts(rowid, thinking)
        SELECT new.id, new.thinking_text WHERE old.thinking_text IS NOT new.thinking_text;
END;
`

// messagesTrigramADTriggerDDL is dropped and restored around bulk session
// deletes, like messagesADTriggerDDL.
const messagesTrigramADTriggerDDL = `
CREATE TRIGGER IF NOT EXISTS messages_trigram_ad AFTER DELETE ON messages BEGIN
    INSERT INTO message_trigram_fts(message_trigram_fts, rowid, content)
        VALUES ('delete', old.id, COALESCE(old.dialogue_text, old.content));
    INSERT INTO thinking_trigram_fts(thinking_trigram_fts, rowid, thinking)
        VALUES ('delete', old.id, old.thinking_text);
END;
`

// substringIndexBulkDeleteSQL removes one session's rows from both tables
// before its messages are deleted with messages_trigram_ad dropped.
var substringIndexBulkDeleteSQL = []string{
	`INSERT INTO message_trigram_fts(message_trigram_fts, rowid, content)
	 SELECT 'delete', id, content FROM message_trigram_source WHERE session_id = ?`,
	`INSERT INTO thinking_trigram_fts(thinking_trigram_fts, rowid, thinking)
	 SELECT 'delete', id, thinking FROM message_trigram_source WHERE session_id = ?`,
}

var dropSubstringIndexSQL = []string{
	"DROP TRIGGER IF EXISTS messages_trigram_ai",
	"DROP TRIGGER IF EXISTS messages_trigram_ad",
	"DROP TRIGGER IF EXISTS messages_trigram_au",
	"DROP TABLE IF EXISTS message_trigram_fts",
	"DROP TABLE IF EXISTS thinking_trigram_fts",
	"DROP VIEW IF EXISTS message_trigram_source",
}

// ensureSubstringIndex builds the substring index once for an archive that
// lacks it, after column migrations have added thinking_text. The build reads
// every message body, so a large archive waits for it on the first writable
// open after an upgrade. Builds without FTS5 keep scanning bodies.
func ensureSubstringIndex(ctx context.Context, w *writerHandle, progress OpenProgressFunc, deferBuild bool) (bool, error) {
	var available, exists bool
	if err := w.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM pragma_module_list WHERE name = 'fts5'),
			EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='message_trigram_fts')`,
	).Scan(&available, &exists); err != nil {
		return false, fmt.Errorf("checking substring index: %w", err)
	}
	if !available || exists {
		return false, nil
	}
	if deferBuild {
		var hasMessages bool
		if err := w.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM messages)`).Scan(&hasMessages); err != nil {
			return false, fmt.Errorf("checking messages for substring index: %w", err)
		}
		if hasMessages {
			return true, nil
		}
	}
	progress.report("Building substring search index")
	log.Print("building SQLite substring search index; scanning all messages")
	return false, rebuildSubstringIndex(ctx, w)
}

// rebuildSubstringIndex replaces the index, its triggers, and its contents in
// one transaction, so readers see either no index or a complete one.
func rebuildSubstringIndex(ctx context.Context, w *writerHandle) error {
	tx, err := w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stmts := append(append([]string{}, dropSubstringIndexSQL...),
		schemaSubstringIndex,
		"INSERT INTO message_trigram_fts(message_trigram_fts) VALUES('rebuild')",
		"INSERT INTO thinking_trigram_fts(thinking_trigram_fts) VALUES('rebuild')",
	)
	for _, stmt := range stmts {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("building substring index: %w", err)
		}
	}
	return tx.Commit()
}

// hasSubstringIndex reports whether both index tables exist and load on this
// connection. Archives that a writable open has not upgraded yet, and bulk
// imports that dropped the index, fall back to scanning bodies.
func (db *DB) hasSubstringIndex(ctx context.Context) bool {
	_, err := db.getReader().Exec(ctx,
		"SELECT (SELECT 1 FROM message_trigram_fts LIMIT 1), (SELECT 1 FROM thinking_trigram_fts LIMIT 1)")
	return err == nil
}

// substringIndexDropped reports whether err came from a query that named a
// substring index table after DropFTS removed it.
func substringIndexDropped(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "no such table: message_trigram_fts") ||
		strings.Contains(msg, "no such table: thinking_trigram_fts")
}

// searchWithSubstringCandidates chooses index filters for query and runs
// search with them. An archive import can drop the index between those
// steps, because each query reads its own snapshot, so a search that then
// names a missing index table runs once more. The second attempt finds no
// index and scans.
func searchWithSubstringCandidates[T any](
	ctx context.Context, db *DB, f ContentSearchFilter, query string,
	search func(substringCandidateFilters) (T, error),
) (T, error) {
	attempt := func() (T, error) {
		candidates, err := db.substringCandidates(ctx, f, query)
		if err != nil {
			var zero T
			return zero, err
		}
		return search(candidates)
	}
	return retryWhenSubstringIndexDropped(attempt)
}

// retryWhenSubstringIndexDropped runs attempt, and runs it once more when it
// named a substring index table an archive import dropped after the attempt
// chose to use the index. The second attempt finds no index and scans.
func retryWhenSubstringIndexDropped[T any](attempt func() (T, error)) (T, error) {
	out, err := attempt()
	if substringIndexDropped(err) {
		return attempt()
	}
	return out, err
}

// maxSubstringIndexTrigrams bounds the lists one lookup intersects. Fewer
// sequences only widen the candidate set, which the LIKE check then narrows.
const maxSubstringIndexTrigrams = 32

// substringIndexQuery returns a case-insensitive FTS5 query for a literal.
// An empty query means the index cannot look it up.
func substringIndexQuery(literal string) string {
	if strings.ContainsRune(literal, 0) {
		return ""
	}
	return strings.Join(substringIndexTerms([]rune(literal), make(map[string]struct{})), " AND ")
}

// substringIndexTerms adds distinct trigrams to seen, up to the shared limit.
func substringIndexTerms(runes []rune, seen map[string]struct{}) []string {
	var terms []string
	for i := 0; i+3 <= len(runes) && len(seen) < maxSubstringIndexTrigrams; i++ {
		trigram := string(runes[i : i+3])
		if _, ok := seen[trigram]; ok {
			continue
		}
		seen[trigram] = struct{}{}
		terms = append(terms, `"`+strings.ReplaceAll(trigram, `"`, `""`)+`"`)
	}
	return terms
}

// regexIndexQuery returns an index query and the longest required literal safe
// for SQLite LIKE. An empty value leaves that prefilter unused.
func regexIndexQuery(pattern string) (string, string) {
	re, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		return "", ""
	}
	var terms []string
	var longest string
	seen := make(map[string]struct{})
	var visit func(*syntax.Regexp)
	visit = func(re *syntax.Regexp) {
		switch re.Op {
		case syntax.OpLiteral:
			var run []rune
			flush := func() {
				if len(run) > len([]rune(longest)) {
					longest = string(run)
				}
				terms = append(terms, substringIndexTerms(run, seen)...)
				run = nil
			}
			for _, r := range re.Rune {
				if r == 0 || (re.Flags&syntax.FoldCase != 0 && (r > 127 || r == 'K' || r == 'k' || r == 'S' || r == 's')) {
					flush()
				} else {
					run = append(run, r)
				}
			}
			flush()
		case syntax.OpCapture, syntax.OpPlus:
			visit(re.Sub[0])
		case syntax.OpRepeat:
			if re.Min > 0 {
				visit(re.Sub[0])
			}
		case syntax.OpConcat:
			for _, sub := range re.Sub {
				visit(sub)
			}
		}
	}
	visit(re)
	return strings.Join(terms, " AND "), longest
}

// substringFilter is an optional SQL condition and its arguments.
type substringFilter struct {
	sql  string
	args []any
}

// substringCandidateFilters limit the message and thinking branches of a
// content search to rows the substring index lists for a query.
type substringCandidateFilters struct {
	message, thinking substringFilter
}

// substringIndexMaxShare is the largest fraction, as 1/n, of the messages
// in a search's scope that the index may list before a scan of the scope's
// sessions costs less than looking up each listed message.
const substringIndexMaxShare = 4

// substringCandidates returns conditions that keep only messages whose text
// may match query. A branch keeps scanning when the archive has no substring
// index, query is empty, or the index lists more than a quarter as many
// messages as the scope's sessions hold. The index lists matches across the whole archive, so a common
// sequence inside one project or date range is cheaper to scan for. The
// capped count stops as soon as it passes that share.
func (db *DB) substringCandidates(
	ctx context.Context, f ContentSearchFilter, query string,
) (substringCandidateFilters, error) {
	var out substringCandidateFilters
	if f.SessionID != "" {
		return out, nil
	}
	if query == "" || !db.hasSubstringIndex(ctx) {
		return out, nil
	}
	scope, scopeArgs := sessionScopeSubquery(f)
	scopeMessages, err := db.substringScopeMessages(ctx,
		"(SELECT id AS session_id, message_count FROM sessions) WHERE "+scope, scopeArgs)
	if err != nil {
		return out, err
	}
	for _, branch := range []struct {
		table  string
		filter *substringFilter
	}{
		{"message_trigram_fts", &out.message},
		{"thinking_trigram_fts", &out.thinking},
	} {
		selective, err := db.substringIndexSelective(ctx, branch.table, query, scopeMessages)
		if err != nil {
			return out, err
		}
		if !selective {
			continue
		}
		*branch.filter = substringFilter{
			sql:  " AND m.id IN (SELECT rowid FROM " + branch.table + " WHERE " + branch.table + " MATCH ?)",
			args: []any{query},
		}
	}
	return out, nil
}

// substringScopeMessages sums message_count over the sessions that from,
// a FROM clause with its WHERE, selects.
func (db *DB) substringScopeMessages(ctx context.Context, from string, args []any) (int64, error) {
	var scopeMessages int64
	if err := db.getReader().QueryRow(ctx,
		`SELECT COALESCE(SUM(message_count), 0) FROM `+from, args...,
	).Scan(&scopeMessages); err != nil {
		return 0, fmt.Errorf("sizing substring search scope: %w", err)
	}
	return scopeMessages, nil
}

// substringIndexSelective reports whether table lists at most a
// substringIndexMaxShare share of scopeMessages for query. The capped count
// stops as soon as it passes that share.
func (db *DB) substringIndexSelective(
	ctx context.Context, table, query string, scopeMessages int64,
) (bool, error) {
	limit := scopeMessages / substringIndexMaxShare
	var listed int64
	if err := db.getReader().QueryRow(ctx,
		`SELECT count(*) FROM (SELECT 1 FROM `+table+` WHERE `+table+` MATCH ? LIMIT ?)`,
		query, limit+1,
	).Scan(&listed); err != nil {
		return false, fmt.Errorf("counting substring candidates: %w", err)
	}
	return listed <= limit, nil
}

// termsIndexQueries returns the substring index query for each term the
// index lists selectively within a terms search's scope. Terms it cannot
// look up, or lists for more than a quarter of the scope's messages, keep
// the session prefilter's scan.
func (db *DB) termsIndexQueries(
	ctx context.Context, f ContentSearchFilter, terms []string,
) (map[string]string, error) {
	if f.SessionID != "" || !db.hasSubstringIndex(ctx) {
		return nil, nil
	}
	where, args := termsScopeSQL(f, SQLiteQueryDialect())
	scopeMessages, err := db.substringScopeMessages(ctx, "sessions WHERE "+where, args)
	if err != nil {
		return nil, err
	}
	indexed := make(map[string]string, len(terms))
	for _, term := range terms {
		query := substringIndexQuery(term)
		if query == "" {
			continue
		}
		selective, err := db.substringIndexSelective(ctx, "message_trigram_fts", query, scopeMessages)
		if err != nil {
			return nil, err
		}
		if selective {
			indexed[term] = query
		}
	}
	return indexed, nil
}
