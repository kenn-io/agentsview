package sync

// Parse-diff report model. Renderer-agnostic: the text renderer in
// cmd/agentsview and --json output share these structs, so field names
// are part of the CLI's machine-readable surface and must stay stable.

// DiffClass buckets one session's parse-diff outcome.
type DiffClass string

const (
	// DiffIdentical: the freshly parsed, normalized session matches the
	// stored rows on every compared field. Identical sessions are counted
	// but not listed, unless they carry informational-only field diffs.
	DiffIdentical DiffClass = "identical"
	// DiffChanged: at least one non-informational field differs.
	DiffChanged DiffClass = "changed"
	// DiffPendingResync: stored data_version < db.CurrentDataVersion().
	// Field diffs are still computed and attached for drill-down, but the
	// next resync rewrites these rows by definition, so they are never
	// counted as parser drift.
	DiffPendingResync DiffClass = "pending_resync"
	// DiffNewOnDisk: a parsed session with no stored row (the archive is
	// behind the disk; running sync would add it).
	DiffNewOnDisk DiffClass = "new_on_disk"
	// DiffParseError: the current binary failed to parse the source file.
	// The error attributes to every stored session of that file; a failing
	// file with no stored sessions yields one entry with an empty
	// SessionID.
	DiffParseError DiffClass = "parse_error"
	// DiffExcluded: the parser intentionally no longer emits this stored
	// session (e.g. Claude usage-probe exclusions); sync would delete it.
	DiffExcluded DiffClass = "excluded_by_parser"
	// DiffNeedsRetry: the parse succeeded but was marked transient
	// low-fidelity output (antigravity-cli with a lagging agy-reader
	// sidecar); differences are expected and not parser drift.
	DiffNeedsRetry DiffClass = "transient_needs_retry"
	// DiffSkipped: a stored session whose source was never re-parsed.
	// Reason says why: source missing (archive-only), remote session,
	// trashed, import-only agent, database-backed agent, not sampled
	// (--limit), or not discovered.
	DiffSkipped DiffClass = "skipped"
	// DiffRaced: the comparison detected a non-informational change, but
	// the on-disk source file's mtime advanced past the snapshot's stored
	// file_mtime, so the file was written after the last sync recorded it.
	// The "change" is therefore a torn comparison against live content (a
	// concurrent daemon or active session), not a parser regression, and
	// is inconclusive. Raced sessions are reported but never counted as a
	// failure: --fail-on-change must not trip on them. Classification is
	// deliberately conservative -- when the mtime relationship is
	// ambiguous (missing stored mtime, unreadable source) a would-be
	// change is treated as raced rather than risk a false regression --
	// but a change is only ever reclassified when the file was not
	// demonstrably untouched. Scope: the guard reclassifies the per-file
	// compare path only; the presence sweep (a stored ID a clean parse no
	// longer emits) is a separate signal and is not yet skew-guarded.
	DiffRaced DiffClass = "raced"
	// DiffIncrementalSkew: the comparison detected a non-informational
	// change, the stored row was last written through the
	// incremental-append path (sessions.last_write_incremental) rather
	// than a full re-normalization, AND every non-informational diff is
	// confined to the incremental-artifact allow-list
	// (diffsConfinedToIncrementalArtifacts). Claude and Codex append new
	// JSONL tails without rewriting the whole session, so a fresh full
	// re-parse can legitimately differ on the ordinal-shape / per-message
	// metadata surface (message_metadata) even at the current data_version
	// and without a pending resync. That change is comparison-basis skew,
	// not parser drift, and is inconclusive: like DiffRaced it is reported
	// for drill-down but never counted as a failure.
	//
	// The confinement is what keeps the marker from masking real
	// regressions: the marker is session-level, but a regression is
	// field-level, so a non-informational diff on any other field
	// (first_message, message_content, usage totals, tool_calls, the
	// recomputed aggregates, ...) keeps the session DiffChanged and trips
	// --fail-on-change even on an incrementally-written row. The frozen
	// session fields the incremental path owns (termination_status,
	// session_name, cwd, ...) are already marked informational and never
	// reach this test. A full resync still gives the cleanest baseline; see
	// the resync recommendation in the CLI report. Precedence is below
	// DiffRaced: when a source both advanced past its snapshot mtime and
	// was last written incrementally, raced wins because the advanced
	// mtime is the stronger, directly-provable diagnosis.
	DiffIncrementalSkew DiffClass = "incremental_skew"
)

