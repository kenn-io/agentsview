package sync

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/secrets"
	"go.kenn.io/agentsview/internal/signals"
	"go.kenn.io/agentsview/internal/timeutil"
)

// codexStagingSink implements parser.CodexSessionSink for the streaming
// full-parse path: messages and tool-call metadata stay in memory (they are
// small relative to tool outputs), while every tool-result event row and
// the per-call agent summary state are written to a scratch SQLite file as
// they arrive. The in-memory model therefore never holds result-event
// content — events carry a unique placeholder — and peak memory is
// O(messages + batch), not O(file size). The scratch database is also the
// publish source: the staged write inserts tool_result_events straight
// from it and resolves result_content summaries per call with transient
// memory bounded by one call's distinct agents.
type codexStagingSink struct {
	*parser.CodexCollectingSink

	scratch *sql.DB
	path    string

	// idPrefix is applied to subagent_session_id at publish time, mirroring
	// applyRemoteRewrites on the collecting path: staged events are inserted
	// directly from scratch and never pass through the in-memory rewrite
	// that prefixes remote (SSH/S3) session ids.
	idPrefix string

	// blocked marks categories whose stored content is blanked. Their raw
	// content never enters scratch storage; only digest, original length,
	// ordering metadata, and summary participation are retained.
	blocked map[string]bool

	// Calls use an occurrence-qualified staging key because provider call IDs
	// can repeat within one transcript. callKeyByPosition is authoritative;
	// currentCallKey is only the compatibility fallback for events that do not
	// carry a parser-resolved occurrence.
	currentCallKey    map[string]string
	callKeyByPosition map[parser.ParsedToolCallPosition]string
	callOccurrences   map[string]int
	categoryByCallKey map[string]string

	// findings collects definite findings from result-event content as
	// it streams by; findingPos records the coordinates to patch after
	// ordinal finalization.
	findings       []db.SecretFinding
	findingPos     []stagedFindingPos
	eventByCallKey map[string]int64
	eventSeq       int64
	// contentFailures records per-call content-failure verdicts captured
	// while the publish transaction resolves summaries, so the engine can
	// fold them into the signal pass after the atomic publish.
	contentFailures map[string]bool
	// stageErr is the sticky first scratch failure. Once set, staging is
	// unrecoverable for this parse: events are no longer accepted and the
	// publish must fail, so a disk-full or I/O error can never commit a
	// "successful" archive missing tool outputs.
	stageErr        error
	validationStats db.ValidationStats
}

// fail records the sticky staging failure. Later events and the publish
// path consult Err and refuse to proceed.
func (s *codexStagingSink) fail(err error) {
	if s.stageErr == nil {
		s.stageErr = fmt.Errorf("codex staging: %w", err)
	}
}

// Err returns the sticky staging failure, or nil when the scratch
// database has been healthy for this parse.
func (s *codexStagingSink) Err() error {
	return s.stageErr
}

func (s *codexStagingSink) addValidationStats(stats db.ValidationStats) {
	s.validationStats.ControlCharsStripped += stats.ControlCharsStripped
	s.validationStats.ModelClamped += stats.ModelClamped
	s.validationStats.TokensClamped += stats.TokensClamped
	s.validationStats.RoleCoerced += stats.RoleCoerced
	s.validationStats.TimestampsBlanked += stats.TimestampsBlanked
}

// ValidationStats returns fixes applied to real staged result content. The
// ordinary message validation pass sees placeholders, so the engine records
// these additional counts after the staged publish resolves its summaries.
func (s *codexStagingSink) ValidationStats() db.ValidationStats {
	return s.validationStats
}

type stagedFindingPos struct {
	stageKey   string
	eventIndex int
}

// codexStagingFilePrefix identifies scratch files owned by AgentsView.
const codexStagingFilePrefix = "agentsview-codex-stage-"

// prepareCodexStagingDir creates the private scratch directory and removes
// abandoned files older than one day. Recent files may belong to another
// running AgentsView process and are left untouched.
func prepareCodexStagingDir(dir string) error {
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating codex staging directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("securing codex staging directory: %w", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("reading codex staging directory: %w", err)
	}
	cutoff := time.Now().Add(-24 * time.Hour)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), codexStagingFilePrefix) {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		_ = os.Remove(filepath.Join(dir, entry.Name()))
	}
	return nil
}

