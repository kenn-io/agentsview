package clickhouse

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

// fingerprintProgressStride bounds how often the preparing phase reports.
const fingerprintProgressStride = 200

// sessionFingerprint identifies a session's local state. full covers
// everything a push writes for the session and is stored as
// agentsview_push_fingerprint; a push skips a session whose full
// fingerprint, archive, and dashboard facts already match. content covers
// everything outside the session row and is stored as
// agentsview_content_fingerprint; a push that finds only the content
// unchanged rewrites just the session row.
type sessionFingerprint struct {
	full, content string
}

// sessionFingerprints hashes each candidate's local content: the mirrored
// session scalars, every message with its tool calls and result events,
// the usage-event fingerprint, the tool-call fingerprint (file_path and
// call_index are json:"-" on ToolCall), secret findings, pins, commit
// links, and successful image text. It also returns how many message rows
// a push stores for each session, which a usage-only archive keeps fewer of
// than the session's message count.
func (s *Sync) sessionFingerprints(
	ctx context.Context, sessions []db.Session, onProgress func(storage.PushProgress),
) (fingerprints map[string]sessionFingerprint, messageRows map[string]int, err error) {
	fingerprints = make(map[string]sessionFingerprint, len(sessions))
	messageRows = make(map[string]int, len(sessions))
	for i, sess := range sessions {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		payload, err := s.loadPayload(ctx, sess)
		if err != nil {
			return nil, nil, err
		}
		fingerprints[sess.ID] = payload.fingerprint
		messageRows[sess.ID] = len(payload.messages)
		if onProgress != nil && (i+1)%fingerprintProgressStride == 0 {
			onProgress(storage.PushProgress{Phase: "preparing", SessionsDone: i + 1, SessionsTotal: len(sessions)})
		}
	}
	return fingerprints, messageRows, nil
}

// SessionContent is what a push writes for a session outside the session
// row: the dependent tables and the rows derived from them. Every value a
// dependent row takes from the local archive must reach this struct, or a
// change to it alone would leave the stored row stale. It is exported
// because encoding/json inlines only an embedded exported struct.
type SessionContent struct {
	Messages       []db.Message
	Dialogue       []*string `json:",omitzero"`
	Usage          string
	ToolCalls      string
	SecretFindings []db.SecretFinding
	Pins           []db.PinnedMessage
	CommitLinks    []db.CommitLinkRow `json:",omitzero"`
	ImageText      []db.ImageTextRow  `json:",omitzero"`
	// Omitted when nil so sessions without plan usage keep the
	// fingerprints they had before the table existed.
	PlanUsage *db.SessionPlanUsage `json:",omitzero"`
	// Omitted when nil for the same reason as PlanUsage.
	SessionTotal *db.SessionUsageTotal `json:",omitzero"`
}

func (s *Sync) snapshotFingerprint(
	snapshot *db.SessionMirrorSnapshot, total *db.SessionUsageTotal,
) (sessionFingerprint, error) {
	sess := snapshot.Session
	content := SessionContent{
		Messages:       snapshot.Messages,
		Dialogue:       db.DialogueFingerprintValues(snapshot.Messages),
		Usage:          snapshot.UsageFingerprint,
		ToolCalls:      snapshot.ToolCallFingerprint,
		SecretFindings: snapshot.Findings,
		Pins:           snapshot.Pins,
		CommitLinks:    snapshot.CommitLinks,
		ImageText:      snapshot.ImageText,
		PlanUsage:      snapshot.PlanUsage,
		SessionTotal:   total,
	}
	// The content fields are inlined after the session fields, so the full
	// fingerprint keeps the encoding it had before the content fingerprint
	// existed and an upgrade rewrites nothing.
	full, err := hashJSON(sess.ID, struct {
		SessionFields  []any
		SessionContent `json:",inline"`
	}{
		SessionFields:  sessionFingerprintFields(sess, db.MirroredSessionMachine(sess, s.machine)),
		SessionContent: content,
	})
	if err != nil {
		return sessionFingerprint{}, err
	}
	contentHash, err := hashJSON(sess.ID, content)
	if err != nil {
		return sessionFingerprint{}, err
	}
	return sessionFingerprint{full: full, content: contentHash}, nil
}

