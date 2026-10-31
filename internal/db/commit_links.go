package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"net/url"
	"strings"

	"go.kenn.io/agentsview/internal/commitlinks"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/secrets"
	"go.kenn.io/agentsview/internal/stringutil"
)

const commitExtractorVersion = 1
const maxCommitSessionBytes = 16 << 20

type commitSessionInput struct {
	id, revision, project, cwd, machine, agent, started string
	deleted                                             sql.NullString
}

func readCommitSession(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id string) (commitSessionInput, error) {
	s := commitSessionInput{id: id}
	err := q.QueryRowContext(ctx, `SELECT COALESCE(transcript_revision,''),project,COALESCE(cwd,''),machine,agent,COALESCE(started_at,''),deleted_at FROM sessions WHERE id=?`, id).Scan(&s.revision, &s.project, &s.cwd, &s.machine, &s.agent, &s.started, &s.deleted)
	return s, err
}

func commitSessionPath(id string) string {
	prefix, rest, found := strings.Cut(id, ":")
	if found {
		return "/sessions/" + url.PathEscape(prefix) + "/" + url.PathEscape(rest)
	}
	return "/sessions/" + url.PathEscape(id)
}

func unavailableCommit(link *CommitLink) {
	link.Session = nil
	link.Gone = true
	link.Turn = nil
	link.Agent = ""
	link.Project = ""
	link.SessionStartedAt = ""
	link.WebURL = ""
}

func hydrateCommit(link *CommitLink, s commitSessionInput, revision string) {
	boundID := ""
	if link.Session != nil {
		boundID = link.Session.ID
	}
	link.SessionID = s.id
	link.Session = &CommitSession{ID: s.id}
	link.Gone = false
	link.Agent = s.agent
	link.Project = s.project
	link.SessionStartedAt = s.started
	link.WebURL = commitSessionPath(s.id)
	if revision != s.revision || boundID != "" && boundID != s.id {
		link.Turn = nil
	}
}

func commitEvidenceID(link CommitLink, printed string) string {
	values := []any{link.SessionID, printed, link.Source, link.MessageOrdinal, link.CallIndex, link.RepositoryPath}
	encoded, _ := json.Marshal(values)
	hash := sha256.Sum256(encoded)
	return hex.EncodeToString(hash[:])
}

func sanitizeCommit(link *CommitLink) {
	link.Subject = stringutil.SafeTruncate(secrets.Redact(link.Subject), 4096)
	link.Branch = stringutil.SafeTruncate(link.Branch, 1024)
	if len(link.RepositoryPath) > 4096 {
		link.RepositoryPath = ""
	}
	link.CommitSHA = strings.ToLower(link.CommitSHA)
	link.Ambiguous = !link.Resolved
	if link.Turn != nil {
		t := *link.Turn
		t.Prompt = stringutil.TruncateRunes(secrets.Redact(t.Prompt), 600, "")
		link.Turn = &t
	}
}