// checkCodexStagingSpace fails a staged parse before it writes a byte when
// the scratch directory cannot hold a conservative estimate of the staged
// database plus SQLite overhead. The minimum protects small test/configured
// sources; large sources scale the requirement with their actual size.
func checkCodexStagingSpace(dir string, sourceBytes int64) error {
	if dir == "" {
		dir = os.TempDir()
	}
	available, ok, err := stagingDirFreeBytes(dir)
	if err != nil || !ok {
		// Filesystems without a capacity query fail open here; CreateTemp and
		// SQLite report concrete write errors without changing the archive.
		return nil
	}
	const (
		stagedScratchMinFree  = int64(256 << 20)
		stagedScratchOverhead = int64(64 << 20)
	)
	required := stagedScratchMinFree
	if sourceBytes > 0 {
		scaled := sourceBytes + sourceBytes/2 + stagedScratchOverhead
		if scaled > required {
			required = scaled
		}
	}
	if int64(available) < required {
		return fmt.Errorf(
			"codex staging: %s has %dMiB free, need at least %dMiB for a %dMiB source",
			dir, available/(1<<20), required/(1<<20), sourceBytes/(1<<20),
		)
	}
	return nil
}

// stagedCodexParseMinBytes is the full-parse size above which a Codex
// source streams through the scratch staging path instead of the
// collecting parser. Engines may override it (see
// EngineConfig.StagedCodexParseMinBytes) for tests.
const stagedCodexParseMinBytes = 128 << 20

// stagedCodexMinBytes resolves a configured override to the default.
func stagedCodexMinBytes(override int64) int64 {
	if override > 0 {
		return override
	}
	return stagedCodexParseMinBytes
}

// stagedColdSyncGCPercent is the GC percent held while a staged Codex cold
// sync is in flight. The streaming path's live set is bounded, so a lower
// target keeps the peak heap (and with it RSS) near the live set instead
// of letting transient per-event garbage double it. Restored on release.
const stagedColdSyncGCPercent = 30

// The GC percent is process-global state, so the refcount lives at package
// scope: two engines interleaving staged syncs must share one lowered
// window rather than racing to restore each other's baseline.
var (
	stagedGCMu   sync.Mutex
	stagedGCRefs int
	stagedGCPrev int
)

// beginStagedColdSync lowers the process GC percent for the duration of one
// staged Codex cold sync and returns the function that restores the prior
// value. Concurrent staged syncs share one lowered window via a refcount.
func beginStagedColdSync() func() {
	stagedGCMu.Lock()
	defer stagedGCMu.Unlock()
	if stagedGCRefs == 0 {
		stagedGCPrev = debug.SetGCPercent(stagedColdSyncGCPercent)
	}
	stagedGCRefs++
	released := false
	return func() {
		stagedGCMu.Lock()
		defer stagedGCMu.Unlock()
		if released {
			return
		}
		released = true
		stagedGCRefs--
		if stagedGCRefs == 0 {
			debug.SetGCPercent(stagedGCPrev)
		}
	}
}

const codexStagingSchema = `
CREATE TABLE stage_events (
    seq INTEGER PRIMARY KEY,
    call_key TEXT NOT NULL,
    tool_use_id TEXT NOT NULL,
    agent_id TEXT NOT NULL DEFAULT '',
    subagent_session_id TEXT NOT NULL DEFAULT '',
    source TEXT NOT NULL,
    status TEXT NOT NULL,
    content TEXT NOT NULL,
    raw_content_digest BLOB NOT NULL,
    content_length INTEGER NOT NULL,
    timestamp TEXT NOT NULL DEFAULT '',
    blanked INTEGER NOT NULL DEFAULT 0,
    summary_participates INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_stage_events_call ON stage_events(call_key, seq);`