func hashJSON(sessionID string, value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encoding session fingerprint %s: %w", sessionID, err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// sessionFingerprintFields lists every session scalar the session row
// mirrors, in column order. TestSessionFingerprintCoversEveryMirroredColumn
// keeps this list and the sessions table spec in step: a column left out
// here could go stale while the fingerprint still reports the session as
// unchanged.
func sessionFingerprintFields(sess db.Session, machine string) []any {
	return []any{
		sess.ID, sess.Project, sess.ProjectAssigned, machine, sess.Agent,
		sess.AgentLabel, sess.Entrypoint, sess.SessionKind,
		sess.FirstMessage, sess.DisplayName, sess.SessionName,
		sess.StartedAt, sess.EndedAt,
		sess.MessageCount, sess.UserMessageCount,
		sess.FilePath, sess.FileSize, sess.FileMtime, sess.FileInode, sess.FileDevice, sess.FileHash,
		sess.LocalModifiedAt, sess.TranscriptRevision,
		sess.ParentSessionID, sess.RelationshipType,
		sess.TotalOutputTokens, sess.PeakContextTokens,
		sess.HasTotalOutputTokens, sess.HasPeakContextTokens, sess.IsAutomated,
		sess.ToolFailureSignalCount, sess.ToolRetryCount, sess.EditChurnCount,
		sess.ConsecutiveFailureMax, sess.Outcome, sess.OutcomeConfidence,
		sess.EndedWithRole, sess.FinalFailureStreak, sess.SignalsPendingSince,
		sess.CompactionCount, sess.MidTaskCompactionCount,
		sess.ContextPressureMax, sess.HealthScore, sess.HealthGrade,
		sess.HasToolCalls, sess.HasContextData,
		sess.QualitySignalVersion, sess.ShortPromptCount, sess.UnstructuredStart,
		sess.MissingSuccessCriteriaCount, sess.MissingVerificationCount,
		sess.DuplicatePromptCount, sess.NoCodeContextCount, sess.RunawayToolLoopCount,
		sess.DataVersion,
		sess.Cwd, sess.GitBranch, sess.SourceSessionID, sess.SourceVersion, sess.GroupKey, sess.TranscriptFidelity,
		sess.ParserMalformedLines, sess.IsTruncated,
		sess.DeletedAt, sess.DeletionCause, sess.CreatedAt, sess.TerminationStatus,
		sess.SecretLeakCount, sess.SecretsRulesVersion,
		db.EncodePRLinks(sess.PRLinks), db.LabelsArg(sess.Labels),
	}
}

// sessionFingerprintColumns names, in order, the sessions columns that
// sessionFingerprintFields covers. The remaining sessions columns are
// derived at push time (last_message_at from the messages, which the
// fingerprint already covers) or are push bookkeeping
// (agentsview_push_fingerprint, source_archive_id, push_version).
var sessionFingerprintColumns = []string{
	"id", "project", "project_assigned", "machine", "agent",
	"agent_label", "entrypoint", "session_kind",
	"first_message", "display_name", "session_name",
	"started_at", "ended_at",
	"message_count", "user_message_count",
	"file_path", "file_size", "file_mtime", "file_inode", "file_device", "file_hash",
	"local_modified_at", "transcript_revision",
	"parent_session_id", "relationship_type",
	"total_output_tokens", "peak_context_tokens",
	"has_total_output_tokens", "has_peak_context_tokens", "is_automated",
	"tool_failure_signal_count", "tool_retry_count", "edit_churn_count",
	"consecutive_failure_max", "outcome", "outcome_confidence",
	"ended_with_role", "final_failure_streak", "signals_pending_since",
	"compaction_count", "mid_task_compaction_count",
	"context_pressure_max", "health_score", "health_grade",
	"has_tool_calls", "has_context_data",
	"quality_signal_version", "short_prompt_count", "unstructured_start",
	"missing_success_criteria_count", "missing_verification_count",
	"duplicate_prompt_count", "no_code_context_count", "runaway_tool_loop_count",
	"data_version",
	"cwd", "git_branch", "source_session_id", "source_version", "group_key", "transcript_fidelity",
	"parser_malformed_lines", "is_truncated",
	"deleted_at", "deletion_cause", "created_at", "termination_status",
	"secret_leak_count", "secrets_rules_version",
	"pr_links", "labels",
}

// derivedSessionColumns are the sessions columns no fingerprint field
// covers, with the reason each is safe to leave out.
var derivedSessionColumns = map[string]string{
	"last_message_at":             "derived from the messages the fingerprint hashes",
	"agentsview_push_fingerprint": "the fingerprint itself",
	// A push writes both with the content; they describe it.
	"agentsview_content_fingerprint": "the content fingerprint itself",
	"content_version":                "push bookkeeping",
	"source_archive_id":              "push bookkeeping",
	"stored_message_count":           "derived from the messages the fingerprint hashes",
	// A push rewrites the session rows of sessions below the current
	// dashboardFactsVersion; otherwise these change only with the messages.
	"dashboard_facts_version":       "push bookkeeping",
	"fact_user_messages":            "derived from the messages the fingerprint hashes",
	"fact_assistant_messages":       "derived from the messages the fingerprint hashes",
	"fact_thinking_messages":        "derived from the messages the fingerprint hashes",
	"fact_tool_use_messages":        "derived from the messages the fingerprint hashes",
	"fact_tool_calls":               "derived from the messages the fingerprint hashes",
	"fact_frustration_markers":      "derived from the messages the fingerprint hashes",
	"fact_velocity_messages":        "derived from the messages the fingerprint hashes",
	"fact_velocity_turn_cycles":     "derived from the messages the fingerprint hashes",
	"fact_velocity_first_response":  "derived from the messages the fingerprint hashes",
	"fact_velocity_active_seconds":  "derived from the messages the fingerprint hashes",
	"fact_velocity_assistant_chars": "derived from the messages the fingerprint hashes",
}
