package readbase

import (
	"strings"

	"go.kenn.io/agentsview/internal/db"
)

const MessageColumns = `id, session_id, ordinal, role, content, thinking_text,
	timestamp, has_thinking, has_tool_use, content_length,
	is_system, model, reasoning_effort, token_usage, context_tokens, output_tokens,
	provider_id,
	has_context_tokens, has_output_tokens, claude_message_id,
	claude_request_id, source_type, source_subtype, prompt_source, source_uuid,
	source_parent_uuid, is_sidechain, is_compact_boundary`

// SessionQueries builds SQL shared by mirror backends.
type SessionQueries struct{}

func (SessionQueries) StarredSessionIDsSQL() (string, []any) {
	return `SELECT session_id FROM starred_sessions ORDER BY created_at DESC, session_id DESC`, nil
}

func (SessionQueries) PinnedMessagesSQL(sessionID, project string) (string, []any) {
	if sessionID != "" {
		return `SELECT id, session_id, message_id, ordinal, note, created_at
			FROM pinned_messages WHERE session_id = ?
			ORDER BY created_at DESC, id DESC`, []any{sessionID}
	}
	query := `SELECT p.id, p.session_id, p.message_id, p.ordinal,
		p.note, p.created_at, m.content, m.role,
		s.project, s.agent, COALESCE(s.display_name, s.session_name), s.first_message
		FROM pinned_messages p
		JOIN sessions s ON p.session_id = s.id AND s.deleted_at IS NULL
		LEFT JOIN messages m ON p.session_id = m.session_id AND p.message_id = m.id`
	args := []any{}
	if project != "" {
		query += " WHERE s.project = ?"
		args = append(args, project)
	}
	return query + " ORDER BY p.created_at DESC, p.id DESC LIMIT 500", args
}

func (SessionQueries) TrashedSessionsSQL(columns string) string {
	return "SELECT " + columns + " FROM sessions WHERE deleted_at IS NOT NULL ORDER BY deleted_at DESC LIMIT 500"
}

func (SessionQueries) ChildSessionsSQL(columns string) string {
	return "SELECT " + columns + ` FROM sessions
		WHERE parent_session_id = ? AND deleted_at IS NULL
		ORDER BY COALESCE(started_at, created_at) ASC`
}

func (SessionQueries) AllMessagesSQL() string {
	return "SELECT " + MessageColumns + " FROM messages WHERE session_id = ? ORDER BY ordinal ASC"
}

func (SessionQueries) SessionVersionSQL() string {
	return `SELECT message_count, file_mtime, file_hash,
		COALESCE(local_modified_at, ended_at, started_at, created_at),
		COALESCE(agentsview_push_fingerprint, '')
		FROM sessions WHERE id = ?`
}

func CountSessionsSQL(countExpr, where string) string {
	return "SELECT " + countExpr + " FROM sessions WHERE " + where
}

func SecretFindingsSQL(f db.SecretFindingFilter, dateExpr, dateParam, tieBreakColumn string) (string, []any) {
	var preds []string
	args := []any{}
	add := func(pred string, value any) {
		preds = append(preds, pred+" ?")
		args = append(args, value)
	}
	if f.Project != "" {
		add("s.project =", f.Project)
	}
	if f.Agent != "" {
		add("s.agent =", f.Agent)
	}
	if f.Rule != "" {
		add("sf.rule_name =", f.Rule)
	}
	if f.Confidence != "" && f.Confidence != "all" {
		add("sf.confidence =", f.Confidence)
	}
	if len(f.RulesVersions) > 0 {
		var ph []string
		for _, v := range f.RulesVersions {
			if v == "" {
				continue
			}
			ph = append(ph, "?")
			args = append(args, v)
		}
		if len(ph) > 0 {
			preds = append(preds, "sf.rules_version IN ("+strings.Join(ph, ",")+")")
		}
	}
	if f.DateFrom != "" {
		preds = append(preds, dateExpr+" >= "+dateParam)
		args = append(args, f.DateFrom)
	}
	if f.DateTo != "" {
		preds = append(preds, dateExpr+" <= "+dateParam)
		args = append(args, f.DateTo)
	}
	where := "s.deleted_at IS NULL"
	if len(preds) > 0 {
		where += " AND " + strings.Join(preds, " AND ")
	}
	args = append(args, f.Limit+1, f.Cursor)
	return `SELECT sf.session_id, sf.rule_name, sf.confidence,
		sf.location_kind, sf.message_ordinal, sf.call_index,
		sf.event_index, sf.match_start, sf.match_end,
		sf.match_index, sf.redacted_match, sf.rules_version, s.project, s.agent
		FROM secret_findings sf JOIN sessions s ON s.id = sf.session_id
		WHERE ` + where + `
		ORDER BY COALESCE(s.ended_at, s.started_at, s.created_at) DESC,
			sf.session_id, sf.message_ordinal, sf.match_start, sf.match_index, ` + tieBreakColumn + `
		LIMIT ? OFFSET ?`, args
}

func (SessionQueries) SidebarSQL(f db.SessionFilter, dialect db.QueryDialect) (string, []any) {
	where, args := db.BuildSessionFilterSQL(f, dialect)
	return `SELECT id, parent_session_id, relationship_type,
  project, project_assigned, machine, agent, agent_label, entrypoint, session_kind,
  COALESCE(display_name, session_name) AS display_name,
  started_at, ended_at, created_at, termination_status, message_count,
  user_message_count, transcript_revision, is_automated, ` +
		dialect.Position("COALESCE(first_message, '')", "'<teammate-message'") + ` > 0
  FROM sessions WHERE ` + where + `
  ORDER BY COALESCE(ended_at, started_at, created_at) DESC, id DESC`, args
}
