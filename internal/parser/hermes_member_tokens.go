package parser

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"hash"
	"regexp"
	"strings"
)

func hermesHasTranscriptRevision(ctx context.Context, conn *sql.DB) (bool, error) {
	var count int
	err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('sessions') WHERE name='transcript_revision'`).Scan(&count)
	return count == 1, err
}

func hermesMemberTokenQuery(revision, single bool) string {
	rev := "0"
	if revision {
		rev = "transcript_revision"
	}
	where := ""
	if single {
		where = " WHERE id = ?"
	}
	return `SELECT ` + hermesStateSessionColumns + `, ` + rev + ` AS revision FROM sessions` + where + ` ORDER BY id`
}

func listHermesMemberTokens(ctx context.Context, conn *sql.DB, id string, yield func(string, string, bool, []hermesModelUsage) error) error {
	revision, err := hermesHasTranscriptRevision(ctx, conn)
	if err != nil {
		return err
	}
	usage, err := readHermesModelUsage(ctx, conn, id)
	if err != nil {
		return err
	}
	checksums := make(map[string]hash.Hash)
	query := `SELECT session_id, id, timestamp, COALESCE(finish_reason, '') FROM messages`
	var messageArgs []any
	if id != "" {
		query += ` WHERE session_id = ?`
		messageArgs = append(messageArgs, id)
	}
	query += ` ORDER BY session_id, id`
	messageRows, err := conn.QueryContext(ctx, query, messageArgs...)
	if err != nil {
		return err
	}
	for messageRows.Next() {
		var sid string
		var messageID, timestamp, finish any
		if err := messageRows.Scan(&sid, &messageID, &timestamp, &finish); err != nil {
			messageRows.Close()
			return err
		}
		h := checksums[sid]
		if h == nil {
			h = sha256.New()
			checksums[sid] = h
		}
		fmt.Fprintf(h, "%T:%q\x00%T:%q\x00%T:%q\x00", messageID, fmt.Sprint(messageID), timestamp, fmt.Sprint(timestamp), finish, fmt.Sprint(finish))
	}
	err = messageRows.Err()
	messageRows.Close()
	if err != nil {
		return err
	}
	var args []any
	if id != "" {
		args = []any{id}
	}
	rows, err := conn.QueryContext(ctx, hermesMemberTokenQuery(revision, id != ""), args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return err
	}
	for rows.Next() {
		// Scan raw values so a malformed session still gets selected and follows
		// the normal member-error/transcript-fallback path.
		values := make([]any, len(columns))
		targets := make([]any, len(values))
		for i := range values {
			targets[i] = &values[i]
		}
		if err := rows.Scan(targets...); err != nil {
			return err
		}
		sid, ok := values[0].(string)
		if !ok || !isValidHermesSessionID(sid) {
			continue
		}
		h := sha256.New()
		for _, value := range values {
			fmt.Fprintf(h, "%T:%q\x00", value, fmt.Sprint(value))
		}
		fmt.Fprintf(h, "%#v", usage[sid])
		if checksum := checksums[sid]; checksum != nil {
			h.Write(checksum.Sum(nil))
		}
		if err := yield(sid, hex.EncodeToString(h.Sum(nil)), values[1] == "cron", usage[sid]); err != nil {
			return err
		}
	}
	return rows.Err()
}

func hermesMemberToken(ctx context.Context, conn *sql.DB, id string) (string, []hermesModelUsage, error) {
	var token string
	var usage []hermesModelUsage
	err := listHermesMemberTokens(ctx, conn, id, func(_ string, value string, _ bool, rows []hermesModelUsage) error {
		token, usage = value, rows
		return nil
	})
	return token, usage, err
}

func hermesStoredMemberToken(value string) (string, bool) {
	parts := strings.Split(value, ":")
	if len(parts) != 3 || parts[0] != "hst1" {
		return "", false
	}
	return parts[1], true
}

var (
	hermesTriggerComments = regexp.MustCompile(`--[^\n]*`)
	hermesRevisionTrigger = regexp.MustCompile(`(?is)^create\s+trigger\s+(?:if\s+not\s+exists\s+)?\w+\s+after\s+update\s+of\s+([\w,\s]+)\s+on\s+messages\s+begin\s+update\s+sessions\s+set\s+transcript_revision\s*=\s*transcript_revision\s*\+\s*1\s+where\s+id\s*=\s*new\.session_id\s*;\s*end\s*;?$`)
)

// Recheck on every event: an older Hermes writer can recreate a shorter
// trigger on opening the store. Unrecognized definitions fail closed.
func hermesStoreTracksRewrites(ctx context.Context, conn *sql.DB) (bool, error) {
	// Schema 33 is the verified ceiling, not a promise against additive changes.
	var version any
	err := conn.QueryRowContext(ctx, `SELECT version FROM schema_version LIMIT 1`).Scan(&version)
	if err != nil {
		return false, err
	}
	v, ok := version.(int64)
	if !ok || v < 1 || v > 33 {
		return false, nil
	}
	revision, err := hermesHasTranscriptRevision(ctx, conn)
	if err != nil || !revision {
		return false, err
	}
	rows, err := conn.QueryContext(ctx, `SELECT sql FROM sqlite_master WHERE type='trigger' AND tbl_name='messages'`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	covered := map[string]bool{}
	for rows.Next() {
		var definition string
		if err := rows.Scan(&definition); err != nil {
			return false, err
		}
		match := hermesRevisionTrigger.FindStringSubmatch(strings.TrimSpace(hermesTriggerComments.ReplaceAllString(definition, "")))
		if len(match) != 2 {
			continue
		}
		for column := range strings.SplitSeq(strings.ToLower(match[1]), ",") {
			covered[strings.TrimSpace(column)] = true
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	for _, column := range []string{"role", "content", "tool_call_id", "tool_calls", "reasoning", "reasoning_content", "reasoning_details", "codex_reasoning_items", "codex_message_items"} {
		if !covered[column] {
			return false, nil
		}
	}
	return true, nil
}

func (p *hermesProvider) StoredMemberFreshnessContainer(path string) (string, bool) {
	for _, root := range p.sources.roots {
		if isHermesProfilesContainer(root) {
			profile, ok := hermesProfileRootForPath(root, path)
			if !ok {
				continue
			}
			root = profile
		}
		if stateDB, _, ok := hermesStatePaths(root); ok && hermesStatePathAffectsArchive(path, stateDB) {
			return stateDB, true
		}
	}
	return "", false
}

func (s hermesSourceSet) changedHermesMembers(ctx context.Context, root, stateDB, sessionsDir string, pager StoredMemberFreshnessPager) ([]SourceRef, bool, error) {
	conn, err := openSQLiteReadOnly(stateDB, sqliteReadOptions{})
	if err != nil {
		return nil, false, err
	}
	defer conn.Close()
	// All token inputs and fallback readability checks use the same snapshot.
	conn.SetMaxOpenConns(1)
	if _, err := conn.ExecContext(ctx, "BEGIN"); err != nil {
		return nil, false, err
	}
	// Finish this caller's read snapshot after cancellation.
	defer func() { _, _ = conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK") }()
	tracks, err := hermesStoreTracksRewrites(ctx, conn)
	if err != nil || !tracks {
		return nil, false, err
	}
	cursor, found, err := firstUnsuppressedStoredPage(ctx, pager)
	if err != nil || !found {
		return nil, false, ctx.Err()
	}
	var sources []SourceRef
	selected := make(map[string]struct{})
	err = listHermesMemberTokens(ctx, conn, "", func(id, token string, cron bool, _ []hermesModelUsage) error {
		row, found, err := cursor.Lookup(ctx, VirtualSourcePath(stateDB, id))
		if err != nil {
			return err
		}
		if found {
			if row.Suppressed {
				return nil
			}
			if stored, ok := hermesStoredMemberToken(row.FingerprintHash); ok && stored == token && !cron {
				return nil
			}
		}
		source := s.stateMemberSourceRef(root, stateDB, id)
		src := source.Opaque.(hermesSource)
		src.FingerprintSingleMember = true
		source.Opaque = src
		sources = append(sources, source)
		selected[id] = struct{}{}
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	// The token query has closed, so reuse the same snapshot for transcript membership.
	err = s.discoverTranscriptEachWithMembership(ctx, root, sessionsDir, func(id string) (bool, error) {
		if _, changed := selected[id]; changed {
			return hermesStateDBHasReadableSession(ctx, conn, id)
		}
		// Unchanged and suppressed live members need only an indexed existence
		// check. Orphans have no row; changed members get full readability checks.
		var exists bool
		err := conn.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sessions WHERE id=?)`, id).Scan(&exists)
		return exists, err
	}, func(source SourceRef) error {
		sources = append(sources, source)
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return sources, true, nil
}
