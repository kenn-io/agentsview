package sync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

// incrementalUpdate holds the delta produced by an
// incremental JSONL parse, used to partially update the
// session row without overwriting unrelated columns.
type incrementalUpdate struct {
	agent               parser.AgentType
	sessionID           string
	project             string
	sourceProject       string
	machine             string
	cwd                 string
	recordedGitRemote   string // appended lines never carry the source's URL
	msgs                []parser.ParsedMessage
	links               []parser.ClaudeSubagentLink
	toolCallUpdates     []parser.ParsedToolCallUpdate
	messageUsageUpdates []parser.ParsedMessageTokenUsageUpdate
	// checkpoint is the machine-local parser checkpoint to persist in the
	// same transaction as this incremental delta. nil keeps the existing
	// checkpoint (or leaves none).
	checkpoint           *db.ParserCheckpoint
	checkpointBlobs      *db.ParserCheckpointBlobs
	endedAt              time.Time
	terminationStatus    *string
	msgCount             int // total (old + new)
	userMsgCount         int // total (old + new)
	fileSize             int64
	fileMtime            int64
	fileHash             string
	nextOrdinal          int
	lastEntryUUID        string
	totalOutputTokens    int // absolute (old + new)
	peakContextTokens    int // absolute max(old, new)
	hasTotalOutputTokens bool
	hasPeakContextTokens bool
	providerStatHash     *pendingProviderStatHash
	// suffixReplace marks a delta that re-parses the open same-message.id
	// assistant run from its first record. msgs replaces every stored row
	// from replaceFromOrdinal onward; rows below it are left untouched and
	// the write adjusts the session aggregates from the replaced range, so
	// the count and token fields above are unused on this path.
	suffixReplace bool
	// replaceFromOrdinal is the ordinal of the run's merged message: the
	// first stored ordinal the re-parsed suffix replaces.
	replaceFromOrdinal int
}

// hasSubstantiveUserMessage reports whether the delta contains a user
// message with non-whitespace content. Such deltas can change prompt-derived
// heuristics, which the incremental signal maintainer does not fold; they
// fall back to the debounced full recompute.
func (inc *incrementalUpdate) hasSubstantiveUserMessage() bool {
	for _, m := range inc.msgs {
		if m.Role == parser.RoleUser &&
			strings.TrimSpace(m.Content) != "" {
			return true
		}
	}
	return false
}

// hasCompactBoundary reports whether the delta contains a compact-boundary
// message. Boundaries change the compaction detectors, which the incremental
// signal maintainer does not fold; they fall back to a full recompute.
func (inc *incrementalUpdate) hasCompactBoundary() bool {
	for _, m := range inc.msgs {
		if m.IsCompactBoundary {
			return true
		}
	}
	return false
}

// hasResultSubagentLink reports whether the delta carries a subagent link
// with result content. Linked results update tool_calls.result_content
// outside ToolCallResultUpdates, so the incremental secret scan never sees
// them; maintenance must decline and let the debounced full recompute
// rescan the affected call instead of stamping it current.
func (inc *incrementalUpdate) hasResultSubagentLink() bool {
	for _, link := range inc.links {
		if link.HasResult && strings.TrimSpace(link.ResultContentRaw) != "" {
			return true
		}
	}
	return false
}

