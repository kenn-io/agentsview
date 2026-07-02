package sync

import gosync "sync"

// Phase describes the current sync phase.
type Phase string

const (
	PhaseIdle             Phase = "idle"
	PhaseDiscovering      Phase = "discovering"
	PhasePreparingResync  Phase = "preparing_resync"
	PhaseSyncing          Phase = "syncing"
	PhaseCopyingMetadata  Phase = "copying_metadata"
	PhaseCopyingOrphans   Phase = "copying_orphans"
	PhaseReclassifying    Phase = "reclassifying"
	PhaseRebuildingSearch Phase = "rebuilding_search"
	PhaseSwappingDatabase Phase = "swapping_database"
	PhaseDone             Phase = "done"
)

// Progress reports sync progress to listeners.
type Progress struct {
	Phase           Phase  `json:"phase"`
	Detail          string `json:"detail,omitempty"`
	Hint            string `json:"hint,omitempty"`
	Resync          bool   `json:"resync,omitempty"`
	CurrentProject  string `json:"current_project,omitempty"`
	ProjectsTotal   int    `json:"projects_total"`
	ProjectsDone    int    `json:"projects_done"`
	SessionsTotal   int    `json:"sessions_total"`
	SessionsDone    int    `json:"sessions_done"`
	MessagesIndexed int    `json:"messages_indexed"`
	BytesDone       int64  `json:"bytes_done,omitempty"`
	BytesTotal      int64  `json:"bytes_total,omitempty"`
}

// SyncResult describes the outcome of syncing a single session.
type SyncResult struct {
	SessionID string `json:"session_id"`
	Project   string `json:"project"`
	Skipped   bool   `json:"skipped"`
	Messages  int    `json:"messages"`
}

// SyncStats summarizes a full sync run.
//
// TotalSessions counts discovered files plus DB-backed sessions.
// Synced counts sessions (one file can produce multiple via fork
// detection; DB-backed agents add sessions directly). Failed counts
// files with hard parse/stat errors. filesOK counts files that
// produced at least one session — used by ResyncAll to compare
// against Failed on the same unit.
type SyncStats struct {
	TotalSessions  int      `json:"total_sessions"`
	Synced         int      `json:"synced"`
	Skipped        int      `json:"skipped"`
	Failed         int      `json:"failed"`
	OrphanedCopied int      `json:"orphaned_copied,omitempty"`
	Warnings       []string `json:"warnings,omitempty"`
	Aborted        bool     `json:"aborted,omitempty"`

	// Anomalies aggregates per-run parser/sanitizer anomaly signals
	// surfaced in the CLI sync summary. These are live per-run counters
	// (reset each sync), not persisted state. A zero value means a clean
	// run and is omitted from the summary.
	Anomalies AnomalyStats `json:"anomalies,omitzero"`

	filesOK         int // unexported: file-level success counter
	filesDiscovered int // file-based total, excludes DB-backed agents
	// nonContainerDiscovered counts discovered files that are not part of
	// a self-preserving container store (OpenCode-format storage and its
	// SQLite virtual paths). The resync empty-discovery guard uses it so a
	// container store's discovery does not mask the disappearance of plain
	// file-backed sessions whose directories went empty.
	nonContainerDiscovered int
	messagesIndexed        int // unexported: progress message counter
	parserExcludedFiles    int // file-level intentional parser exclusions
	parserExcludedIDs      []string
}