// newCodexStagingSink opens a scratch SQLite database for one streaming
// parse. The caller must Close it once the staged write has published.
// dir selects the scratch directory; empty means the system temporary
// directory.
func newCodexStagingSink(
	dir string,
	blocked map[string]bool,
	sourceSize ...int64,
) (*codexStagingSink, error) {
	if err := prepareCodexStagingDir(dir); err != nil {
		return nil, err
	}
	var sourceBytes int64
	if len(sourceSize) > 0 {
		sourceBytes = sourceSize[0]
	}
	if err := checkCodexStagingSpace(dir, sourceBytes); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(dir, codexStagingFilePrefix+"*.sqlite")
	if err != nil {
		return nil, fmt.Errorf("creating codex staging file: %w", err)
	}
	path := f.Name()
	if err := f.Close(); err != nil {
		os.Remove(path)
		return nil, err
	}
	scratch, err := sql.Open("sqlite3", path)
	if err != nil {
		os.Remove(path)
		return nil, fmt.Errorf("opening codex staging db: %w", err)
	}
	for _, pragma := range []string{
		"PRAGMA journal_mode=OFF",
		"PRAGMA synchronous=OFF",
		"PRAGMA temp_store=FILE",
	} {
		if _, err := scratch.Exec(pragma); err != nil {
			scratch.Close()
			os.Remove(path)
			return nil, fmt.Errorf("configuring codex staging db: %w", err)
		}
	}
	if _, err := scratch.Exec(codexStagingSchema); err != nil {
		scratch.Close()
		os.Remove(path)
		return nil, fmt.Errorf("creating codex staging schema: %w", err)
	}
	return &codexStagingSink{
		CodexCollectingSink: parser.NewCodexCollectingSink(0),
		scratch:             scratch,
		path:                path,
		blocked:             blocked,
		currentCallKey:      make(map[string]string),
		callKeyByPosition:   make(map[parser.ParsedToolCallPosition]string),
		callOccurrences:     make(map[string]int),
		categoryByCallKey:   make(map[string]string),
		eventByCallKey:      make(map[string]int64),
	}, nil
}

// Close releases the scratch database and removes its file.
func (s *codexStagingSink) Close() error {
	err := s.scratch.Close()
	if err == nil {
		err = os.Remove(s.path)
	}
	return err
}

// Path returns the staging file path for ATTACH-based publishing.
func (s *codexStagingSink) Path() string {
	return s.path
}

// stagedCodexParseOutcome runs the streaming Codex parse through sink and
// folds the result into the same ParseOutcome shape the provider's
// collecting parse returns, so the engine's downstream outcome pipeline is
// shared between the two paths.
func stagedCodexParseOutcome(
	cfg parser.ProviderConfig,
	source parser.SourceRef,
	fingerprint parser.SourceFingerprint,
	sink *codexStagingSink,
) (parser.ParseOutcome, error) {
	sess, msgs, cursor, hashState, anchorDigest, retryReason, err :=
		parser.ParseCodexSessionStreaming(cfg, source, sink)
	if err != nil {
		return parser.ParseOutcome{}, err
	}
	if stageErr := sink.Err(); stageErr != nil {
		return parser.ParseOutcome{}, stageErr
	}
	// The collecting provider copies the fingerprint hash onto the parsed
	// session; the streaming entry point has no fingerprint parameter, so
	// mirror that here. A staged full parse with a precomputed fingerprint
	// must persist the same file_hash the collecting path would, or the
	// checkpoint's hash and the stored file_hash disagree and every later
	// validation forces another full parse.
	if fingerprint.Hash != "" {
		sess.File.Hash = fingerprint.Hash
	}
	// Return the parse phase's transient arenas before the publish builds
	// its own transient working set, so the process RSS high-water mark
	// reflects the publish rather than the sum of both phases' slack.
	debug.FreeOSMemory()
	result := parser.ParseResultOutcome{
		Result: parser.ParseResult{
			Session:                *sess,
			Messages:               msgs,
			Checkpoint:             cursor,
			CheckpointHashState:    hashState,
			CheckpointAnchorDigest: anchorDigest,
		},
		DataVersion: parser.DataVersionCurrent,
	}
	if retryReason != "" {
		// An explicit fork parent could not be resolved: keep the child
		// visible but mark its stored data version for retry so a later
		// unchanged-object sync can replace the temporary overcount.
		result.DataVersion = parser.DataVersionNeedsRetry
		result.RetryReason = retryReason
	}
	return parser.ParseOutcome{
		Results:           []parser.ParseResultOutcome{result},
		ResultSetComplete: true,
		ForceReplace:      true,
	}, nil
}