// tryProviderIncrementalAppend reproduces the legacy incremental-append
// sync path for a provider-authoritative agent that supports append-only
// incremental parsing (Claude or Codex). The provider owns the byte-offset parse
// via ParseIncremental, but the engine still owns the DB-aware
// bookkeeping (session lookup, data-version and identity guards, ordinal
// resume, cross-sync split detection, and cumulative counters), so this
// drives the shared tryIncrementalJSONL with an adapter that calls the
// provider. Returns (result, true) when the incremental path produced a
// terminal result, or (result, false) to fall through to the full
// provider parse (carrying any forceReplace signal).
func (e *Engine) tryProviderIncrementalAppend(
	ctx context.Context,
	provider parser.Provider,
	source parser.SourceRef,
	file parser.DiscoveredFile,
	fingerprint parser.SourceFingerprint,
	seed []byte,
	fullHash string,
	hashState []byte,
	checkpoint *db.ParserCheckpoint,
) (processResult, bool) {
	// Match the shared tryIncrementalJSONL gate: parse-diff and an explicit
	// full import both require a complete replacement rather than an append.
	// A per-file ForceParse keeps Claude on its incremental path; Codex is the
	// explicit exception below because a single-session refresh must rebuild
	// head-derived metadata.
	if e.forceParse || e.forceFullParse || file.ForceFullParse {
		return processResult{}, false
	}
	if provider.Capabilities().Source.IncrementalAppend !=
		parser.CapabilitySupported {
		return processResult{}, false
	}
	// Incremental appends can write before the full-parse rank gate.
	if lowerRanked, err := e.providerSourceLowerRanked(
		ctx, provider, file.Agent, source,
	); err != nil {
		return processResult{
			err: fmt.Errorf("rank %s source %s: %w", file.Agent, providerDiscoveredPath(source), err),
		}, true
	} else if lowerRanked {
		return processResult{
			skip: true, suppressPresenceSweep: true,
		}, true
	}
	path := providerDiscoveredPath(source)
	if isCodexFormatAgent(file.Agent) {
		fork, err := e.db.HasCodexForkSource(ctx, string(file.Agent), e.effectiveSourcePath(path))
		if err != nil {
			return processResult{err: err}, true
		}
		if fork {
			// A full fork write updates provenance with content. This declines
			// cursor resumption without defeating later unchanged-source gates.
			return processResult{}, false
		}
	}
	if path == "" {
		return processResult{}, false
	}
	// Codex-format incremental parsing intentionally preserves head-derived
	// metadata. A manual refresh, title change, or stale project needs the
	// authoritative full parse, and forceReplace prevents the later DB skip
	// gates from swallowing that refresh. Only Codex itself has a
	// session_index.jsonl title, so that check stays keyed to it: a fork would
	// pay a DB lookup that can never report a change.
	providerAgent := provider.Definition().Type
	if isCodexFormatAgent(providerAgent) &&
		(file.ForceParse ||
			e.pathNeedsProjectReparse(ctx, providerAgent, path) ||
			(providerAgent == parser.AgentCodex &&
				e.codexIndexSessionNameChanged(ctx, path))) {
		return processResult{forceReplace: true}, false
	}
	info, err := os.Stat(path)
	if err != nil {
		return processResult{}, false
	}

	parseFn := func(
		_ string, inc *db.IncrementalInfo,
	) ([]parser.ParsedMessage, []parser.ClaudeSubagentLink, []parser.ParsedToolCallUpdate, []parser.ParsedMessageTokenUsageUpdate, time.Time, int64, *string, []byte, error) {
		// The Claude parser needs the stored tail's provider message id
		// so its queued-command masking fallback fires only for a real
		// same-message.id continuation; without it, every routine queued
		// command followed by a fresh response would force a full parse.
		var storedLastClaudeMessageID *string
		var storedTailTimestamp time.Time
		var storedSessionName *string
		var storedPRLinks map[string]struct{}
		if provider.Definition().Type == parser.AgentClaude {
			id := e.db.LastClaudeMessageID(ctx, inc.ID)
			storedLastClaudeMessageID = &id
			var tailTimestamp string
			terr := e.db.Reader().QueryRow(ctx, `
				SELECT COALESCE(timestamp, '') FROM messages
				WHERE session_id = ? AND ordinal < ?
				ORDER BY ordinal DESC LIMIT 1`,
				inc.ID, inc.NextOrdinal,
			).Scan(&tailTimestamp)
			if terr != nil && !errors.Is(terr, sql.ErrNoRows) {
				return nil, nil, nil, nil, time.Time{}, 0, nil, nil,
					parser.ErrIncrementalNeedsFullParse
			}
			storedTailTimestamp, _ = db.ParseStoredTimestamp(tailTimestamp)
			links, lerr := e.db.GetSessionPRLinkURLs(ctx, inc.ID)
			if lerr != nil {
				return nil, nil, nil, nil, time.Time{}, 0, nil, nil,
					fmt.Errorf("read stored Claude pr links: %w", lerr)
			}
			storedPRLinks = links
			if !e.db.ArchiveContent().UsageOnly() {
				name, found, nerr := e.db.GetSessionName(ctx, inc.ID)
				if nerr != nil {
					return nil, nil, nil, nil, time.Time{}, 0, nil, nil,
						fmt.Errorf("read stored Claude session name: %w", nerr)
				}
				if found {
					storedSessionName = &name
				}
			}
		}
		outcome, status, perr := provider.ParseIncremental(
			ctx,
			parser.IncrementalRequest{
				Source:                    source,
				Fingerprint:               fingerprint,
				SessionID:                 inc.ID,
				Offset:                    inc.FileSize,
				StartOrdinal:              inc.NextOrdinal,
				Machine:                   inc.Machine,
				Seed:                      seed,
				LastEntryUUID:             inc.LastEntryUUID,
				StoredAgentLabel:          inc.AgentLabel,
				StoredEntrypoint:          inc.Entrypoint,
				StoredSessionKind:         inc.SessionKind,
				StoredUserMessageCount:    inc.UserMsgCount,
				StoredClaudeLinearParse:   inc.ClaudeLinearParse,
				StoredLastClaudeMessageID: storedLastClaudeMessageID,
				StoredTailTimestamp:       storedTailTimestamp,
				StoredSessionName:         storedSessionName,
				StoredPRLinks:             storedPRLinks,
				StoredPendingUsageOrdinal: inc.PendingUsageOrdinal,
			},
		)
		if perr != nil {
			return nil, nil, nil, nil, time.Time{}, 0, nil, nil, perr
		}
		switch status {
		case parser.IncrementalNeedsFullParse:
			if outcome.ForceReplace {
				// Signal the shared helper to fall back to a
				// full parse that replaces stored messages.
				return nil, nil, nil, nil, time.Time{}, 0, nil, nil,
					parser.ErrIncrementalNeedsFullParse
			}
			// A plain full-parse fallback without a replace request.
			// The Claude provider always sets ForceReplace on its
			// fallbacks (a DAG fork can drop or re-branch stored
			// rows), so this branch serves providers that only need
			// an append-preserving full parse.
			return nil, nil, nil, nil, time.Time{}, 0, nil, nil, parser.ErrDAGDetected
		case parser.IncrementalNoNewData:
			return nil, nil, nil, nil, time.Time{}, 0, nil, nil, nil
		default:
			var terminationStatus *string
			if outcome.TerminationStatus != nil {
				status := string(*outcome.TerminationStatus)
				terminationStatus = &status
			}
			return outcome.Messages, outcome.SubagentLinks,
				outcome.ToolCallUpdates,
				outcome.MessageTokenUsageUpdates,
				outcome.EndedAt, outcome.ConsumedBytes, terminationStatus,
				outcome.NextCursor, nil
		}
	}

	return e.tryIncrementalJSONL(
		ctx, file, info, file.Agent, parseFn,
		checkpoint, fullHash, hashState,
	)
}