// AnomalyStats aggregates parser-output anomaly signals observed during a
// single sync run. It surfaces numbers that already exist or are already
// computed but were previously discarded: per-agent parser malformed-line
// counts (parser_malformed_lines) and the per-category counts returned by
// the central validateAndSanitize pass. It is a per-run summary only; no
// new persisted columns back it.
type AnomalyStats struct {
	// MalformedLinesByAgent maps an agent type to the total number of
	// parser malformed lines reported by sessions of that agent in this
	// run. Only non-zero agents are present.
	MalformedLinesByAgent map[string]int `json:"malformed_lines_by_agent,omitempty"`
	// MalformedLinesTotal is the grand total across all agents.
	MalformedLinesTotal int `json:"malformed_lines_total,omitempty"`

	// UnknownSchemaSessionsByAgent maps an agent type to the number of
	// sessions written this run that were decoded from an unrecognized
	// (newer) Antigravity schema -- source_version carries an "agy-schema:"
	// marker, so the heuristic decode may be incomplete or wrong. Unlike the
	// malformed-line counter this counts whole SESSIONS, not lines. Only
	// non-zero agents are present.
	UnknownSchemaSessionsByAgent map[string]int `json:"unknown_schema_sessions_by_agent,omitempty"`
	// UnknownSchemaSessionsTotal is the grand total across all agents.
	UnknownSchemaSessionsTotal int `json:"unknown_schema_sessions_total,omitempty"`

	// Sanitize aggregates the central validation/sanitization fix counts
	// across every session, message, and usage event written this run.
	Sanitize SanitizeStats `json:"sanitize,omitzero"`
}

// SanitizeStats mirrors the per-category fix counts produced by the central
// validateAndSanitize pass, accumulated across a full sync run. It is the
// exported, summary-facing form of the internal validationStats so the CLI
// summary can render it.
type SanitizeStats struct {
	ControlCharsStripped int `json:"control_chars_stripped,omitempty"`
	ModelClamped         int `json:"model_clamped,omitempty"`
	TokensClamped        int `json:"tokens_clamped,omitempty"`
	RoleCoerced          int `json:"role_coerced,omitempty"`
	TimestampsBlanked    int `json:"timestamps_blanked,omitempty"`
}

// Total returns the sum of all sanitize fix counts.
func (s SanitizeStats) Total() int {
	return s.ControlCharsStripped + s.ModelClamped + s.TokensClamped +
		s.RoleCoerced + s.TimestampsBlanked
}

// IsZero reports whether no sanitize fixes were recorded.
func (s SanitizeStats) IsZero() bool {
	return s.Total() == 0
}

// IsZero reports whether the run observed no anomalies at all, so the CLI
// summary can omit the anomaly section entirely on clean runs.
func (a AnomalyStats) IsZero() bool {
	return a.MalformedLinesTotal == 0 &&
		a.UnknownSchemaSessionsTotal == 0 &&
		a.Sanitize.IsZero()
}

// RecordMalformedLines attributes n parser malformed lines to the given
// agent and updates the grand total. A zero count is ignored so clean
// agents do not appear in the per-agent breakdown.
func (a *AnomalyStats) RecordMalformedLines(agent string, n int) {
	if n <= 0 {
		return
	}
	if a.MalformedLinesByAgent == nil {
		a.MalformedLinesByAgent = make(map[string]int)
	}
	a.MalformedLinesByAgent[agent] += n
	a.MalformedLinesTotal += n
}

// RecordUnknownSchemaSessions attributes n unrecognized-schema sessions to the
// given agent and updates the grand total. A non-positive count is ignored so
// clean agents do not appear in the per-agent breakdown.
func (a *AnomalyStats) RecordUnknownSchemaSessions(agent string, n int) {
	if n <= 0 {
		return
	}
	if a.UnknownSchemaSessionsByAgent == nil {
		a.UnknownSchemaSessionsByAgent = make(map[string]int)
	}
	a.UnknownSchemaSessionsByAgent[agent] += n
	a.UnknownSchemaSessionsTotal += n
}

// addSanitize accumulates the per-category counts from one
// validateAndSanitize pass into the aggregate.
func (a *AnomalyStats) addSanitize(v validationStats) {
	a.Sanitize.ControlCharsStripped += v.ControlCharsStripped
	a.Sanitize.ModelClamped += v.ModelClamped
	a.Sanitize.TokensClamped += v.TokensClamped
	a.Sanitize.RoleCoerced += v.RoleCoerced
	a.Sanitize.TimestampsBlanked += v.TimestampsBlanked
}

