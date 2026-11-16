package db

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ConversationToolSummary describes what a session's tool calls did, without
// their names, arguments or results. Calls overlap, so DurationMs merges them
// and the category durations do not add up to it.
type ConversationToolSummary struct {
	DurationMs int64                      `json:"duration_ms"`
	Categories []ConversationToolCategory `json:"categories"`
	Skills     []ConversationSkillUse     `json:"skills"`
}

// ConversationToolCategory totals one tool category. Observed counts the calls
// whose result was classified, and Errored the ones classified as an error.
// Timed counts the calls with a measured interval; DurationMs covers only
// those.
type ConversationToolCategory struct {
	Category   string `json:"category"`
	Calls      int    `json:"calls"`
	Observed   int    `json:"observed"`
	Errored    int    `json:"errored"`
	Timed      int    `json:"timed"`
	DurationMs int64  `json:"duration_ms"`
}

type ConversationSkillUse struct {
	Name  string `json:"name"`
	Calls int    `json:"calls"`
}

const conversationToolChangeType = "tool_summary"

// conversationToolRevisionKey counts tool journal entries. It is separate from
// the publication revision, so the journal changes no message or session
// revision and a listing without summaries reads exactly as before.
const conversationToolRevisionKey = "conversation_tool_revision"

// conversationToolChangesTx lists the sessions journaled in (after, through],
// oldest first, up to limit, and reports whether more remain. Each change
// takes the session's current trash state and usage policy gap; the summary
// is attached afterwards from the same snapshot.
func conversationToolChangesTx(ctx context.Context, tx *sql.Tx, after, through int64, limit int) ([]ConversationChange, bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT t.session_id,CAST(t.revision AS TEXT),s.deleted_at IS NOT NULL,
		 CASE WHEN EXISTS(SELECT 1 FROM conversation_session_changes c WHERE c.session_id=t.session_id AND c.gap='archive_content_excluded')
		  THEN 'archive_content_excluded' ELSE '' END
		FROM conversation_tool_changes t JOIN sessions s ON s.id=t.session_id
		WHERE t.revision > ? AND t.revision <= ? ORDER BY t.revision LIMIT ?`, after, through, limit+1)
	if err != nil {
		return nil, false, fmt.Errorf("reading session tool changes: %w", err)
	}
	defer rows.Close()
	var changes []ConversationChange
	for rows.Next() {
		change := ConversationChange{Type: conversationToolChangeType}
		if err := rows.Scan(&change.SessionID, &change.Revision, &change.Deleted, &change.Gap); err != nil {
			return nil, false, err
		}
		changes = append(changes, change)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, false, err
	}
	if len(changes) > limit {
		return changes[:limit], true, nil
	}
	return changes, false, nil
}

// The two statements that journal one session: advance the counter, then give
// the session the new revision.
const conversationToolAdvanceSQL = `INSERT INTO archive_metadata(key,value) VALUES ('` + conversationToolRevisionKey + `','1')
 ON CONFLICT(key) DO UPDATE SET value=CAST(value AS INTEGER)+1`

func conversationToolJournalSQL(session string) string {
	return `INSERT INTO conversation_tool_changes(session_id,revision)
 SELECT ` + session + `,CAST(value AS INTEGER) FROM archive_metadata WHERE key='` + conversationToolRevisionKey + `'
 ON CONFLICT(session_id) DO UPDATE SET revision=excluded.revision`
}

// journalConversationToolSessionTx journals a session whose tool call rows
// were rebuilt. A rewrite of an unchanged transcript leaves the transcript
// revision alone, and the rebuilt rows can carry a different classification,
// or none, so no trigger would see the change.
func journalConversationToolSessionTx(tx transactionQueries, sessionID string) error {
	active, err := conversationExportActiveTx(tx)
	if err != nil || !active {
		return err
	}
	if _, err := tx.Exec(conversationToolAdvanceSQL); err != nil {
		return fmt.Errorf("advancing the session tool journal: %w", err)
	}
	if _, err := tx.Exec(conversationToolJournalSQL("?"), sessionID); err != nil {
		return fmt.Errorf("journaling session tool summary: %w", err)
	}
	return nil
}

// conversationToolSchemaSQL journals a session whenever an input of its tool
// summary changes:
//
//   - its tool calls: every write that adds, replaces or links them advances
//     the transcript revision, so the session row covers them and no row
//     trigger slows tool call inserts or deletes;
//   - the classification of a call's result, which is written later. A
//     write that rebuilds tool call rows journals the session itself
//     (journalConversationToolSessionTx);
//   - its start and end, which clip measured time;
//   - the start, end and existence of a subagent session, which time the
//     parent's delegating call;
//   - its trash state and its usage policy gap.
var conversationToolSchemaSQL = conversationToolTriggersSQL()

func conversationToolTriggersSQL() string {
	const active = `EXISTS(SELECT 1 FROM archive_metadata WHERE key='` + conversationExportInitializedKey + `')`
	journal := func(session string) string {
		return `
 ` + conversationToolAdvanceSQL + `;
 ` + conversationToolJournalSQL(session) + `;`
	}
	// The sessions that delegate to a subagent session, one revision each so
	// a walk can page between them. Their revisions follow the counter, so
	// the highest one journaled is the counter's next value.
	journalParents := func(subagent string) string {
		const parents = `FROM tool_calls WHERE subagent_session_id=`
		return `
 INSERT INTO conversation_tool_changes(session_id,revision)
 SELECT session_id,COALESCE((SELECT CAST(value AS INTEGER) FROM archive_metadata WHERE key='` + conversationToolRevisionKey + `'),0)
  +ROW_NUMBER() OVER (ORDER BY session_id)
 FROM (SELECT DISTINCT session_id ` + parents + subagent + `) WHERE true
 ON CONFLICT(session_id) DO UPDATE SET revision=excluded.revision;
 INSERT INTO archive_metadata(key,value)
 SELECT '` + conversationToolRevisionKey + `',CAST(revision AS TEXT) FROM conversation_tool_changes
 WHERE EXISTS(SELECT 1 ` + parents + subagent + `) ORDER BY revision DESC LIMIT 1
 ON CONFLICT(key) DO UPDATE SET value=excluded.value;`
	}
	changed := func(columns ...string) string {
		parts := make([]string, len(columns))
		for i, column := range columns {
			parts[i] = "OLD." + column + " IS NOT NEW." + column
		}
		return "(" + strings.Join(parts, " OR ") + ")"
	}
	var sb strings.Builder
	trigger := func(name, event, when, body string) {
		fmt.Fprintf(&sb, "DROP TRIGGER IF EXISTS %s;\nCREATE TRIGGER %s AFTER %s\nWHEN %s\nBEGIN%s\nEND;\n", name, name, event, when, body)
	}
	trigger("conversation_tool_session_insert", "INSERT ON sessions", active, journal("NEW.id")+journalParents("NEW.id"))
	// Trashing and restoring republish the summary, as they do each message.
	trigger("conversation_tool_session_update", "UPDATE OF transcript_revision,deleted_at,started_at,ended_at ON sessions",
		changed("transcript_revision", "deleted_at", "started_at", "ended_at")+" AND "+active, journal("NEW.id"))
	trigger("conversation_tool_subagent_timing", "UPDATE OF started_at,ended_at ON sessions",
		changed("started_at", "ended_at")+" AND "+active, journalParents("NEW.id"))
	trigger("conversation_tool_call_outcome", "UPDATE OF observed_outcome ON tool_calls",
		changed("observed_outcome")+" AND "+active, journal("NEW.session_id"))
	// The usage policy can settle a stored session without rewriting it. The
	// session's change row is created by its first session change.
	trigger("conversation_tool_session_gap_insert", "INSERT ON conversation_session_changes",
		"NEW.gap!='' AND "+active, journal("NEW.session_id"))
	trigger("conversation_tool_session_gap_update", "UPDATE OF gap ON conversation_session_changes",
		changed("gap")+" AND "+active, journal("NEW.session_id"))
	// A deleted session publishes its own deletion and has no summary left.
	trigger("conversation_tool_session_delete", "DELETE ON sessions", "1", `
 DELETE FROM conversation_tool_changes WHERE session_id=OLD.id;`)
	trigger("conversation_tool_subagent_delete", "DELETE ON sessions", active, journalParents("OLD.id"))
	return sb.String()
}

// ensureConversationToolSchema installs the tool change journal and, on an
// active archive that predates it, journals the sessions already stored.
func ensureConversationToolSchema(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, conversationToolSchemaSQL); err != nil {
		return err
	}
	active, err := conversationExportActiveTx(contextTransaction{ctx: ctx, tx: tx})
	if err != nil || !active {
		return err
	}
	return journalConversationToolSessionsTx(ctx, tx)
}

// journalConversationToolSessionsTx gives every session without a journal row
// its own journal revision, so a walk can page between any two of them.
// The triggers journal every later session, so later opens find nothing to add.
func journalConversationToolSessionsTx(ctx context.Context, tx *sql.Tx) error {
	base, err := archiveJournalRevision(ctx, tx, conversationToolRevisionKey, "conversation tool")
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO conversation_tool_changes(session_id,revision)
	 SELECT id,?+ROW_NUMBER() OVER (ORDER BY id) FROM sessions s
	 WHERE NOT EXISTS(SELECT 1 FROM conversation_tool_changes t WHERE t.session_id=s.id)`, base)
	if err != nil {
		return fmt.Errorf("journaling session tool summaries: %w", err)
	}
	added, err := result.RowsAffected()
	if err != nil || added == 0 {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO archive_metadata(key,value) VALUES(?,?)
	 ON CONFLICT(key) DO UPDATE SET value=excluded.value`, conversationToolRevisionKey, strconv.FormatInt(base+added, 10))
	return err
}

// A read-only open does not migrate, so an archive last written by an older
// version has no journal to read.
func requireConversationToolSchema(ctx context.Context, tx *sql.Tx) error {
	var present bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='conversation_tool_changes')`).Scan(&present); err != nil {
		return err
	}
	if !present {
		return &SchemaUpgradeRequiredError{Table: "conversation_tool_changes", Column: "session_id"}
	}
	return nil
}