// incrementalParseFunc reads new JSONL lines from a file
// starting at the given byte offset with the given starting
// ordinal and persisted session ID. Returns parsed messages, the latest
// timestamp (endedAt), bytes consumed (relative to offset), an optional
// authoritative termination status, and any error. The consumed count covers
// only complete, valid JSON lines so it can be used as a safe resume offset.
type incrementalParseFunc func(
	path string, inc *db.IncrementalInfo,
) ([]parser.ParsedMessage, []parser.ClaudeSubagentLink, []parser.ParsedToolCallUpdate, []parser.ParsedMessageTokenUsageUpdate, time.Time, int64, *string, []byte, error)

// tryIncrementalJSONL attempts an incremental parse of an
// append-only JSONL file by reading only bytes appended since
// the last sync. Returns (result, true) on success, or
// (zero, false) to fall through to a full parse. Falls back
// to full parse when the file maps to multiple DB sessions
// (e.g. Claude DAG forks).
func (e *Engine) tryIncrementalJSONL(
	ctx context.Context,
	file parser.DiscoveredFile,
	info os.FileInfo,
	agent parser.AgentType,
	parseFn incrementalParseFunc,
	checkpoint *db.ParserCheckpoint,
	fullHash string,
	hashState []byte,
) (processResult, bool) {
	if e.forceParse || e.forceFullParse || file.ForceFullParse {
		// Parse-diff and explicit full imports never produce append deltas.
		return processResult{}, false
	}
	lookupPath := file.Path
	if e.pathRewriter != nil {
		lookupPath = e.pathRewriter(file.Path)
	}
	inc, ok := e.db.GetSessionForIncremental(ctx, lookupPath, string(agent))
	if !ok || inc.FileSize <= 0 {
		return processResult{}, false
	}

	// A session archived before the cwd allow-list was configured
	// must not keep growing through the append path, which bypasses
	// the prepareSessionWrite veto. Fall back to the full parse path
	// so the same veto applies; it also re-derives the cwd from the
	// whole file, which covers a stored cwd that predates cwd capture.
	if !e.cwdFilter.allows(inc.Cwd) {
		return processResult{}, false
	}

	// Existing rows from an older parser lack new metadata
	// columns. Force a full parse so the rewrite picks them
	// up rather than appending new rows on top of stale ones.
	if e.db.GetSessionDataVersion(ctx, inc.ID) <
		db.CurrentDataVersion() {
		return processResult{}, false
	}

	currentSize := info.Size()

	// A prior sync that stored no message rows has no safe append
	// boundary. Rewritten files can grow in place and keep the same
	// identity, which makes a full-file replacement look like an
	// append from the old file_size offset.
	if inc.MsgCount == 0 {
		return processResult{}, false
	}

	// If the file was replaced (different inode/device), fall
	// back to a full parse so we don't append on top of stale
	// state. Only check when both sides have a known identity
	// (non-zero); zeros mean the data is missing or the
	// filesystem does not expose a stable identity.
	if e.pathRewriter == nil && inc.FileInode != 0 && inc.FileDevice != 0 {
		curInode, curDevice := getFileIdentity(file.Path, info)
		if curInode != 0 && curDevice != 0 &&
			(curInode != inc.FileInode ||
				curDevice != inc.FileDevice) {
			log.Printf(
				"incremental %s %s: file identity changed "+
					"(inode %d→%d, device %d→%d), full parse",
				agent, file.Path,
				inc.FileInode, curInode,
				inc.FileDevice, curDevice,
			)
			return processResult{forceReplace: true}, false
		}
	}
	if currentSize < inc.FileSize {
		if e.pathRewriter != nil {
			log.Printf(
				"incremental %s remote source: file truncated from %d to %d, full parse",
				agent, inc.FileSize, currentSize,
			)
		} else {
			log.Printf(
				"incremental %s %s: file truncated from %d to %d, full parse",
				agent, file.Path, inc.FileSize, currentSize,
			)
		}
		return processResult{forceReplace: true}, false
	}
	if currentSize == inc.FileSize {
		if isCodexFormatAgent(agent) {
			// A Codex-format rollout with no new transcript bytes can still
			// reach the fingerprint path. Codex's composite mtime may also
			// change when session_index.jsonl does. Let the later database
			// freshness checks decide whether to skip or full-parse.
			return processResult{}, false
		}
		log.Printf(
			"incremental %s %s: file size unchanged at %d but changed since last sync, full parse",
			agent, file.Path, currentSize,
		)
		return processResult{forceReplace: true}, false
	}

	// Persist the same effective file_mtime a full parse would store. Codex
	// folds in session_index.jsonl (parser.CodexEffectiveMtime),
	// exactly as ParseCodexSession sets File.Mtime; a full sync of the same
	// file stores that effective value. Keeping the incremental write on the
	// same basis means parse-diff's raced guard -- which reads the freshly
	// parsed effective File.Mtime -- compares against a matching stored
	// file_mtime no matter whether the last write was incremental or full,
	// and shouldSkipCodex's storedMtime==effectiveMtime fast path stays
	// accurate. Other JSONL agents, including TraeX, keep the raw stat.
	incMtime := info.ModTime().UnixNano()
	if agent == parser.AgentCodex {
		incMtime = e.codexMetadata().EffectiveMtime(file.Path, incMtime)
	}

	// Incremental parse seam: every gate above returns a lease-free decline.
	// From here parseFn reads and parses the appended bytes, so acquire the
	// retention lease that bounds the parsed payload. It is attached to the
	// incremental results below and released on every decline (fall-through to
	// a full parse re-acquires at the provider parse seam) or skip return.
	sourceBytes := e.parseRetentionSourceBytes(file)
	lease, leaseErr := e.retentionBudget().acquire(
		ctx, sourceBytes,
	)
	if leaseErr != nil {
		return processResult{err: leaseErr}, true
	}

	newMsgs, links, toolCallUpdates, messageUsageUpdates, endedAt, consumed, terminationStatus, cursor, err := parseFn(
		file.Path, inc,
	)
	if err != nil {
		lease.Release()
		if parser.IsIncrementalFullParseFallback(err) {
			log.Printf(
				"incremental %s %s: %v (explicit full parse fallback)",
				agent, file.Path, err,
			)
			// The fallback fires when appended lines update
			// already-stored rows (toolUseResult.agentId
			// linkage, same-message.id chunk merging). The
			// full parse must replace existing messages —
			// otherwise the append-only write path skips
			// rows whose ordinal ≤ maxOrd and the updates
			// are silently dropped.
			return processResult{forceReplace: true}, false
		}
		log.Printf(
			"incremental %s %s: %v (full parse)",
			agent, file.Path, err,
		)
		return processResult{}, false
	}

	if isCodexFormatAgent(agent) {
		prepareCodexResultIdentities(newMsgs, toolCallUpdates)
	}
	if len(toolCallUpdates) > 0 {
		positions := make([]db.ToolCallPosition, len(toolCallUpdates))
		for i, update := range toolCallUpdates {
			positions[i] = db.ToolCallPosition{MessageOrdinal: update.MessageOrdinal, CallIndex: update.CallIndex}
		}
		missing, err := e.db.HasMissingToolResultMetadata(ctx, inc.ID, positions)
		if err != nil {
			lease.Release()
			return processResult{err: err}, true
		}
		if missing {
			lease.Release()
			return processResult{forceReplace: true}, false
		}
	}

	// Use the offset through the last valid JSON line, not
	// info.Size(), so partial lines at EOF are retried on
	// the next sync.
	newOffset := inc.FileSize + consumed
	// A read that reached bytes the pre-parse stat did not see makes that
	// stat's mtime describe the earlier, shorter file. A writer that appends
	// between the stat and the read therefore leaves file_size describing
	// the later state and file_mtime the earlier one; on the next sync the
	// size matches and the mtime doesn't, which lands in the size-unchanged
	// branch above and forces a full re-parse. Re-stat once the read has
	// finished and adopt it when the file still ends at newOffset, so the
	// stored pair describes one moment. Only a read past the stat is
	// refreshed, which leaves a same-size in-place rewrite's mtime change
	// visible to the next sync.
	if newOffset > info.Size() {
		if after, statErr := os.Stat(file.Path); statErr == nil &&
			after.Size() == newOffset {
			info = after
			incMtime = after.ModTime().UnixNano()
			if agent == parser.AgentCodex {
				incMtime = e.codexMetadata().EffectiveMtime(
					file.Path, incMtime,
				)
			}
		}
	}
	var incHash string
	resumeOK := false
	// Refresh the stored content fingerprint on the incremental path. Codex
	// needs it for parse-diff's raced-skew detection; Claude needs it so
	// providerSingleSessionFresh can compare the stored hash against the
	// on-disk bytes and catch a same-size, same-mtime, same-inode in-place
	// rewrite that the size/mtime/identity skip signals cannot see.
	if fullHash != "" {
		// The stored fingerprint must cover only the committed safe prefix,
		// not an unfinished partial tail at EOF. Resume the hash state
		// through newOffset (the last complete record) rather than trusting
		// the full-file hash computed by the checkpoint gate.
		if state, hash, hashErr := codexResumeHashFn(
			file.Path, inc.FileSize, newOffset, hashState,
		); hashErr == nil {
			incHash = hash
			hashState = state
			resumeOK = true
		} else {
			log.Printf(
				"resuming codex hash for %s at %d: %v",
				file.Path, newOffset, hashErr,
			)
			incHash = fullHash
		}
	} else if agent == parser.AgentClaude && e.pathRewriter == nil && !e.db.ArchiveContent().OmitsToolContent() {
		if state, hash, err := codexResumeHashFn(file.Path, 0, newOffset, nil); err == nil {
			incHash = hash
			hashState = state
			resumeOK = true
		}
	} else if isCodexFormatAgent(agent) || agent == parser.AgentClaude {
		if hash, err := ComputeFileHashPrefix(file.Path, newOffset); err == nil {
			incHash = hash
		}
	}

	// Persist the advanced parser checkpoint in the same transaction as the
	// delta, including Claude's first append that bootstraps the hash state.
	var nextCheckpoint *db.ParserCheckpoint
	var nextCheckpointBlobs *db.ParserCheckpointBlobs
	// Only persist the advanced checkpoint when the resumable hash state was
	// proven to cover the new offset. On reconstruction failure the write
	// keeps the previous checkpoint: its offset now disagrees with the
	// committed file size, so the next gate rejects the stale state instead
	// of resuming at a newer offset and omitting bytes.
	if (agent == parser.AgentClaude || checkpoint != nil && len(cursor) > 0) && hashState != nil && resumeOK {
		if agent == parser.AgentClaude {
			cursor = []byte{}
		}
		cpNextOrdinal := inc.NextOrdinal
		if len(newMsgs) > 0 {
			cpNextOrdinal = nextParsedOrdinal(inc.NextOrdinal, newMsgs)
		}
		anchorDigest, anchorErr := codexCheckpointAnchorDigest(
			file.Path, newOffset,
		)
		if anchorErr != nil {
			log.Printf(
				"building codex checkpoint %s: %v", file.Path, anchorErr,
			)
		} else {
			inode, device := getFileIdentity(file.Path, info)
			changeTime, _ := fileChangeTime(file.Path, info)
			built, blobs := buildCodexCheckpoint(
				inc.ID,
				string(agent),
				e.effectiveSourcePath(file.Path),
				uint64(inode),
				uint64(device),
				incMtime,
				changeTime,
				newOffset,
				cursor,
				hashState,
				incHash,
				cpNextOrdinal,
				anchorDigest,
			)
			nextCheckpoint = built
			nextCheckpointBlobs = &blobs
		}
	}

	totalOut := inc.TotalOutputTokens
	peakCtx := inc.PeakContextTokens
	hasTotalOut := inc.HasTotalOutputTokens
	hasPeakCtx := inc.HasPeakContextTokens
	for _, m := range newMsgs {
		msgHasCtx, msgHasOut := m.TokenPresence()
		// Accumulate from per-message values already bounded to the
		// per-message clamp the central pass applies to the stored rows, so
		// a corrupt new message cannot inflate the session aggregates past
		// what the persisted rows justify (parity with the full path, which
		// re-derives message-derived totals from the clamped rows).
		if msgHasOut {
			totalOut += clampedTokens(m.OutputTokens)
			hasTotalOut = true
		}
		if ctx := clampedTokens(m.ContextTokens); msgHasCtx &&
			(!hasPeakCtx || ctx > peakCtx) {
			peakCtx = ctx
			hasPeakCtx = true
		}
	}
	for _, usageUpdate := range messageUsageUpdates {
		if usageUpdate.HasOutputTokens {
			totalOut += clampedTokens(usageUpdate.OutputTokens)
			hasTotalOut = true
		}
		if ctx := clampedTokens(usageUpdate.ContextTokens); usageUpdate.HasContextTokens && (!hasPeakCtx || ctx > peakCtx) {
			peakCtx = ctx
			hasPeakCtx = true
		}
	}

	if len(newMsgs) == 0 {
		// No new messages, but advance the offset past
		// non-message lines (progress events, metadata)
		// so they aren't re-read on every sync. Carry
		// endedAt forward so session bounds stay current
		// with non-message timestamps (e.g. progress).
		if consumed > 0 {
			return processResult{
				sourceBytes: sourceBytes,
				incremental: &incrementalUpdate{
					agent:                agent,
					sessionID:            inc.ID,
					project:              inc.Project,
					sourceProject:        inc.SourceProject,
					machine:              inc.Machine,
					cwd:                  inc.Cwd,
					recordedGitRemote:    inc.RecordedGitRemote,
					links:                links,
					toolCallUpdates:      toolCallUpdates,
					messageUsageUpdates:  messageUsageUpdates,
					checkpoint:           nextCheckpoint,
					checkpointBlobs:      nextCheckpointBlobs,
					endedAt:              endedAt,
					terminationStatus:    terminationStatus,
					msgCount:             inc.MsgCount,
					userMsgCount:         inc.UserMsgCount,
					fileSize:             newOffset,
					fileMtime:            incMtime,
					fileHash:             incHash,
					nextOrdinal:          inc.NextOrdinal,
					lastEntryUUID:        inc.LastEntryUUID,
					totalOutputTokens:    totalOut,
					peakContextTokens:    peakCtx,
					hasTotalOutputTokens: hasTotalOut,
					hasPeakContextTokens: hasPeakCtx,
				},
				retentionLease: lease,
			}, true
		}
		// A larger source with no complete record consumed is an unfinished
		// append, not evidence that this fingerprint is fully processed. Keep
		// the persisted cursor unchanged and suppress the mtime skip entry so a
		// completed record is retried even when the writer restores the same
		// filesystem timestamp.
		lease.Release()
		return processResult{skip: true, noCacheSkip: true}, true
	}

	// Claude cross-sync split detection: when the first appended
	// assistant message shares its provider message id with the
	// last already-stored assistant message for this session, the
	// previous sync may have stopped mid-stream. The backward scan
	// settles it. When a user record sits between the stored record
	// and the appended one (parallel tool calls), the full parser does
	// not merge them either, so the window is a plain append. When the
	// run straddles the offset, re-parse from its first record and
	// replace only the rows from the run's ordinal onward. Anything the
	// scan cannot classify falls back to a whole-transcript parse.
	if agent == parser.AgentClaude {
		first := newMsgs[0]
		if first.Role == parser.RoleAssistant &&
			first.ClaudeMessageID != "" &&
			e.db.LastClaudeMessageID(ctx, inc.ID) ==
				first.ClaudeMessageID {
			runStart, verdict := parser.ClaudeSplitRunStart(
				file.Path, inc.FileSize, first.ClaudeMessageID,
			)
			var update *incrementalUpdate
			if verdict == parser.ClaudeSplitFound {
				var err error
				update, err = e.tryClaudeSplitSuffixUpdate(
					ctx, file, inc, first.ClaudeMessageID, runStart,
					newOffset, incMtime, incHash, parseFn,
				)
				if err != nil {
					lease.Release()
					return processResult{err: err}, true
				}
			}
			switch {
			case update != nil:
				if nextCheckpoint != nil {
					nextCheckpoint.NextOrdinal = update.nextOrdinal
				}
				update.checkpoint = nextCheckpoint
				update.checkpointBlobs = nextCheckpointBlobs
				log.Printf(
					"incremental %s %s: appended chunk shares"+
						" message.id with stored tail, "+
						"re-parsed the open run only",
					agent, file.Path,
				)
				return processResult{
					sourceBytes:    sourceBytes,
					incremental:    update,
					retentionLease: lease,
				}, true
			case verdict != parser.ClaudeSplitNone:
				log.Printf(
					"incremental %s %s: appended chunk shares"+
						" message.id with stored tail, full parse",
					agent, file.Path,
				)
				lease.Release()
				return processResult{forceReplace: true}, false
			}
		}
	}

	// Claude-only: an empty stored preview means no real user prompt
	// has been parsed yet (a session that starts with injected IDE
	// context, a continuation record, /clear, or /effort). When this
	// chunk carries the first real prompt, fall back to a full parse
	// so first_message is re-derived from the whole file. Chunks
	// without one — streamed assistant work after an auto-compact
	// continuation, or more injected IDE context — stay incremental
	// so per-event work is bounded by the appended bytes rather than
	// the transcript size.
	//
	// Other agents can legitimately have an empty first_message
	// alongside real user rows — for example Codex inserts orphan
	// subagent notifications as Role=user messages that bypass
	// firstMessage — so this fall-through is gated on Claude. Usage-only
	// archives deliberately discard every preview; their incremental
	// automation classifier consumes the raw appended rows instead.
	if !e.db.ArchiveContent().UsageOnly() &&
		agent == parser.AgentClaude && inc.FirstMessage == "" &&
		chunkHasRealUserPrompt(newMsgs) {
		log.Printf(
			"incremental %s %s: first real user prompt after "+
				"empty preview, full parse",
			agent, file.Path,
		)
		lease.Release()
		return processResult{}, false
	}

	newUserCount := countUserMsgs(newMsgs)
	nextOrdinal := nextParsedOrdinal(inc.NextOrdinal, newMsgs)
	lastEntryUUID := lastParsedSourceUUID(inc.LastEntryUUID, newMsgs)

	log.Printf(
		"incremental %s %s: %d new message(s) "+
			"from offset %d",
		agent, inc.ID, len(newMsgs), inc.FileSize,
	)

	return processResult{
		sourceBytes: sourceBytes,
		incremental: &incrementalUpdate{
			agent:                agent,
			sessionID:            inc.ID,
			project:              inc.Project,
			sourceProject:        inc.SourceProject,
			machine:              inc.Machine,
			cwd:                  inc.Cwd,
			recordedGitRemote:    inc.RecordedGitRemote,
			msgs:                 newMsgs,
			links:                links,
			toolCallUpdates:      toolCallUpdates,
			messageUsageUpdates:  messageUsageUpdates,
			checkpoint:           nextCheckpoint,
			checkpointBlobs:      nextCheckpointBlobs,
			endedAt:              endedAt,
			terminationStatus:    terminationStatus,
			msgCount:             inc.MsgCount + len(newMsgs),
			userMsgCount:         inc.UserMsgCount + newUserCount,
			fileSize:             newOffset,
			fileMtime:            incMtime,
			fileHash:             incHash,
			nextOrdinal:          nextOrdinal,
			lastEntryUUID:        lastEntryUUID,
			totalOutputTokens:    totalOut,
			peakContextTokens:    peakCtx,
			hasTotalOutputTokens: hasTotalOut,
			hasPeakContextTokens: hasPeakCtx,
		},
		retentionLease: lease,
	}, true
}