// merge folds another AnomalyStats into the receiver.
func (a *AnomalyStats) merge(o AnomalyStats) {
	for agent, n := range o.MalformedLinesByAgent {
		a.RecordMalformedLines(agent, n)
	}
	for agent, n := range o.UnknownSchemaSessionsByAgent {
		a.RecordUnknownSchemaSessions(agent, n)
	}
	a.Sanitize.ControlCharsStripped += o.Sanitize.ControlCharsStripped
	a.Sanitize.ModelClamped += o.Sanitize.ModelClamped
	a.Sanitize.TokensClamped += o.Sanitize.TokensClamped
	a.Sanitize.RoleCoerced += o.Sanitize.RoleCoerced
	a.Sanitize.TimestampsBlanked += o.Sanitize.TimestampsBlanked
}

// anomalyAccumulator is the engine's per-run, concurrency-safe sink for
// anomaly signals recorded at the write seam (prepareSessionWrite,
// writeIncremental, and the usage-event conversion path). It mirrors the
// phaseStats accumulator pattern: reset at the start of a sync run and
// folded into the returned SyncStats before the run completes. Although the
// write path is serialized under syncMu today, the mutex keeps recording
// safe if a future caller writes from multiple goroutines.
type anomalyAccumulator struct {
	mu    gosync.Mutex
	stats AnomalyStats
	// malformedFiles tracks source paths whose malformed-line count has
	// already been recorded this run, so a file that forks into several
	// sessions counts its malformed lines once. Reset each run.
	malformedFiles map[string]bool
}

// reset clears the accumulator at the start of a sync run.
func (a *anomalyAccumulator) reset() {
	a.mu.Lock()
	a.stats = AnomalyStats{}
	a.malformedFiles = nil
	a.mu.Unlock()
}

// recordMalformedLines accumulates an agent's parser malformed-line count for
// one source file. A single source file can fork into several sessions (e.g.
// Claude subagents) that each carry the same source-level count, so the count
// is recorded once per non-empty source path to avoid multiplying a single
// malformed line across the fork. Sessions with no source path (DB-backed
// agents) are recorded as-is.
func (a *anomalyAccumulator) recordMalformedLines(
	agent, sourcePath string, n int,
) {
	if n <= 0 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if sourcePath != "" {
		if a.malformedFiles[sourcePath] {
			return
		}
		if a.malformedFiles == nil {
			a.malformedFiles = make(map[string]bool)
		}
		a.malformedFiles[sourcePath] = true
	}
	a.stats.RecordMalformedLines(agent, n)
}

// recordUnknownSchemaSession attributes one unrecognized-schema session to the
// given agent. Unlike malformed lines this counts whole sessions, so it is not
// deduplicated per source path: a source that forks into several sessions
// records each fork.
func (a *anomalyAccumulator) recordUnknownSchemaSession(agent string) {
	a.mu.Lock()
	a.stats.RecordUnknownSchemaSessions(agent, 1)
	a.mu.Unlock()
}

// recordSanitize accumulates one validateAndSanitize pass's fix counts.
func (a *anomalyAccumulator) recordSanitize(v validationStats) {
	if (v == validationStats{}) {
		return
	}
	a.mu.Lock()
	a.stats.addSanitize(v)
	a.mu.Unlock()
}

// applyTo folds the accumulated anomalies into the given SyncStats.
func (a *anomalyAccumulator) applyTo(s *SyncStats) {
	a.mu.Lock()
	s.Anomalies.merge(a.stats)
	a.mu.Unlock()
}

// RecordSkip increments the skipped session counter.
func (s *SyncStats) RecordSkip() {
	s.Skipped++
}

// RecordSynced adds n to the synced session counter.
func (s *SyncStats) RecordSynced(n int) {
	s.Synced += n
}

// RecordFailed increments the hard-failure counter.
func (s *SyncStats) RecordFailed() {
	s.Failed++
}

// Percent returns the sync progress as a percentage (0–100).
func (p Progress) Percent() float64 {
	if p.SessionsTotal == 0 {
		return 0
	}
	return float64(p.SessionsDone) /
		float64(p.SessionsTotal) * 100
}

// ProgressFunc is called with progress updates during sync.
type ProgressFunc func(Progress)