// definiteFindingCount counts the definite-confidence findings in a
// merged findings slice, stamping the session's secret-leak signal.
func definiteFindingCount(findings []db.SecretFinding) int {
	n := 0
	for _, f := range findings {
		if f.Confidence == "definite" {
			n++
		}
	}
	return n
}

// closeCodexStagingSinks releases a batch of staging sinks, removing their
// scratch files. It is the staging analog of releaseParseRetentionLeases.
func closeCodexStagingSinks(sinks []*codexStagingSink) {
	for _, s := range sinks {
		if s == nil {
			continue
		}
		if err := s.Close(); err != nil {
			log.Printf("closing codex staging sink: %v", err)
		}
	}
}

// releaseStagedGCGuards restores the process GC percent after a batch of
// staged cold syncs finished, in reverse open order.
func releaseStagedGCGuards(guards []func()) {
	for i := len(guards) - 1; i >= 0; i-- {
		if guards[i] != nil {
			guards[i]()
		}
	}
}

func (s *codexStagingSink) AppendMessage(m parser.ParsedMessage) int {
	ordinal := s.CodexCollectingSink.AppendMessage(m)
	for callIndex, tc := range m.ToolCalls {
		if tc.ToolUseID == "" {
			continue
		}
		occurrence := s.callOccurrences[tc.ToolUseID]
		s.callOccurrences[tc.ToolUseID] = occurrence + 1
		stageKey := db.StagedToolCallKey(tc.ToolUseID, occurrence)
		s.currentCallKey[tc.ToolUseID] = stageKey
		s.callKeyByPosition[parser.ParsedToolCallPosition{
			MessageOrdinal: ordinal,
			CallIndex:      callIndex,
		}] = stageKey
		s.categoryByCallKey[stageKey] = tc.Category
	}
	return ordinal
}