// Compared-field names. Used as FieldDiff.Field values and
// ParseDiffReport.FieldCounts keys.
const (
	FieldMessageCount      = "message_count"
	FieldUserMessageCount  = "user_message_count"
	FieldFirstMessage      = "first_message"
	FieldSessionName       = "session_name"
	FieldStartedAt         = "started_at"
	FieldEndedAt           = "ended_at"
	FieldModels            = "models"
	FieldTotalOutputTokens = "total_output_tokens"
	FieldPeakContextTokens = "peak_context_tokens"
	FieldMessageTokens     = "message_tokens"
	// FieldMessageContent covers per-message body drift: the
	// content_length column and a hash of the body itself, catching
	// equal-length rewrites the token fingerprint does not cover.
	// Bodies are never included in the diff; only sizes are reported.
	FieldMessageContent = "message_content"
	// FieldMessageMetadata catches per-message drift in fields that are
	// in the fingerprint but not separately surfaced: role, timestamp,
	// source ids, sidechain/compact flags, and the thinking/system/
	// tool-use flags (is_system, has_thinking, has_tool_use, thinking_text).
	FieldMessageMetadata = "message_metadata"
	// FieldToolCalls covers per-message tool_call drift in the
	// parser-owned columns: tool_name, category, tool_use_id, input_json,
	// skill_name, subagent_session_id, and result_content_length. The
	// database-assigned ids and the (possibly blocked) result body are
	// never compared; result content is represented only by its length.
	// The sibling tool_result_events table is also not compared per-event:
	// the blocked-category config clears those rows wholesale, so a strict
	// comparison would be config-sensitive; their dominant signal, the
	// summarized content length, is captured by result_content_length.
	FieldToolCalls         = "tool_calls"
	FieldUsageEventCount   = "usage_event_count"
	FieldUsageEventTotals  = "usage_event_totals"
	FieldTerminationStatus = "termination_status"
	// Session metadata fields written only on the full-replace path.
	// UpdateSessionIncremental leaves them frozen, so for the
	// incremental-append agents (Claude, Codex) a difference is benign
	// pipeline history rather than parser drift and is marked
	// informational, mirroring termination_status. The session "project"
	// column is deliberately not compared: it is rewritten from the
	// mutable worktree_project_mappings table, so its parser-owned input
	// (cwd) is compared instead.
	FieldAgentLabel           = "agent_label"
	FieldEntrypoint           = "entrypoint"
	FieldCwd                  = "cwd"
	FieldGitBranch            = "git_branch"
	FieldParentSessionID      = "parent_session_id"
	FieldRelationshipType     = "relationship_type"
	FieldSourceSessionID      = "source_session_id"
	FieldSourceVersion        = "source_version"
	FieldTranscriptFidelity   = "transcript_fidelity"
	FieldParserMalformedLines = "parser_malformed_lines"
	FieldIsTruncated          = "is_truncated"
	// FieldPresence is the synthetic diff attached when a stored,
	// non-excluded session disappears from its file's parse output.
	FieldPresence = "presence"
)

// FieldDiff is one changed field with pre-rendered old/new values.
// Values are rendered at build time (NULL -> "(null)", long strings
// truncated with the full length noted in Detail) so both renderers
// share one representation.
type FieldDiff struct {
	Field  string `json:"field"`
	Stored string `json:"stored"`
	Parsed string `json:"parsed"`
	// Detail carries comparison context, e.g. "2/142 messages differ;
	// first at ordinal 17".
	Detail string `json:"detail,omitempty"`
	// Informational marks differences explained by pipeline history
	// rather than parser drift (e.g. termination_status cleared to NULL
	// by an incremental append). Informational diffs never make a
	// session DiffChanged and never trip --fail-on-change.
	Informational bool `json:"informational,omitempty"`
}

