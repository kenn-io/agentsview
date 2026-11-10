package parser

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

// Tokens describe the pinned producer's writes, not arbitrary external SQL.
// Timestamp repair at aca4f1df uses this exact BETWEEN predicate, including
// SQLite's NULL handling. See session-format-sources.md before changing it.
const hermesMessageTokenColumns = `COUNT(*) AS n, MAX(id) AS last_id,
 SUM(NOT(timestamp BETWEEN 0.0 AND 4200000000.0)) AS invalid_times`

func hermesHasTranscriptRevision(ctx context.Context, conn *sql.DB) (bool, error) {
	var count int
	err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('sessions') WHERE name='transcript_revision'`).Scan(&count)
	return count == 1, err
}

// The whole-store and single-member forms read identical values. Grouping the
// covering session/timestamp index avoids fetching transcript payload pages.
func hermesMemberTokenQuery(revision bool, single bool) string {
	rev := "0"
	if revision {
		rev = "transcript_revision"
	}
	where, messageWhere := "", ""
	if single {
		where = " WHERE id = ?"
		messageWhere = " WHERE session_id = ?"
	}
	return `WITH members AS (SELECT ` + hermesStateSessionColumns + `, ` + rev + ` AS revision FROM sessions` + where + `),
 metadata AS (SELECT session_id, ` + hermesMessageTokenColumns + ` FROM messages` + messageWhere + ` GROUP BY session_id)
 SELECT members.*, COALESCE(metadata.n,0), COALESCE(metadata.last_id,0), COALESCE(metadata.invalid_times,0)
 FROM members LEFT JOIN metadata ON metadata.session_id=members.id ORDER BY members.id`
}

func listHermesMemberTokens(ctx context.Context, conn *sql.DB, id string, yield func(string, string) error) error {
	revision, err := hermesHasTranscriptRevision(ctx, conn)
	if err != nil {
		return err
	}
	usage, err := readHermesModelUsage(ctx, conn, id)
	if err != nil {
		return err
	}
	var args []any
	if id != "" {
		args = []any{id, id}
	}
	rows, err := conn.QueryContext(ctx, hermesMemberTokenQuery(revision, id != ""), args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		// Scan raw values so a malformed session still gets selected and follows
		// the normal member-error/transcript-fallback path.
		values := make([]any, 22)
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
		if err := yield(sid, hex.EncodeToString(h.Sum(nil))); err != nil {
			return err
		}
	}
	return rows.Err()
}

func hermesMemberToken(ctx context.Context, conn *sql.DB, id string) (string, error) {
	var token string
	err := listHermesMemberTokens(ctx, conn, id, func(_ string, value string) error { token = value; return nil })
	return token, err
}

func hermesStoredMemberToken(value string) (string, bool) {
	parts := strings.Split(value, ":")
	if len(parts) != 3 || parts[0] != "hst1" || len(parts[1]) != 64 || len(parts[2]) != 64 {
		return "", false
	}
	for _, part := range parts[1:] {
		if _, err := hex.DecodeString(part); err != nil {
			return "", false
		}
	}
	return parts[1], true
}

var hermesTriggerComments = regexp.MustCompile(`--[^\n]*`)
var hermesRevisionTrigger = regexp.MustCompile(`(?is)^create\s+trigger\s+(?:if\s+not\s+exists\s+)?\w+\s+after\s+update\s+of\s+([\w,\s]+)\s+on\s+messages\s+begin\s+update\s+sessions\s+set\s+transcript_revision\s*=\s*transcript_revision\s*\+\s*1\s+where\s+id\s*=\s*new\.session_id\s*;\s*end\s*;?$`)

// Recheck on every event: an older Hermes writer can recreate a shorter
// trigger on opening the store. Unrecognized definitions fail closed.
func hermesStoreTracksRewrites(ctx context.Context, conn *sql.DB) (bool, error) {
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
		for _, column := range strings.Split(strings.ToLower(match[1]), ",") {
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
	defer conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK") // Finish this caller's read snapshot after cancellation.
	tracks, err := hermesStoreTracksRewrites(ctx, conn)
	if err != nil || !tracks {
		return nil, false, err
	}
	cursor := NewStoredMemberFreshnessCursor(pager)
	var sources []SourceRef
	orphan := func(row StoredMemberFreshness) error {
		if row.Suppressed {
			return nil
		}
		dbPath, id, ok := ParseVirtualSourcePathForBase(row.Path, "state.db")
		if !ok || !samePath(dbPath, stateDB) || !isValidHermesSessionID(id) {
			return nil
		}
		if path := findHermesSourceFile(sessionsDir, id); path != "" {
			source, ok := s.transcriptSourceRef(root, path)
			if ok {
				sources = append(sources, source)
			}
		}
		return nil
	}
	cursor.Passed = orphan
	var changed []string
	err = listHermesMemberTokens(ctx, conn, "", func(id, token string) error {
		row, found, err := cursor.Lookup(ctx, VirtualSourcePath(stateDB, id))
		if err != nil {
			return err
		}
		if found {
			if row.Suppressed {
				return nil
			}
			if stored, ok := hermesStoredMemberToken(row.FingerprintHash); ok && stored == token {
				return nil
			}
		}
		source := s.stateMemberSourceRef(root, stateDB, id)
		src := source.Opaque.(hermesSource)
		src.FingerprintSingleMember = true
		source.Opaque = src
		sources = append(sources, source)
		changed = append(changed, id)
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	if err := cursor.Rest(ctx, orphan); err != nil {
		return nil, false, err
	}
	// Close the streaming token query before using the one snapshot connection
	// for member readability. Only selected members need these indexed probes.
	for _, id := range changed {
		readable, err := hermesStateDBHasReadableSession(ctx, conn, id)
		if err != nil {
			return nil, false, err
		}
		if !readable {
			if err := orphan(StoredMemberFreshness{Path: VirtualSourcePath(stateDB, id)}); err != nil {
				return nil, false, err
			}
		}
	}
	return sources, true, nil
}