// AppendToolResultEvent stages the full event row and the per-call summary
// state, then records a contentless placeholder in the in-memory model so
// downstream conversions stay shape-compatible without retaining content.
func (s *codexStagingSink) AppendToolResultEvent(
	callID string, target *parser.ParsedToolCallPosition,
	ev parser.ParsedToolResultEvent,
) {
	if callID == "" || s.stageErr != nil {
		return
	}
	// The parser extracts event fields as gjson substrings of the source
	// line. Storing those small strings in the in-memory model (event
	// identity fields, map keys below) would pin the entire line's backing
	// buffer — for large tool outputs that keeps the whole transcript's
	// line bytes reachable across the parse. Clone the fields the model
	// keeps; content is replaced by a placeholder after staging.
	callID = strings.Clone(callID)
	ev.ToolUseID = strings.Clone(ev.ToolUseID)
	ev.AgentID = strings.Clone(ev.AgentID)
	ev.SubagentSessionID = strings.Clone(ev.SubagentSessionID)
	ev.Status = strings.Clone(ev.Status)
	ev.Source = strings.Clone(ev.Source)
	// The legacy write path normalizes event timestamps through
	// timeutil.Format before storing them; the staged rows must store the
	// same normalized form so stored projections match byte for byte.
	tsStr := timeutil.Format(ev.Timestamp)
	// Events for calls that never registered in the message model are
	// unreachable regardless of how they are held: parser.ParseResult
	// carries no ToolCallUpdates field, so every full-parse consumer
	// (collecting and staged alike) discards them. Drop the event outright
	// instead of forwarding it to the embedded collecting sink's orphan
	// path, which would retain ev.Content -- an uncloned reference into
	// the source line's backing buffer -- purely to be thrown away,
	// defeating the staged sink's bounded-memory guarantee on large
	// orphan outputs. Late outputs still merge through the incremental
	// append path on later syncs, unchanged.
	stageKey, ok := "", false
	if target != nil {
		stageKey, ok = s.callKeyByPosition[*target]
	} else {
		stageKey, ok = s.currentCallKey[callID]
	}
	if !ok {
		return
	}
	// The legacy deduplication compares raw parser content before the central
	// sanitizer runs. Keep that identity as a digest so the staged row can
	// store sanitized content without collapsing events that differed only by
	// stripped controls. The digest also avoids retaining a second copy of a
	// potentially very large raw result in scratch.
	rawContentDigest := sha256.Sum256([]byte(ev.Content))
	var exists int
	err := s.scratch.QueryRow(
		`SELECT 1 FROM stage_events
		 WHERE call_key = ? AND agent_id = ? AND status = ?
		   AND raw_content_digest = ? LIMIT 1`,
		stageKey, ev.AgentID, ev.Status, rawContentDigest[:],
	).Scan(&exists)
	if err == nil {
		return // equivalent event already staged
	}
	if err != sql.ErrNoRows {
		s.fail(err)
		return
	}

	s.eventSeq++
	seq := s.eventSeq
	subagent := ev.SubagentSessionID
	if subagent == "" && strings.TrimSpace(ev.AgentID) != "" {
		agentID := strings.TrimSpace(ev.AgentID)
		if strings.HasPrefix(agentID, "codex:") {
			subagent = agentID
		} else {
			subagent = "codex:" + agentID
		}
	}
	blanked := 0
	if s.blocked[s.categoryByCallKey[stageKey]] {
		blanked = 1
	}
	contentLength := len(ev.Content)
	summaryParticipates := strings.TrimSpace(ev.Content) != ""
	if blanked == 0 {
		// The collecting path sanitizes result-event content in the central
		// db validation pass. Staged events bypass that pass because the
		// in-memory message carries only a placeholder, so apply the same
		// contract before the real content enters the scratch publish source.
		// Keep dedup above this point raw: two provider events that differ
		// only by stripped controls remain two events on the collecting path.
		toolCall := db.ToolCall{ResultEvents: []db.ToolResultEvent{{
			Content:       ev.Content,
			ContentLength: contentLength,
		}}}
		s.addValidationStats(db.SanitizeToolCall(&toolCall))
		ev.Content = toolCall.ResultEvents[0].Content
		contentLength = toolCall.ResultEvents[0].ContentLength
	} else {
		// Blocked content must not be recoverable from an abandoned scratch
		// database after a crash. The digest and original length preserve
		// deduplication and result_content_length parity without storing bytes.
		ev.Content = ""
	}
	if _, err := s.scratch.Exec(
		`INSERT INTO stage_events (
		     seq, call_key, tool_use_id, agent_id, subagent_session_id,
		     source, status, content, raw_content_digest, content_length,
		     timestamp, blanked, summary_participates
		 ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		seq, stageKey, callID, ev.AgentID, subagent, ev.Source, ev.Status,
		ev.Content, rawContentDigest[:], contentLength, tsStr, blanked,
		summaryParticipates,
	); err != nil {
		s.fail(err)
		return
	}

	// Definite findings from the stored (blanked) content — the same
	// text the legacy scan reads back from the database.
	storedContent := ev.Content
	if blanked != 0 {
		storedContent = ""
	}
	eventIndex := int(s.eventByCallKey[stageKey])
	s.eventByCallKey[stageKey]++
	s.addEventFindings(stageKey, eventIndex, storedContent)

	// The in-memory model keeps a unique placeholder instead of the
	// content: downstream conversions stay shape-compatible and the
	// collecting dedup treats every staged event as distinct.
	ev.Content = fmt.Sprintf("staged:%d", seq)
	s.CodexCollectingSink.AppendToolResultEvent(callID, target, ev)
}

func (s *codexStagingSink) addEventFindings(
	stageKey string, eventIndex int, content string,
) {
	if content == "" {
		return
	}
	matches := secrets.ScanDefinite(content)
	for _, match := range matches {
		s.findings = append(s.findings, db.SecretFinding{
			RuleName:      match.Rule,
			Confidence:    match.Confidence,
			LocationKind:  "tool_result_event",
			MatchStart:    match.Start,
			MatchEnd:      match.End,
			MatchIndex:    match.Index,
			RedactedMatch: match.Redacted,
			RulesVersion:  secrets.DefiniteRulesVersion(),
		})
		s.findingPos = append(s.findingPos, stagedFindingPos{
			stageKey:   stageKey,
			eventIndex: eventIndex,
		})
	}
}

// Findings returns the staged event findings with session, ordinal, and
// call coordinates stamped from the final message model.
func (s *codexStagingSink) Findings(
	sessionID string,
	positions map[string]db.StagedToolCallPosition,
) []db.SecretFinding {
	out := make([]db.SecretFinding, len(s.findings))
	for i, f := range s.findings {
		f.SessionID = sessionID
		pos, ok := positions[s.findingPos[i].stageKey]
		if ok {
			f.MessageOrdinal = pos.Ordinal
			callIdx := pos.CallIndex
			evIdx := s.findingPos[i].eventIndex
			f.CallIndex = &callIdx
			f.EventIndex = &evIdx
		}
		out[i] = f
	}
	return out
}

// InsertEventsTx inserts the staged result events into tool_result_events
// within the caller's publish transaction, ordered by emission so
// event_index matches the legacy slice order. The caller attached the
// scratch database as codex_staging on the transaction's connection and
// detaches it after the transaction settles; the transaction itself only
// ever modifies main, so the cross-database crash-atomicity limit for
// WAL-mode attached databases is respected.
func (s *codexStagingSink) InsertEventsTx(
	ctx context.Context, tx *sql.Tx, sessionID string,
	messageOrdinals map[string]db.StagedToolCallPosition,
) error {
	if s.stageErr != nil {
		return s.stageErr
	}
	for stageKey, pos := range messageOrdinals {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO tool_result_events (
				session_id, tool_call_message_ordinal, call_index,
				tool_use_id, agent_id, subagent_session_id,
				source, status, content, content_length,
				timestamp, event_index
			)
			SELECT ?, ?, ?, tool_use_id,
			       CASE WHEN agent_id = '' THEN NULL ELSE agent_id END,
			       CASE
			           WHEN subagent_session_id = '' THEN NULL
			           WHEN ? = '' OR instr(subagent_session_id, ?) = 1
			               THEN subagent_session_id
			           ELSE ? || subagent_session_id
			       END,
			       source, status,
			       CASE WHEN blanked = 1 THEN '' ELSE content END,
			       content_length,
			       CASE WHEN timestamp = '' THEN NULL ELSE timestamp END,
			       row_number() OVER (ORDER BY seq) - 1
			FROM codex_staging.stage_events
			WHERE call_key = ?
			ORDER BY seq`,
			sessionID, pos.Ordinal, pos.CallIndex,
			s.idPrefix, s.idPrefix, s.idPrefix, stageKey,
		); err != nil {
			return fmt.Errorf(
				"publishing staged events for %s/%s: %w",
				sessionID, pos.ToolUseID, err,
			)
		}
	}
	return nil
}