// SessionDiff is one listed session. Identical sessions appear only
// when they carry informational-only diffs.
type SessionDiff struct {
	SessionID         string      `json:"session_id,omitempty"`
	Agent             string      `json:"agent"`
	FilePath          string      `json:"file_path,omitempty"`
	Class             DiffClass   `json:"class"`
	Reason            string      `json:"reason,omitempty"`
	StoredDataVersion int         `json:"stored_data_version,omitempty"`
	Fields            []FieldDiff `json:"fields,omitempty"`
}

// ParseDiffTotals aggregates per-class session counts.
type ParseDiffTotals struct {
	// Examined counts stored sessions compared against a fresh parse
	// (identical + changed + pending_resync).
	Examined         int `json:"examined"`
	Identical        int `json:"identical"`
	Changed          int `json:"changed"`
	PendingResync    int `json:"pending_resync"`
	NewOnDisk        int `json:"new_on_disk"`
	ParseErrors      int `json:"parse_errors"`
	ExcludedByParser int `json:"excluded_by_parser"`
	NeedsRetry       int `json:"transient_needs_retry"`
	Skipped          int `json:"skipped"`
	// Raced counts sessions whose would-be change was reclassified
	// because the on-disk source advanced past the snapshot mtime. They
	// are inconclusive (live-write skew), so HasFailures excludes them.
	Raced int `json:"raced"`
	// IncrementalSkew counts sessions whose would-be change was
	// reclassified because the stored row was last written through the
	// incremental-append path (last_write_incremental), so a full re-parse
	// legitimately differs. They are inconclusive comparison-basis skew,
	// not parser drift, so HasFailures excludes them; they are still part
	// of Examined because they were compared, exactly like Raced.
	IncrementalSkew int `json:"incremental_skew"`
	// InformationalOnly counts sessions classified identical whose only
	// diffs are informational. Included in Identical.
	InformationalOnly int `json:"informational_only"`
}

// ParseDiffReport is the full result of one report-only re-parse
// comparison. Sessions is sorted by (class, agent, file path, session
// id) for deterministic output.
type ParseDiffReport struct {
	GeneratedAt string `json:"generated_at"`
	// DataVersion is db.CurrentDataVersion() of the running binary.
	DataVersion int `json:"data_version"`
	// DBPath identifies the archive that was vetted, so an attached
	// report is self-describing. Set by the CLI after the run.
	DBPath string   `json:"db_path,omitempty"`
	Agents []string `json:"agents"`
	// FilesExamined counts source files re-parsed; FilesLimited reports
	// whether --limit truncated discovery.
	FilesExamined int  `json:"files_examined"`
	FilesLimited  bool `json:"files_limited"`

	Totals ParseDiffTotals `json:"totals"`
	// FieldCounts maps a compared-field name to the number of
	// DiffChanged sessions with a non-informational diff on that field.
	FieldCounts map[string]int `json:"field_counts"`
	Sessions    []SessionDiff  `json:"sessions"`
}

// HasFailures reports whether --fail-on-change should exit non-zero:
// real per-session changes or files the current binary cannot parse.
// Raced sessions are deliberately excluded -- a change masked by a
// live-write skew is inconclusive, not a parser regression, so it must
// not trip the gate.
func (r *ParseDiffReport) HasFailures() bool {
	return r.Totals.Changed > 0 || r.Totals.ParseErrors > 0
}

// VacuousResync reports that every examined session is pending_resync,
// i.e. the running binary's data version is ahead of the whole
// archive. In that state the comparison detects no drift by
// construction (resync will rewrite every row), so a clean result is
// not evidence the parser is unchanged. Parser PRs that bump
// dataVersion in the same commit hit this; the caller should warn and
// not treat the run as a passing vet.
func (r *ParseDiffReport) VacuousResync() bool {
	return r.Totals.Examined > 0 &&
		r.Totals.Examined == r.Totals.PendingResync
}