func storeCommitLinkTx(ctx context.Context, tx *sql.Tx, link CommitLink, revision, projectHint, printed, id string) error {
	sanitizeCommit(&link)
	if err := ValidateCommitSHAs([]string{link.CommitSHA}); err != nil {
		return err
	}
	if link.Source != "transcript" && link.Source != "trailer" {
		return errors.New("invalid commit link source")
	}
	if link.Resolved && len(link.CommitSHA) != 40 {
		return ErrInvalidCommitQuery
	}
	if printed == "" {
		printed = link.CommitSHA
	}
	if id == "" {
		id = commitEvidenceID(link, printed)
	}
	payload, err := json.Marshal(link)
	if err != nil {
		return err
	}
	if len(payload) > 64<<10 {
		return ErrCommitLookupTooLarge
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO commit_session_links(id,session_id,commit_sha,source,printed_sha,project_hint,source_revision,payload)
 VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET commit_sha=excluded.commit_sha,project_hint=excluded.project_hint,source_revision=excluded.source_revision,payload=excluded.payload`, id, link.SessionID, link.CommitSHA, link.Source, printed, projectHint, revision, string(payload))
	return err
}

// StoreCommitLinks records explicit observations. Unknown identities retain facts
// without any supplied session text. Sync owns repository discovery, never reads.
func (d *DB) StoreCommitLinks(ctx context.Context, links []CommitLink) error {
	return d.storeCommitLinks(ctx, links, nil, false, true)
}

func (d *DB) storeCommitLinks(ctx context.Context, links []CommitLink, paths []string, complete, derive bool) error {
	if len(paths) > 256 {
		paths = nil
		complete = false
	}
	encodedPaths, err := json.Marshal(paths)
	if err != nil {
		return err
	}
	if len(encodedPaths) > 64<<10 {
		paths = nil
		complete = false
		encodedPaths = []byte("[]")
	}
	if !complete {
		paths = nil
	}
	if d.ArchiveContent() == config.ArchiveContentUsage {
		paths = nil
		complete = false
		encodedPaths = []byte("[]")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.getWriter().Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, link := range links {
		nativeID := link.SessionID
		if link.Source == "transcript" && d.ArchiveContent() != config.ArchiveContentFull {
			continue
		}
		s, err := resolveCommitSession(ctx, tx, &link)
		revision, hint := "", ""
		if err == nil && !s.deleted.Valid {
			revision, hint = s.revision, s.project
			hydrateCommit(&link, s, revision)
			if derive && link.Source == "trailer" && d.ArchiveContent() != config.ArchiveContentUsage {
				link.Turn, err = trailerTurnTx(ctx, tx, s, link.CommittedAt, paths)
				if err != nil {
					return err
				}
			}
		} else if errors.Is(err, sql.ErrNoRows) || err == nil {
			unavailableCommit(&link)
		} else {
			return err
		}
		if d.ArchiveContent() == config.ArchiveContentUsage {
			link.Subject = ""
			link.RepositoryPath = ""
			link.Branch = ""
			link.Turn = nil
		}
		if !derive && err == nil {
			if _, err := tx.ExecContext(ctx, "DELETE FROM commit_link_state WHERE session_id=?", s.id); err != nil {
				return err
			}
		}
		link.SessionID = nativeID
		// A rescan has no new project evidence when its owner is unavailable.
		// Preserve the retained scope saved by permanent deletion.
		if link.Gone && hint == "" {
			err := tx.QueryRowContext(ctx, "SELECT project_hint FROM commit_session_links WHERE id=?", commitEvidenceID(link, link.CommitSHA)).Scan(&hint)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		if err := storeCommitLinkTx(ctx, tx, link, revision, hint, "", ""); err != nil {
			return err
		}
		if link.Source == "trailer" {
			if _, err := tx.ExecContext(ctx, "UPDATE commit_session_links SET changed_paths=?,paths_complete=? WHERE id=?", string(encodedPaths), complete, commitEvidenceID(link, link.CommitSHA)); err != nil {
				return err
			}
		}
	}
	if err := markCommitIndexReady(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

func realCommitPrompt(m Message) bool {
	if m.Role != "user" || m.IsSystem || IsSystemPrefixed(m.Content, m.Role) {
		return false
	}
	if m.SourceSubtype == "function_call_output" || m.SourceSubtype == "tool_result" || m.SourceType == "tool_result" {
		return false
	}
	text := m.Content
	if m.DialogueText != nil {
		text = *m.DialogueText
	}
	return strings.TrimSpace(text) != ""
}

func commitPrompt(m Message, why string) *CommitTurn {
	text := m.Content
	if m.DialogueText != nil {
		text = *m.DialogueText
	}
	return &CommitTurn{Ordinal: m.Ordinal, At: m.Timestamp, Prompt: stringutil.TruncateRunes(secrets.Redact(text), 600, ""), Why: why}
}

// RefreshCommitLinks derives one session from one bounded read snapshot and
// publishes only if all source inputs still agree. It performs no host I/O.
func (d *DB) RefreshCommitLinks(ctx context.Context, id string) error {
	release, err := d.acquireBulkRead(ctx)
	if err != nil {
		return err
	}
	tx, err := d.getReader().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		release()
		return err
	}
	s, err := readCommitSession(ctx, tx, id)
	if errors.Is(err, sql.ErrNoRows) {
		tx.Rollback()
		release()
		return nil
	}
	if err != nil {
		tx.Rollback()
		release()
		return err
	}
	policy := d.ArchiveContent()
	oversized := false
	var currentState bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM commit_link_state WHERE session_id=? AND revision=? AND project=? AND cwd=? AND machine=? AND policy=? AND extractor_version=?)`, id, s.revision, s.project, s.cwd, s.machine, string(policy), commitExtractorVersion).Scan(&currentState)
	if err != nil {
		tx.Rollback()
		release()
		return err
	}
	if currentState {
		tx.Rollback()
		release()
		return nil
	}
	var bytes int64
	err = tx.QueryRowContext(ctx, `SELECT
 COALESCE((SELECT SUM(length(CAST(content AS BLOB))+length(CAST(COALESCE(thinking_text,'') AS BLOB))+length(CAST(COALESCE(dialogue_text,'') AS BLOB))+length(CAST(COALESCE(token_usage,'') AS BLOB))) FROM messages WHERE session_id=?),0)+
 COALESCE((SELECT SUM(length(CAST(COALESCE(input_json,'') AS BLOB))+length(CAST(COALESCE(result_content,'') AS BLOB))) FROM tool_calls WHERE session_id=?),0)+
 COALESCE((SELECT SUM(length(CAST(content AS BLOB))) FROM tool_result_events WHERE session_id=?),0)`, id, id, id).Scan(&bytes)
	if err != nil {
		tx.Rollback()
		release()
		return err
	}
	oversized = bytes > maxCommitSessionBytes
	var tooMany bool
	if err := tx.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM messages WHERE session_id=?)>10000 OR (SELECT COUNT(*) FROM tool_calls WHERE session_id=?)>20000 OR (SELECT COUNT(*) FROM tool_result_events WHERE session_id=?)>20000`, id, id, id).Scan(&tooMany); err != nil {
		tx.Rollback()
		release()
		return err
	}
	oversized = oversized || tooMany
	var msgs []Message
	if !s.deleted.Valid && !oversized && policy == config.ArchiveContentFull {
		for from := 0; ; {
			page, err := getMessagesWith(ctx, tx, id, from, 1000, true)
			if err != nil {
				tx.Rollback()
				release()
				return err
			}
			msgs = append(msgs, page...)
			if len(page) < 1000 {
				break
			}
			from = page[len(page)-1].Ordinal + 1
		}
	}
	rollbackErr := tx.Rollback()
	release()
	if rollbackErr != nil {
		return rollbackErr
	}
	var links []CommitLink
	var prompt *CommitTurn
	for _, m := range msgs {
		if realCommitPrompt(m) {
			prompt = commitPrompt(m, "commit-call")
		}
		for _, call := range m.ToolCalls {
			if call.Category != "Bash" {
				continue
			}
			command, dir := commitlinks.CommandInput(call.InputJSON, s.cwd)
			inv, ok := commitlinks.CommitCommand(command, dir)
			if !ok {
				continue
			}
			for _, summary := range commitlinks.ParseSummaries(call.ResultContent) {
				ordinal, index := m.Ordinal, call.CallIndex
				link := CommitLink{SessionID: id, CommitSHA: summary.SHA, Source: "transcript", MessageOrdinal: &ordinal, CallIndex: &index, RepositoryPath: inv.Repository, Branch: summary.Branch, Subject: summary.Subject, CommittedAt: m.Timestamp, Turn: prompt}
				hydrateCommit(&link, s, s.revision)
				links = append(links, link)
			}
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	w, err := d.getWriter().Begin(ctx)
	if err != nil {
		return err
	}
	defer w.Rollback()
	current, err := readCommitSession(ctx, w, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if current != s || d.ArchiveContent() != policy {
		return nil
	}
	// Preserve successful Git resolution when unchanged transcript evidence is refreshed.
	for i := range links {
		key := commitEvidenceID(links[i], links[i].CommitSHA)
		var old string
		if err := w.QueryRowContext(ctx, "SELECT payload FROM commit_session_links WHERE id=?", key).Scan(&old); err == nil {
			var previous CommitLink
			if json.Unmarshal([]byte(old), &previous) == nil && previous.Resolved {
				links[i].CommitSHA = previous.CommitSHA
				links[i].Resolved = true
				links[i].CommittedAt = previous.CommittedAt
			}
		}
	}
	if _, err := w.ExecContext(ctx, "DELETE FROM commit_session_links WHERE session_id=? AND source='transcript'", id); err != nil {
		return err
	}
	for _, link := range links {
		printed := ""
		// The current call's printed SHA remains stable across resolution.
		for _, m := range msgs {
			if link.MessageOrdinal != nil && m.Ordinal == *link.MessageOrdinal {
				for _, c := range m.ToolCalls {
					if link.CallIndex != nil && c.CallIndex == *link.CallIndex {
						for _, v := range commitlinks.ParseSummaries(c.ResultContent) {
							if strings.HasPrefix(link.CommitSHA, v.SHA) && len(v.SHA) > len(printed) {
								printed = v.SHA
							}
						}
					}
				}
			}
		}
		if printed == "" {
			printed = link.CommitSHA
		}
		if err := storeCommitLinkTx(ctx, w, link, s.revision, s.project, printed, ""); err != nil {
			return err
		}
	}
	// Trailer turns follow the same revision-conditional publication. This also
	// enriches retained trailers when their session arrives after the Git scan.
	var trailerBytes int64
	var trailerCount int
	if err := w.QueryRowContext(ctx, "SELECT COUNT(*),COALESCE(SUM(length(CAST(payload AS BLOB))+length(CAST(changed_paths AS BLOB))),0) FROM commit_session_links WHERE source='trailer' AND (session_id=? OR session_id IN (SELECT parser_id FROM raw_archive_sessions WHERE session_id=?))", id, id).Scan(&trailerCount, &trailerBytes); err != nil {
		return err
	}
	deriveTrailers := trailerCount <= MaxCommitLinks && trailerBytes <= maxCommitSessionBytes && int64(trailerCount)*bytes <= maxCommitSessionBytes
	if !deriveTrailers {
		if _, err := w.ExecContext(ctx, "UPDATE commit_session_links SET payload=json_set(payload,'$.turn',json('null')) WHERE source='trailer' AND (session_id=? OR session_id IN (SELECT parser_id FROM raw_archive_sessions WHERE session_id=?))", id, id); err != nil {
			return err
		}
	}
	trailerRows, err := w.QueryContext(ctx, "SELECT id,payload,printed_sha,changed_paths,paths_complete FROM commit_session_links WHERE source='trailer' AND ? AND (session_id=? OR session_id IN (SELECT parser_id FROM raw_archive_sessions WHERE session_id=?)) LIMIT 4097", deriveTrailers, id, id)
	if err != nil {
		return err
	}
	type retained struct {
		id, raw, printed, paths string
		complete                bool
	}
	var trailers []retained
	for trailerRows.Next() {
		var r retained
		if err := trailerRows.Scan(&r.id, &r.raw, &r.printed, &r.paths, &r.complete); err != nil {
			trailerRows.Close()
			return err
		}
		trailers = append(trailers, r)
	}
	err = trailerRows.Err()
	trailerRows.Close()
	if err != nil {
		return err
	}
	for _, r := range trailers {
		var link CommitLink
		if err := json.Unmarshal([]byte(r.raw), &link); err != nil {
			return err
		}
		nativeID := link.SessionID
		bound, err := resolveCommitSession(ctx, w, &link)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		if bound.id != id {
			continue
		}
		var paths []string
		if r.complete && len(r.paths) <= 64<<10 {
			if err := json.Unmarshal([]byte(r.paths), &paths); err != nil {
				return err
			}
		}
		link.Turn = nil
		if !oversized && policy != config.ArchiveContentUsage {
			link.Turn, err = trailerTurnTx(ctx, w, s, link.CommittedAt, paths)
			if err != nil {
				return err
			}
		}
		hydrateCommit(&link, s, s.revision)
		link.SessionID = nativeID
		if policy == config.ArchiveContentUsage {
			link.Subject = ""
			link.Branch = ""
			link.RepositoryPath = ""
			link.Turn = nil
		}
		if err := storeCommitLinkTx(ctx, w, link, s.revision, s.project, r.printed, r.id); err != nil {
			return err
		}
		if policy == config.ArchiveContentUsage {
			if _, err := w.ExecContext(ctx, "UPDATE commit_session_links SET changed_paths='[]',paths_complete=0 WHERE id=?", r.id); err != nil {
				return err
			}
		}
	}
	_, err = w.ExecContext(ctx, `INSERT INTO commit_link_state(session_id,revision,project,cwd,machine,policy,extractor_version,unsupported) VALUES(?,?,?,?,?,?,?,?)
 ON CONFLICT(session_id) DO UPDATE SET revision=excluded.revision,project=excluded.project,cwd=excluded.cwd,machine=excluded.machine,policy=excluded.policy,extractor_version=excluded.extractor_version,unsupported=excluded.unsupported`, id, s.revision, s.project, s.cwd, s.machine, string(policy), commitExtractorVersion, oversized)
	if err != nil {
		return err
	}
	if err := markCommitIndexReady(ctx, w); err != nil {
		return err
	}
	return w.Commit()
}

func markCommitIndexReady(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `UPDATE commit_link_publication_state SET ready=1, revision=revision+1 WHERE singleton=1 AND ready=0 AND NOT EXISTS(
 SELECT 1 FROM sessions s LEFT JOIN commit_link_state st ON st.session_id=s.id WHERE s.deleted_at IS NULL AND st.session_id IS NULL)`)
	return err
}

// BackfillCommitLinks visits at most limit stale sessions. It is database-only,
// works on retained orphan archives, and resumes from durable per-session state.
func (d *DB) BackfillCommitLinks(ctx context.Context, limit int) (int, error) {
	if limit < 1 || limit > 100 {
		limit = 100
	}
	rows, err := d.getReader().QueryContext(ctx, `SELECT s.id FROM sessions s LEFT JOIN commit_link_state st ON st.session_id=s.id
 WHERE s.deleted_at IS NULL AND (st.session_id IS NULL OR st.revision<>COALESCE(s.transcript_revision,'') OR st.project<>s.project OR st.cwd<>s.cwd OR st.machine<>s.machine OR st.policy<>? OR st.extractor_version<>?) ORDER BY s.id LIMIT ?`, string(d.ArchiveContent()), commitExtractorVersion, limit)
	if err != nil {
		return 0, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	for i, id := range ids {
		if err := d.RefreshCommitLinks(ctx, id); err != nil {
			return i, err
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	w, err := d.getWriter().Begin(ctx)
	if err != nil {
		return len(ids), err
	}
	defer w.Rollback()
	if err := markCommitIndexReady(ctx, w); err != nil {
		return len(ids), err
	}
	return len(ids), w.Commit()
}

// CommitLinks reads stored observations only. Local visibility and source revision
// are evaluated in one transaction. No Git or source refresh is performed.
func (d *DB) CommitLinks(ctx context.Context, shas []string, id string) ([]CommitLink, error) {
	if err := ValidateCommitSHAs(shas); err != nil {
		return nil, err
	}
	if id == "" && len(shas) == 0 {
		return []CommitLink{}, nil
	}
	tx, err := d.getReader().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var ready int
	if err := tx.QueryRowContext(ctx, "SELECT ready FROM commit_link_publication_state WHERE singleton=1").Scan(&ready); err != nil {
		return nil, ErrCommitIndexUnavailable
	}
	if ready == 0 {
		return nil, ErrCommitIndexUnavailable
	}
	b := NewQueryBuilder(SQLiteQueryDialect(), 0)
	predicate := BuildCommitLinksPredicate(b, shas, id)
	if strings.Contains(id, "~") {
		predicate = "(" + predicate + " OR session_id IN (SELECT parser_id FROM raw_archive_sessions WHERE session_id=" + b.Add(id) + "))"
	}
	rows, err := tx.QueryContext(ctx, "SELECT CASE WHEN length(CAST(payload AS BLOB))>65536 THEN '' ELSE payload END,source_revision FROM commit_session_links WHERE "+predicate+" LIMIT 4097", b.Args()...)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return nil, ErrCommitIndexUnavailable
		}
		return nil, err
	}
	type record struct {
		link     CommitLink
		revision string
	}
	records := []record{}
	bytes := 0
	for rows.Next() {
		var raw, rev string
		if err := rows.Scan(&raw, &rev); err != nil {
			rows.Close()
			return nil, err
		}
		bytes += len(raw)
		if raw == "" || len(records) >= MaxCommitLinks || bytes > MaxCommitResponseBytes || len(raw) > 64<<10 {
			rows.Close()
			return nil, ErrCommitLookupTooLarge
		}
		var link CommitLink
		if err := json.Unmarshal([]byte(raw), &link); err != nil {
			rows.Close()
			return nil, err
		}
		records = append(records, record{link, rev})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	out := []CommitLink{}
	bindings := map[string]commitSessionBinding{}
	enrichedBytes := 2
	for _, r := range records {
		link := r.link
		if link.Source == "transcript" && d.ArchiveContent() != config.ArchiveContentFull {
			continue
		}
		key := link.Source + ":" + link.SessionID
		bound, ok := bindings[key]
		if !ok {
			bound, err = readCommitSessionBinding(ctx, tx, link)
			if err != nil {
				return nil, err
			}
			bindings[key] = bound
		}
		link.Ambiguous = link.Ambiguous || bound.ambiguous
		link.SessionID = bound.session.id
		if bound.missing {
			unavailableCommit(&link)
		} else if bound.session.deleted.Valid {
			continue
		} else {
			hydrateCommit(&link, bound.session, r.revision)
		}
		if id != "" && link.SessionID != id {
			continue
		}
		if len(shas) > 0 {
			matched := false
			for _, sha := range shas {
				matched = matched || CommitSHAMatches(link, sha)
			}
			if !matched {
				continue
			}
		}
		if d.ArchiveContent() == config.ArchiveContentUsage {
			link.Subject = ""
			link.Branch = ""
			link.RepositoryPath = ""
			link.Turn = nil
		}
		encoded, err := json.Marshal(link)
		if err != nil {
			return nil, err
		}
		enrichedBytes += len(encoded) + 1
		if enrichedBytes > MaxCommitResponseBytes {
			return nil, ErrCommitLookupTooLarge
		}
		out = append(out, link)
	}
	if len(shas) == 1 {
		MarkCommitAmbiguity(out, shas)
	} else {
		MarkCommitAmbiguity(out, nil)
	}
	SortCommitLinks(out)
	if err := CheckCommitResponseSize(out); err != nil {
		return nil, err
	}
	return out, nil
}

// ResolveCommitLink updates an observation in place, keeping its printed identity.
func (d *DB) ResolveCommitLink(ctx context.Context, id, sha, at string) error {
	if len(sha) != 40 || ValidateCommitSHAs([]string{sha}) != nil {
		return ErrInvalidCommitQuery
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.getWriter().Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var raw, rev, hint, printed string
	if err := tx.QueryRowContext(ctx, "SELECT payload,source_revision,project_hint,printed_sha FROM commit_session_links WHERE id=?", id).Scan(&raw, &rev, &hint, &printed); err != nil {
		return err
	}
	var link CommitLink
	if err := json.Unmarshal([]byte(raw), &link); err != nil {
		return err
	}
	if !strings.HasPrefix(strings.ToLower(sha), strings.ToLower(printed)) {
		return ErrInvalidCommitQuery
	}
	link.CommitSHA = strings.ToLower(sha)
	link.Resolved = true
	link.CommittedAt = at
	if err := storeCommitLinkTx(ctx, tx, link, rev, hint, printed, id); err != nil {
		return err
	}
	return tx.Commit()
}

// CommitLinkRevision changes for evidence and for session visibility/metadata.
func (d *DB) CommitLinkRevision(ctx context.Context) (int64, error) {
	var revision int64
	err := d.getReader().QueryRowContext(ctx, "SELECT revision FROM commit_link_publication_state WHERE singleton=1").Scan(&revision)
	return revision, err
}

// CommitLinkRecord is a bounded raw observation used by background resolution.
type CommitLinkRecord struct {
	ID   string
	Link CommitLink
}

func (d *DB) UnresolvedCommitLinks(ctx context.Context, limit int) ([]CommitLinkRecord, error) {
	return d.UnresolvedCommitLinksAfter(ctx, "", limit)
}

func (d *DB) UnresolvedCommitLinksAfter(ctx context.Context, after string, limit int) ([]CommitLinkRecord, error) {
	if limit < 1 || limit > 100 {
		limit = 100
	}
	rows, err := d.getReader().QueryContext(ctx, `SELECT id,payload FROM commit_session_links WHERE json_extract(payload,'$.resolved')=0 AND id>? ORDER BY id LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CommitLinkRecord{}
	for rows.Next() {
		var r CommitLinkRecord
		var raw string
		if err := rows.Scan(&r.ID, &raw); err != nil {
			return nil, err
		}
		if len(raw) > 64<<10 {
			return nil, ErrCommitLookupTooLarge
		}
		if err := json.Unmarshal([]byte(raw), &r.Link); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func applyCommitContentPolicyTx(ctx context.Context, tx *sql.Tx, policy config.ArchiveContent) error {
	if policy != config.ArchiveContentFull {
		if _, err := tx.ExecContext(ctx, "DELETE FROM commit_session_links WHERE source='transcript'"); err != nil {
			return err
		}
	}
	if policy == config.ArchiveContentUsage {
		_, err := tx.ExecContext(ctx, `UPDATE commit_session_links SET changed_paths='[]',paths_complete=0,payload=json_set(payload,'$.subject','','$.repository_path','','$.branch','','$.turn',json('null'))`)
		return err
	}
	return nil
}