// ResolveSummary computes the stored result summary for one call by
// walking its staged event rows in emission order, mirroring
// db.SummarizeToolResultEvents: the latest raw content per agent in
// first-write order, followed by the trailing anonymous content. Memory is
// transient and bounded by the call's distinct agents: the strict bound
// is one call's aggregate output (the summary string itself), not the
// whole transcript. While the summary is in hand it also records the
// call's content-failure verdict (see ContentFailures), so the engine's
// post-publish signal fold never resolves summaries a second time.
func (s *codexStagingSink) ResolveSummary(
	ctx context.Context, stageKey string,
) (summary string, contentLength int, err error) {
	if s.stageErr != nil {
		return "", 0, s.stageErr
	}
	blocked := s.blocked[s.categoryByCallKey[stageKey]]
	if blocked {
		contentLength, err = s.resolveBlockedSummaryLength(ctx, stageKey)
		if err != nil {
			return "", 0, err
		}
		if s.contentFailures == nil {
			s.contentFailures = make(map[string]bool)
		}
		s.contentFailures[stageKey] = signals.IsFailure(signals.ToolCallRow{
			Category: s.categoryByCallKey[stageKey],
		})
		return "", contentLength, nil
	}
	rows, err := s.scratch.QueryContext(ctx, `
		SELECT agent_id, content, summary_participates
		FROM stage_events
		WHERE call_key = ?
		ORDER BY seq`,
		stageKey,
	)
	if err != nil {
		return "", 0, err
	}
	defer rows.Close()
	// Emission order makes the first seen row per agent both the
	// first-write anchor and the earliest summary part; later rows simply
	// overwrite the content.
	var order []string
	latest := make(map[string]string)
	var lastAnon string
	hasAnon := false
	for rows.Next() {
		var agentID, content string
		var participates bool
		if err := rows.Scan(&agentID, &content, &participates); err != nil {
			return "", 0, err
		}
		if !participates {
			continue
		}
		agent := strings.TrimSpace(agentID)
		if agent == "" {
			hasAnon = true
			lastAnon = content
			continue
		}
		if _, ok := latest[agent]; !ok {
			order = append(order, agent)
		}
		latest[agent] = content
	}
	if err := rows.Err(); err != nil {
		return "", 0, err
	}
	var parts []string
	for _, agent := range order {
		parts = append(parts, agent+":\n"+latest[agent])
	}
	switch {
	case len(parts) == 0:
		summary = lastAnon
	case len(parts) == 1:
		summary = parts[0][strings.IndexByte(parts[0], '\n')+1:]
		if hasAnon {
			summary += "\n\n" + lastAnon
		}
	default:
		summary = strings.Join(parts, "\n\n")
		if hasAnon {
			summary += "\n\n" + lastAnon
		}
	}
	contentLength = len(summary)
	// Agent labels become part of result_content but are not themselves
	// result-event content. Sanitize the assembled summary as the normal
	// message validation pass does after SummarizeToolResultEvents.
	toolCall := db.ToolCall{
		ResultContent:       summary,
		ResultContentLength: contentLength,
	}
	s.addValidationStats(db.SanitizeToolCall(&toolCall))
	summary = toolCall.ResultContent
	contentLength = toolCall.ResultContentLength
	verdict := signals.IsFailure(signals.ToolCallRow{
		Category:      s.categoryByCallKey[stageKey],
		ResultContent: summary,
	})
	if s.contentFailures == nil {
		s.contentFailures = make(map[string]bool)
	}
	s.contentFailures[stageKey] = verdict
	return summary, contentLength, nil
}

