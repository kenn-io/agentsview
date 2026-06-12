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
)

// Compared-field names. Used as FieldDiff.Field values and
// ParseDiffReport.FieldCounts keys.
const (
	FieldMessageCount      = "message_count"
	FieldUserMessageCount  = "user_message_count"
	FieldFirstMessage      = "first_message"
	FieldSessionName       = "session_name"
	FieldModels            = "models"
	FieldTotalOutputTokens = "total_output_tokens"
	FieldPeakContextTokens = "peak_context_tokens"
	FieldMessageTokens     = "message_tokens"
	FieldUsageEventCount   = "usage_event_count"
	FieldUsageEventTotals  = "usage_event_totals"
	FieldTerminationStatus = "termination_status"
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
	SessionID         string      `json:"session_id"`
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
	DataVersion int      `json:"data_version"`
	Agents      []string `json:"agents"`
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
func (r *ParseDiffReport) HasFailures() bool {
	return r.Totals.Changed > 0 || r.Totals.ParseErrors > 0
}