// tryClaudeSplitSuffixUpdate re-parses the same-message.id assistant run
// that starts at runStart and straddles inc.FileSize, at the ordinal the
// stored partial run already occupies, and returns a delta that replaces
// the stored rows from that ordinal onward. A nil update means the
// re-parse cannot be trusted to match a full parse, leaving the caller
// its whole-transcript fallback.
func (e *Engine) tryClaudeSplitSuffixUpdate(
	ctx context.Context,
	file parser.DiscoveredFile,
	inc *db.IncrementalInfo,
	messageID string,
	runStart int64,
	newOffset int64,
	incMtime int64,
	incHash string,
	parseFn incrementalParseFunc,
) (*incrementalUpdate, error) {
	// The ranged write recounts the session from stored rows. Usage-only
	// storage drops most user rows while message counts still follow the
	// source transcript, so those archives keep the full parse.
	if e.db.ArchiveContent().UsageOnly() {
		return nil, nil
	}
	runOrdinal, ok := e.db.LastClaudeAssistantOrdinal(ctx, inc.ID)
	if !ok {
		return nil, nil
	}
	// Timestamp ordering can place a hook after this assistant even
	// when its source bytes precede the run. Replacing the stored suffix
	// would then delete a row the re-parse cannot reread. Only trailing
	// hooks require a full parse; ordinary rows keep the bounded path.
	if inc.NextOrdinal > runOrdinal+1 {
		var hasHook bool
		err := e.db.Reader().QueryRow(ctx, `
			SELECT EXISTS(SELECT 1 FROM messages
			WHERE session_id = ? AND ordinal >= ? AND ordinal < ?
			  AND is_system = 1 AND source_type IN ('attachment', 'system')
			  AND NOT (`+db.ExcludeClaudeHooksSQL("source_subtype")+`))`,
			inc.ID, runOrdinal+1, inc.NextOrdinal,
		).Scan(&hasHook)
		if err != nil {
			return nil, fmt.Errorf("read trailing Claude hook rows: %w", err)
		}
		if hasHook {
			return nil, nil
		}
	}
	scanInc := *inc
	scanInc.FileSize = runStart
	scanInc.NextOrdinal = runOrdinal
	suffix, links, toolCallUpdates, messageUsageUpdates, endedAt, consumed,
		terminationStatus, _, err := parseFn(file.Path, &scanInc)
	if err != nil {
		// Re-parsing from the run's start can be declined by the same
		// fallbacks the window parse has: a queued command sorting ahead
		// of the run head, a rename or ai-title append, or a fork verdict
		// the run boundary makes unresolvable. Those mean "use the
		// whole-transcript path". Anything else is a real read or
		// database failure and propagates.
		if parser.IsIncrementalFullParseFallback(err) ||
			errors.Is(err, parser.ErrDAGDetected) {
			return nil, nil
		}
		return nil, err
	}
	// The re-parse must reproduce the stored commit boundary exactly and
	// place the merged run on its existing ordinal. Anything else means
	// the scan picked a boundary the parser disagrees with. Updates to
	// rows below the run would change aggregates the ranged write does
	// not recount, and a first real prompt must re-derive first_message
	// through the full path, as the append path requires.
	if runStart+consumed != newOffset || len(suffix) == 0 ||
		suffix[0].Ordinal != runOrdinal ||
		suffix[0].ClaudeMessageID != messageID ||
		len(toolCallUpdates) > 0 || len(messageUsageUpdates) > 0 {
		return nil, nil
	}
	if inc.FirstMessage == "" && chunkHasRealUserPrompt(suffix) {
		return nil, nil
	}
	return &incrementalUpdate{
		agent:              file.Agent,
		sessionID:          inc.ID,
		project:            inc.Project,
		sourceProject:      inc.SourceProject,
		machine:            inc.Machine,
		cwd:                inc.Cwd,
		recordedGitRemote:  inc.RecordedGitRemote,
		msgs:               suffix,
		links:              links,
		endedAt:            endedAt,
		terminationStatus:  terminationStatus,
		fileSize:           newOffset,
		fileMtime:          incMtime,
		fileHash:           incHash,
		nextOrdinal:        nextParsedOrdinal(runOrdinal, suffix),
		lastEntryUUID:      lastParsedSourceUUID(inc.LastEntryUUID, suffix),
		suffixReplace:      true,
		replaceFromOrdinal: runOrdinal,
	}, nil
}