func (s *codexStagingSink) resolveBlockedSummaryLength(
	ctx context.Context, stageKey string,
) (int, error) {
	rows, err := s.scratch.QueryContext(ctx, `
		SELECT agent_id, content_length, summary_participates
		FROM stage_events
		WHERE call_key = ?
		ORDER BY seq`,
		stageKey,
	)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	order := make([]string, 0)
	latestByAgent := make(map[string]int)
	lastAnonLength := 0
	allHaveAgentID := true
	for rows.Next() {
		var agentID string
		var length int
		var participates bool
		if err := rows.Scan(&agentID, &length, &participates); err != nil {
			return 0, err
		}
		if !participates {
			continue
		}
		agentID = strings.TrimSpace(agentID)
		if agentID == "" {
			allHaveAgentID = false
			lastAnonLength = length
			continue
		}
		if _, ok := latestByAgent[agentID]; !ok {
			order = append(order, agentID)
		}
		latestByAgent[agentID] = length
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	if len(latestByAgent) <= 1 {
		if len(latestByAgent) == 0 {
			return lastAnonLength, nil
		}
		length := latestByAgent[order[0]]
		if lastAnonLength > 0 {
			length += 2 + lastAnonLength
		}
		return length, nil
	}
	parts := make([]int, 0, len(order)+1)
	for _, agentID := range order {
		parts = append(parts, len(agentID)+2+latestByAgent[agentID])
	}
	if !allHaveAgentID && lastAnonLength > 0 {
		parts = append(parts, lastAnonLength)
	}
	total := 0
	for i, length := range parts {
		if i > 0 {
			total += 2
		}
		total += length
	}
	return total, nil
}

// ContentFailures returns the per-call content-failure verdicts captured
// during summary resolution in the publish transaction. Calls the
// transaction never resolved (no registered tool call) are absent.
func (s *codexStagingSink) ContentFailures() map[string]bool {
	return s.contentFailures
}