// attachConversationToolSummaries computes the summary of each live tool
// summary change from the stored calls. A session under the usage storage
// policy keeps its gap and gets no summary: its calls were removed, so their
// absence is not a count of zero.
func attachConversationToolSummaries(ctx context.Context, tx *sql.Tx, changes []ConversationChange, now time.Time) error {
	var ids []string
	for _, change := range changes {
		if change.Type == conversationToolChangeType && !change.Deleted && change.Gap == "" {
			ids = append(ids, change.SessionID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	encoded, err := json.Marshal(ids)
	if err != nil {
		return err
	}
	sessions := make(map[string]*Session, len(ids))
	rows, err := tx.QueryContext(ctx, `SELECT id,started_at,ended_at FROM sessions WHERE id IN (SELECT value FROM json_each(?))`, string(encoded))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var session Session
		if err := rows.Scan(&session.ID, &session.StartedAt, &session.EndedAt); err != nil {
			return err
		}
		sessions[session.ID] = &session
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	type call struct {
		row     CallRow
		outcome string
	}
	calls := make(map[string][]call, len(ids))
	rows, err = tx.QueryContext(ctx, `SELECT tc.session_id,tc.category,TRIM(COALESCE(tc.skill_name,'')),COALESCE(tc.observed_outcome,''),
		 tc.subagent_session_id,`+toolExecutionStartedSQL+`,`+toolExecutionCompletedSQL+`,s_sub.started_at,s_sub.ended_at
		FROM tool_calls tc
		JOIN messages m ON m.id = tc.message_id
		LEFT JOIN sessions s_sub ON s_sub.id = tc.subagent_session_id
		WHERE tc.session_id IN (SELECT value FROM json_each(?))`, string(encoded))
	if err != nil {
		return fmt.Errorf("reading session tool calls: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var c call
		var sessionID, skill string
		var executionStart, executionEnd, subagentStart, subagentEnd sql.NullString
		if err := rows.Scan(&sessionID, &c.row.Category, &skill, &c.outcome, &c.row.SubagentSessionID,
			&executionStart, &executionEnd, &subagentStart, &subagentEnd); err != nil {
			return err
		}
		if skill != "" {
			c.row.SkillName = &skill
		}
		c.row.ExecutionStart, c.row.ExecutionEnd = executionStart.String, executionEnd.String
		c.row.SubagentStart, c.row.SubagentEnd = subagentStart.String, subagentEnd.String
		calls[sessionID] = append(calls[sessionID], c)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for i := range changes {
		change := &changes[i]
		session := sessions[change.SessionID]
		if change.Type != conversationToolChangeType || change.Deleted || change.Gap != "" || session == nil {
			continue
		}
		sessionCalls := calls[change.SessionID]
		callRows := make([]CallRow, len(sessionCalls))
		for j, c := range sessionCalls {
			callRows[j] = c.row
		}
		// Session timing measures the intervals, so these durations match the
		// session's timing view.
		timing := &SessionTiming{Running: session.EndedAt == nil || *session.EndedAt == ""}
		intervals := assembleTurnActivity(timing, session, nil, callRows, now)
		summary := &ConversationToolSummary{DurationMs: timing.ToolDurationMs, Categories: []ConversationToolCategory{}, Skills: []ConversationSkillUse{}}
		categories := map[string]ConversationToolCategory{}
		skills := map[string]int{}
		for _, total := range timing.ByCategory {
			categories[total.Category] = ConversationToolCategory{Category: total.Category, Calls: total.CallCount, DurationMs: total.DurationMs}
		}
		for j, c := range sessionCalls {
			category := categories[c.row.Category]
			switch c.outcome {
			case "errored":
				category.Errored++
				category.Observed++
			case "empty", "content":
				category.Observed++
			}
			if intervals[j] != nil {
				category.Timed++
			}
			categories[c.row.Category] = category
			if c.row.SkillName != nil {
				skills[*c.row.SkillName]++
			}
		}
		for _, category := range categories {
			summary.Categories = append(summary.Categories, category)
		}
		slices.SortFunc(summary.Categories, func(a, b ConversationToolCategory) int { return strings.Compare(a.Category, b.Category) })
		for name, count := range skills {
			summary.Skills = append(summary.Skills, ConversationSkillUse{Name: name, Calls: count})
		}
		slices.SortFunc(summary.Skills, func(a, b ConversationSkillUse) int { return strings.Compare(a.Name, b.Name) })
		change.ToolSummary = summary
	}
	return nil
}
