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

// sessionFingerprints hashes each candidate's local content: the mirrored
// session scalars, every message with its tool calls and result events,
// the usage-event fingerprint, the tool-call fingerprint (file_path and
// call_index are json:"-" on ToolCall), secret findings, and pins. The
// mirror stores the hash on the session row; a later push skips sessions
// whose hash is unchanged.
func (s *Sync) sessionFingerprints(
	ctx context.Context, sessions []db.Session, onProgress func(storage.PushProgress),
) (map[string]string, error) {
	usage, err := s.local.UsageEventFingerprints(sessionIDs(sessions))
	if err != nil {
		return nil, fmt.Errorf("computing usage fingerprints: %w", err)
	}
	out := make(map[string]string, len(sessions))
	for i, sess := range sessions {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		msgs, err := s.local.GetAllMessages(ctx, sess.ID)
		if err != nil {
			return nil, fmt.Errorf("message fingerprint %s: %w", sess.ID, err)
		}
		findings, err := s.local.SessionSecretFindings(ctx, sess.ID)
		if err != nil {
			return nil, fmt.Errorf("secret finding fingerprint %s: %w", sess.ID, err)
		}
		pins, err := s.local.ListPinnedMessages(ctx, sess.ID, "")
		if err != nil {
			return nil, fmt.Errorf("pin fingerprint %s: %w", sess.ID, err)
		}
		toolCalls, err := s.local.ToolCallFingerprint(ctx, sess.ID)
		if err != nil {
			return nil, fmt.Errorf("tool call fingerprint %s: %w", sess.ID, err)
		}
		payload := struct {
			SessionFields  []any
			Messages       []db.Message
			Usage          string
			ToolCalls      string
			SecretFindings []db.SecretFinding
			Pins           []db.PinnedMessage
		}{
			SessionFields:  sessionFingerprintFields(sess, mirroredSessionMachine(sess, s.machine)),
			Messages:       msgs,
			Usage:          usage[sess.ID],
			ToolCalls:      toolCalls,
			SecretFindings: findings,
			Pins:           pins,
		}
		data, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("encoding session fingerprint %s: %w", sess.ID, err)
		}
		sum := sha256.Sum256(data)
		out[sess.ID] = hex.EncodeToString(sum[:])
		if onProgress != nil && (i+1)%fingerprintProgressStride == 0 {
			onProgress(storage.PushProgress{Phase: "preparing", SessionsDone: i + 1, SessionsTotal: len(sessions)})
		}
	}
	return out, nil
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
		sess.Cwd, sess.GitBranch, sess.SourceSessionID, sess.SourceVersion, sess.TranscriptFidelity,
		sess.ParserMalformedLines, sess.IsTruncated,
		sess.DeletedAt, sess.DeletionCause, sess.CreatedAt, sess.TerminationStatus,
		sess.SecretLeakCount, sess.SecretsRulesVersion,
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
	"cwd", "git_branch", "source_session_id", "source_version", "transcript_fidelity",
	"parser_malformed_lines", "is_truncated",
	"deleted_at", "deletion_cause", "created_at", "termination_status",
	"secret_leak_count", "secrets_rules_version",
}

// derivedSessionColumns are the sessions columns no fingerprint field
// covers, with the reason each is safe to leave out.
var derivedSessionColumns = map[string]string{
	"last_message_at":             "derived from the messages the fingerprint hashes",
	"agentsview_push_fingerprint": "the fingerprint itself",
	"source_archive_id":           "push bookkeeping",
}
