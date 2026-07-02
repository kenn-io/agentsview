package sync

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

// ParseDiffOptions configures a report-only re-parse comparison.
type ParseDiffOptions struct {
	// Agents restricts the run; empty means every provider-authoritative
	// agent whose registered factory can Discover() and re-parse a source.
	// That includes shared-SQLite DB-backed providers that fan a database
	// out to one virtual source per session (Kiro, OpenCode, Forge,
	// Piebald, Warp). Only import-only agents, which have no source to
	// re-parse, are rejected with an error.
	Agents []parser.AgentType
	// Limit caps the number of source files parsed, newest mtime first
	// across all agents. 0 means no limit.
	Limit int
	// Progress, when non-nil, is called as (filesDone, filesTotal) from
	// the result collector.
	Progress func(done, total int)
}

// NewDiffEngine creates an engine for report-only parse-diff runs. It
// forces Ephemeral so nothing is persisted (no skip cache, no sync
// state) and arms the engine's force-parse mode so every discovered
// file is fully re-parsed regardless of stored size/mtime/data_version
// state.
func NewDiffEngine(database *db.DB, cfg EngineConfig) *Engine {
	cfg.Ephemeral = true
	e := NewEngine(database, cfg)
	e.forceParse = true
	return e
}

// ParseDiff re-parses session source files with the current binary,
// runs the result through the same normalization sync applies, and
// compares it against the stored rows. It writes nothing: no sessions,
// no skip cache, no sync state. It holds the engine's sync mutex for
// the duration.
func (e *Engine) ParseDiff(ctx context.Context, opts ParseDiffOptions) (*ParseDiffReport, error) {
	e.syncMu.Lock()
	defer e.syncMu.Unlock()

	resolved, err := e.resolveParseDiffAgents(opts.Agents)
	if err != nil {
		return nil, err
	}
	resolvedSet := make(map[parser.AgentType]bool, len(resolved))

	report := &ParseDiffReport{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		DataVersion: db.CurrentDataVersion(),
		FieldCounts: map[string]int{},
		// Non-nil so a clean run serializes "sessions": [] rather than
		// null, which jq pipelines and typed consumers expect.
		Sessions: []SessionDiff{},
	}
	for _, def := range resolved {
		resolvedSet[def.Type] = true
		report.Agents = append(report.Agents, string(def.Type))
	}

	// Discovery mirrors syncAllLocked's file phase: provider discovery over
	// the configured dirs per agent, then dedupe and the legacy-Kiro shadow
	// filter. Provider discovery already enumerates shared-SQLite sources
	// (Kiro's data.sqlite3, db-mode OpenCode's opencode.db) per session, so
	// no separate db-source synthesis is needed.
	var files []parser.DiscoveredFile
	for _, def := range resolved {
		providerFiles, err := e.parseDiffProviderSources(ctx, def.Type)
		if err != nil {
			return nil, err
		}
		files = append(files, providerFiles...)
	}
	files = dedupeDiscoveredFiles(files)
	files = e.filterShadowedLegacyKiroFiles(files)

	// Newest first by source mtime (composite stats for virtual
	// paths), tie-broken by path so the --limit sample is stable.
	files, cutPaths, limited := sortAndLimitParseDiffFiles(
		files, opts.Limit,
	)
	report.FilesLimited = limited
	report.FilesExamined = len(files)

	// One full snapshot of the archive (empty range = every session,
	// including trashed rows, with full columns and the raw
	// session_name) indexed by ID and by base source path.
	storedSessions, err := e.db.ListSessionsModifiedBetween(
		ctx, "", "", nil, nil,
	)
	if err != nil {
		return nil, fmt.Errorf("parse-diff: list stored sessions: %w", err)
	}
	storedByID := make(map[string]*db.Session, len(storedSessions))
	storedByPath := make(map[string][]*db.Session)
	for i := range storedSessions {
		s := &storedSessions[i]
		storedByID[s.ID] = s
		if s.FilePath != nil && *s.FilePath != "" {
			base := parseDiffSourceKey(*s.FilePath)
			storedByPath[base] = append(storedByPath[base], s)
		}
	}

	fileAgents := make(map[string]parser.AgentType, len(files))
	for _, f := range files {
		fileAgents[f.Path] = f.Agent
	}

	// The worktree resolver caches are not thread-safe; everything
	// below runs in this single collector goroutine.
	resolver := e.loadWorktreeProjectResolver()
	visited := make(map[string]bool)
	var presencePaths []string

	total := len(files)
	if opts.Progress != nil {
		opts.Progress(0, total)
	}
	// A local cancel lets the error-return paths stop the worker pool
	// instead of parsing every remaining file just to drain it.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := e.startWorkers(runCtx, files)
	for i := range total {
		var r syncJob
		select {
		case <-runCtx.Done():
			cancel()
			drainResults(results, total-i)
			return nil, ctx.Err()
		case r = <-results:
		}
		if r.err != nil && runCtx.Err() != nil {
			// Workers emit ctx.Err() for files skipped after
			// cancellation.
			cancel()
			drainResults(results, total-i-1)
			return nil, ctx.Err()
		}
		if r.incremental != nil {
			cancel()
			drainResults(results, total-i-1)
			return nil, fmt.Errorf(
				"parse-diff: internal error: incremental parse of %s "+
					"despite force-parse mode", r.path,
			)
		}
		if err := e.parseDiffCollectFile(
			ctx, report, r, fileAgents, storedByID, storedByPath,
			visited, resolver, &presencePaths,
		); err != nil {
			cancel()
			drainResults(results, total-i-1)
			return nil, err
		}
		if opts.Progress != nil {
			opts.Progress(i+1, total)
		}
	}

	// Presence sweep after all files: stored sessions under a
	// successfully parsed file that no parse result or exclusion
	// accounted for were silently dropped by the current parser.
	e.parseDiffPresenceSweep(report, presencePaths, storedByPath, visited)

	// Final sweep: stored sessions never visited by any file.
	e.parseDiffSweepStored(
		report, storedSessions, resolvedSet,
		len(opts.Agents) == 0, cutPaths, visited,
	)
	// Raced sessions were compared against a fresh parse just like the
	// others, so they count toward Examined; they are simply not counted
	// as drift. Keeping them in Examined also keeps VacuousResync honest:
	// a run with comparable (raced) sessions is not "all pending resync".
	report.Totals.Examined = report.Totals.Identical +
		report.Totals.Changed + report.Totals.PendingResync +
		report.Totals.Raced

	sort.Slice(report.Sessions, func(i, j int) bool {
		a, b := report.Sessions[i], report.Sessions[j]
		if a.Class != b.Class {
			return a.Class < b.Class
		}
		if a.Agent != b.Agent {
			return a.Agent < b.Agent
		}
		if a.FilePath != b.FilePath {
			return a.FilePath < b.FilePath
		}
		return a.SessionID < b.SessionID
	})
	return report, nil
}

// parseDiffProviderSources discovers an agent's on-disk sources through
// the provider facade. It is scoped to a single agent type so parse-diff
// respects the requested agent set.
func (e *Engine) parseDiffProviderSources(
	ctx context.Context,
	agentType parser.AgentType,
) ([]parser.DiscoveredFile, error) {
	factory, ok := e.providerFactories[agentType]
	if !ok || factory == nil {
		return nil, nil
	}
	roots := e.agentDirs[agentType]
	if len(roots) == 0 {
		return nil, nil
	}
	provider := factory.NewProvider(parser.ProviderConfig{
		Roots:   roots,
		Machine: e.machine,
	})
	sources, err := provider.Discover(ctx)
	if err != nil {
		return nil, fmt.Errorf(
			"parse-diff %s provider discovery: %w",
			agentType, err,
		)
	}
	def := provider.Definition()
	var files []parser.DiscoveredFile
	for _, source := range sources {
		sourcePath := providerDiscoveredPath(source)
		if sourcePath == "" {
			continue
		}
		agent := source.Provider
		if agent == "" {
			agent = def.Type
		}
		sourceCopy := source
		discovered := parser.DiscoveredFile{
			Path:            sourcePath,
			Project:         source.ProjectHint,
			Agent:           agent,
			ProviderSource:  &sourceCopy,
			ProviderProcess: true,
		}
		// Thread S3 discovery metadata onto the file exactly as the main sync
		// discovery path does. Without this, s3:// sources carry a zero mtime
		// and parse-diff ordering falls back to a network stat (or treats them
		// as oldest if that stat fails), skewing --limit selection.
		if s3, ok := source.Opaque.(parser.S3DiscoveredSource); ok {
			discovered.Machine = s3.Machine
			discovered.SourceSize = s3.Size
			discovered.SourceMtime = s3.MtimeNS
			discovered.SourceFingerprint = s3.Fingerprint
			if discovered.Project == "" {
				discovered.Project = s3.Project
			}
		}
		files = append(files, discovered)
	}
	return files, nil
}

func (e *Engine) parseDiffAgentDiscoverable(def parser.AgentDef) bool {
	// A provider-authoritative agent with a registered factory exposes a
	// working Discover()/Parse() the report-only run can re-parse, whether it
	// reads literal per-session files (Claude, Codex) or a shared SQLite store
	// its provider fans out to one virtual source per session (Kiro, OpenCode,
	// and the DB-backed Forge/Piebald/Warp providers). FileBased is not
	// consulted here: it distinguishes literal-file agents from DB-backed ones
	// for the watcher and sync paths, but both kinds satisfy the same provider
	// Discover() contract parse-diff needs. Import-only agents are not
	// provider-authoritative, so the switch still rejects them.
	switch e.providerMigrationModes[def.Type] {
	case parser.ProviderMigrationProviderAuthoritative:
		factory, ok := e.providerFactories[def.Type]
		return ok && factory != nil
	default:
		return false
	}
}

// resolveParseDiffAgents validates the requested agent set against
// the registry and returns the matching defs in registry order. Only
// file-based agents with an on-disk source can be re-parsed.
func (e *Engine) resolveParseDiffAgents(
	requested []parser.AgentType,
) ([]parser.AgentDef, error) {
	var allowed []parser.AgentDef
	allowedSet := make(map[parser.AgentType]bool)
	var names []string
	for _, def := range parser.Registry {
		if e.parseDiffAgentDiscoverable(def) {
			allowed = append(allowed, def)
			allowedSet[def.Type] = true
			names = append(names, string(def.Type))
		}
	}
	if len(requested) == 0 {
		return allowed, nil
	}

	supported := strings.Join(names, ", ")
	reqSet := make(map[parser.AgentType]bool, len(requested))
	for _, t := range requested {
		if allowedSet[t] {
			reqSet[t] = true
			continue
		}
		if _, known := parser.AgentByType(t); known {
			return nil, fmt.Errorf(
				"agent %q has no on-disk source to re-parse; "+
					"supported agents: %s", t, supported,
			)
		}
		return nil, fmt.Errorf(
			"unknown agent %q; supported agents: %s", t, supported,
		)
	}
	out := make([]parser.AgentDef, 0, len(reqSet))
	for _, def := range allowed {
		if reqSet[def.Type] {
			out = append(out, def)
		}
	}
	return out, nil
}

// sortAndLimitParseDiffFiles orders files newest-first by source
// mtime (tie-break: path ascending) and applies the file cap. It
// returns the kept files and the base paths of files cut by the
// limit, used by the final sweep's "not sampled" reason.
func sortAndLimitParseDiffFiles(
	files []parser.DiscoveredFile, limit int,
) ([]parser.DiscoveredFile, map[string]bool, bool) {
	mtimes := make(map[string]int64, len(files))
	for _, f := range files {
		if m, ok := parseDiffDiscoveryMtime(f); ok {
			mtimes[f.Path] = m
			continue
		}
		m, err := discoveredFileMtime(f)
		if err != nil {
			m = 0
		}
		mtimes[f.Path] = m
	}
	sort.SliceStable(files, func(i, j int) bool {
		mi, mj := mtimes[files[i].Path], mtimes[files[j].Path]
		if mi != mj {
			return mi > mj
		}
		return files[i].Path < files[j].Path
	})

	cutPaths := map[string]bool{}
	limited := false
	if limit > 0 && len(files) > limit {
		limited = true
		for _, f := range files[limit:] {
			cutPaths[parseDiffSourceKey(f.Path)] = true
		}
		files = files[:limit]
	}
	return files, cutPaths, limited
}

// parseDiffDiscoveryMtime returns a per-source mtime captured at provider
// discovery when the source carries one. The DB-backed providers (Forge,
// Piebald, Warp) fan a shared SQLite store out to one virtual
// "<db>#<sessionID>" source per session; those paths have no on-disk existence,
// so discoveredFileMtime's os.Stat fallback fails and yields 0. With every
// virtual source pinned to 0, the --limit sampler would order them
// lexicographically by path and could cut the newest sessions. The provider
// stamps SourceRef.DiscoveryMTimeNS with each session's real mtime during
// Discover, so use it directly for ordering here. Ordering only: raced-guard
// reliability (parseDiffSourceReliableForRaced) and skip-cache/data-version
// freshness are unaffected.
func parseDiffDiscoveryMtime(f parser.DiscoveredFile) (int64, bool) {
	if f.ProviderSource == nil || f.ProviderSource.DiscoveryMTimeNS == 0 {
		return 0, false
	}
	return f.ProviderSource.DiscoveryMTimeNS, true
}

func parseDiffSourceKey(path string) string {
	if isPerSessionDBVirtualSource(path) {
		return path
	}
	return stripVirtualSourceSuffix(path)
}

// perSessionDBVirtualSourceBases lists the shared-SQLite database filenames
// whose providers discover one virtual source per session (opencode.db#id,
// warp.sqlite#id, ...). parse-diff keys these by their full virtual path, so a
// --limit sample or a per-session parse failure stays scoped to the single
// session it names instead of fanning out across every sibling row in the same
// physical database. Contrast Kiro/Zed/Shelley, whose providers discover the
// database as one source; those key by the stripped database path.
var perSessionDBVirtualSourceBases = []string{
	"opencode.db", "kilo.db", "mimocode.db",
	parser.WarpDBFilename, parser.ForgeDBFilename, parser.PiebaldDBFilename,
}

func isPerSessionDBVirtualSource(path string) bool {
	for _, base := range perSessionDBVirtualSourceBases {
		if _, _, ok := parser.ParseVirtualSourcePathForBase(path, base); ok {
			return true
		}
	}
	return false
}

// stripVirtualSourceSuffix maps a stored file_path to its on-disk base file by
// removing the "#rawID" suffix that Kiro, Zed, OpenCode, Kilo, MiMoCode,
// IcodeMate, Shelley, Forge, Piebald, and Warp SQLite-backed sessions append to
// their shared database path, the "#conversationID" suffix Visual Studio
// Copilot appends to its shared trace file, and the "#runIdx" suffix aider
// appends to its shared history file.
func stripVirtualSourceSuffix(path string) string {
	if tracePath, _, ok := parser.SplitVisualStudioCopilotVirtualPath(path); ok {
		return tracePath
	}
	if historyPath, _, ok := parser.ParseAiderVirtualPath(path); ok {
		return historyPath
	}
	if dbPath, _, ok := parseKiroSQLiteVirtualPath(path); ok {
		return dbPath
	}
	if dbPath, _, ok := parser.ParseVirtualSourcePathForBase(path, "threads.db"); ok {
		return dbPath
	}
	for _, base := range perSessionDBVirtualSourceBases {
		if dbPath, _, ok := parser.ParseVirtualSourcePathForBase(path, base); ok {
			return dbPath
		}
	}
	if dbPath, _, ok := parser.ParseVirtualSourcePathForBase(path, "icodemate.db"); ok {
		return dbPath
	}
	if dbPath, _, ok := parser.ParseVirtualSourcePathForBase(path, shelleyDBFile); ok {
		return dbPath
	}
	return path
}

// parseDiffSourceReliableForRaced reports whether a session's live source
// mtime can be compared against the stored file_mtime on an apples-to-apples
// basis, gating the live-write skew (raced) reclassification.
//
// The raced guard reclassifies a would-be DiffChanged as DiffRaced when the
// live source mtime is newer than the stored per-session file_mtime. The live
// mtime it consults is resolved by parseDiffLiveMtime at collect time, using
// only mtime sources that are safe per-session race signals. For agents with
// session-specific sibling files, it recomputes the same effective basis a real
// sync would persist; for Codex, it deliberately uses the transcript mtime
// instead of the global session_index.jsonl mtime so unrelated title/index
// writes cannot mask transcript parser drift.
//
// Even so, two source shapes are deliberately held OUT of the raced guard so
// it fails CLOSED rather than risk masking genuine parser drift:
//
//   - Virtual-path sources (Aider "#runIdx", Kiro/Zed/OpenCode/Kilo/MiMoCode
//     "#rawID", Shelley, Forge/Piebald/Warp "#sessionID", Visual Studio Copilot
//     "#conversationID"). Many sessions fan out of ONE physical source, so the
//     source mtime is NOT a per-session signal. A shared DB's rows can carry
//     advanced updated_at values, and Aider's File.Mtime is the whole
//     .aider.chat.history.md file's mtime shared by every run in it --
//     appending a new run bumps it for all runs -- so a newer mtime does not
//     mean THIS session's parsed content was torn. Including them would mask
//     genuine drift on untouched siblings; instead they keep their real
//     changed/unchanged verdict (see
//     TestParseDiffDBBackedSourceNotMaskedAsRaced).
//   - Agents that are not plain file-based (no on-disk literal file at all).
//
// Detecting either makes the source unreliable, so the caller skips the raced
// guard entirely. This never masks genuine drift for those agents, while plain
// file-based agents reading a literal file still get the real race protection.
func (e *Engine) parseDiffSourceReliableForRaced(
	agent parser.AgentType, sourcePath string,
) bool {
	// A virtual path carries a recognized "#..." suffix; stripping changes
	// the string only for such paths. The stored file_mtime for these is a
	// per-row/composite value, not trusted as a mid-run race signal. This is
	// the gate that holds the DB-backed providers (Forge/Piebald/Warp and the
	// shared-SQLite families) out of the raced guard now that
	// parseDiffAgentDiscoverable admits them: their virtual paths strip, so
	// they fail closed here and keep their real changed/unchanged verdict.
	if stripVirtualSourceSuffix(sourcePath) != sourcePath {
		return false
	}
	// Only a literal-file agent reads a source whose os.Stat mtime is a
	// per-session race signal that populated file_mtime. FileBased is exactly
	// that predicate, so it is required here even though the discoverability
	// gate no longer consults it: a DB-backed agent (FileBased == false) is
	// never reliable for the raced guard. Its real sources are virtual and were
	// gated out above; requiring FileBased also fails closed on the impossible
	// non-virtual DB path rather than trusting a shared store's composite mtime.
	def, ok := parser.AgentByType(agent)
	if !ok {
		return false
	}
	return def.FileBased && e.parseDiffAgentDiscoverable(def)
}

// parseDiffLiveMtime resolves a session's live source mtime for the raced
// guard, re-stat'd at collect time (after the worker finished reading) and
// recomputing the agent-aware mtime that is safe to use as a per-session race
// signal. Computing it now -- rather than trusting the parser's pre-read
// File.Mtime -- catches a sibling-file write that landed after the parser's own
// stat but before classification when that sibling is session-specific:
//
//   - Codex deliberately uses the transcript mtime only. Its
//     session_index.jsonl is global to every Codex session, so an unrelated
//     title/index write must not mask transcript-derived parser drift.
//   - OpenHands folds base_state.json/TASKS.json/events/* (OpenHandsSnapshot).
//   - Copilot folds workspace.yaml (copilotEffectiveMtime).
//
// Every other reliable agent is handled by discoveredFileMtime, which already
// recomputes their effective mtime at stat time (Cowork/CommandCode/Vibe/
// Reasonix/Antigravity/...). Only literal-file sources reach here -- virtual
// and DB-backed sources are gated out by parseDiffSourceReliableForRaced -- so
// the OpenCode-format storage children (virtual "#rawID" paths) never apply.
func parseDiffLiveMtime(
	agent parser.AgentType, path string,
) (int64, error) {
	switch agent {
	case parser.AgentCodex:
		info, err := os.Stat(path)
		if err != nil {
			return 0, err
		}
		return info.ModTime().UnixNano(), nil
	case parser.AgentOpenHands:
		snapshot, err := parser.OpenHandsSnapshot(path)
		if err != nil {
			return 0, err
		}
		return snapshot.Mtime, nil
	case parser.AgentCopilot:
		info, err := os.Stat(path)
		if err != nil {
			return 0, err
		}
		return copilotEffectiveMtime(path, info), nil
	}
	return discoveredFileMtime(parser.DiscoveredFile{
		Path: path, Agent: agent,
	})
}

// parseDiffCodexTranscriptChangedSinceStored reports whether the Codex
// transcript differs from the archived source snapshot on a size basis Codex
// has historically stored. Full parses store the raw file size, while
// incremental parses can store only the parser-consumed JSONL boundary when a
// partial line trails the file. Codex rows persist file_mtime on an
// index-folded basis so title changes can invalidate sync caches, but
// parse-diff's raced guard uses transcript-only live mtimes to avoid letting the
// global session_index.jsonl mask unrelated parser drift. When the index-folded
// stored mtime is newer than a later transcript append, the mtime comparison
// alone cannot prove the write; the live raw size or parser-consumed JSONL
// boundary can.
func parseDiffCodexTranscriptChangedSinceStored(
	stored *db.Session, parsed parser.ParsedSession,
) bool {
	if stored == nil || parsed.Agent != parser.AgentCodex {
		return false
	}
	if stored.FileSize == nil {
		return false
	}

	storedSize := *stored.FileSize
	info, err := os.Stat(parsed.File.Path)
	if err != nil {
		return true
	}
	if info.Size() == storedSize {
		return false
	}

	consumedSize, err := parser.CodexTranscriptConsumedSize(parsed.File.Path)
	if err != nil {
		return true
	}
	return consumedSize != storedSize
}

// parseDiffCollectFile folds one worker result into the report.
func (e *Engine) parseDiffCollectFile(
	ctx context.Context,
	report *ParseDiffReport,
	job syncJob,
	fileAgents map[string]parser.AgentType,
	storedByID map[string]*db.Session,
	storedByPath map[string][]*db.Session,
	visited map[string]bool,
	resolver worktreeProjectResolver,
	presencePaths *[]string,
) error {
	base := parseDiffSourceKey(job.path)

	if job.err != nil {
		storedHere := storedByPath[base]
		if len(storedHere) == 0 {
			report.Sessions = append(report.Sessions, SessionDiff{
				Agent:    string(fileAgents[job.path]),
				FilePath: job.path,
				Class:    DiffParseError,
				Reason:   job.err.Error(),
			})
			report.Totals.ParseErrors++
			return nil
		}
		for _, s := range storedHere {
			visited[s.ID] = true
			report.Sessions = append(report.Sessions, SessionDiff{
				SessionID:         s.ID,
				Agent:             s.Agent,
				FilePath:          job.path,
				Class:             DiffParseError,
				Reason:            job.err.Error(),
				StoredDataVersion: s.DataVersion,
			})
			report.Totals.ParseErrors++
		}
		return nil
	}

	// Count how many parsed sessions in this job map to each source path. A
	// source shared by more than one session -- e.g. Hermes' state.db, which
	// fans out to every session in the archive under one literal path -- has
	// an mtime that is not a per-session signal: touching it for one session
	// would race-mask genuine drift on its siblings. Those are skipped by the
	// raced guard below (count > 1) and keep their real changed/unchanged
	// verdict, failing closed the same way virtual fan-out sources do.
	sourceSessionCount := make(map[string]int, len(job.results))
	for _, pr := range job.results {
		sourceSessionCount[pr.Session.File.Path]++
	}

	for _, pr := range job.results {
		pw := pendingWrite{
			sess:        pr.Session,
			msgs:        pr.Messages,
			usageEvents: pr.UsageEvents,
			needsRetry:  job.needsRetryForSession(pr.Session.ID),
		}
		prepared, msgs, ok := e.prepareSessionWrite(pw, resolver)
		id := prepared.ID
		if !ok {
			// prepareSessionWrite returns a zero session on veto;
			// reconstruct the final ID the way applyRemoteRewrites
			// would have.
			id = e.idPrefix + pw.sess.ID
		}
		stored := storedByID[id]
		if stored != nil {
			visited[stored.ID] = true
		}

		var fields []FieldDiff
		compare := ok && !pw.needsRetry &&
			stored != nil && stored.DeletedAt == nil
		if compare {
			events, _ := toDBUsageEvents(id, pw.usageEvents)
			var err error
			fields, err = e.compareStoredSession(
				ctx, stored, prepared, msgs, events,
			)
			if err != nil {
				return err
			}
		}
		realDiffs := 0
		for _, f := range fields {
			if !f.Informational {
				realDiffs++
			}
		}

		// Only consult the skew guard when there is a real change to
		// reclassify; an identical or pending-resync session is unaffected
		// by a mid-run write. A nil stored row leaves storedMtime nil.
		//
		// The live mtime is parseDiffLiveMtime, re-computed NOW after
		// the worker finished reading, using only mtimes that are safe
		// per-session race signals. Re-stat'ing at collect time catches
		// session-specific sibling-file writes such as OpenHands'
		// events/*.json and Copilot's workspace.yaml that landed after
		// the parser's own stat but before classification; Codex uses
		// the transcript mtime only because its session_index.jsonl is a
		// global file shared by every Codex session. For other agents,
		// the value is floored by the parsed File.Mtime so a source
		// rewritten with an older mtime mid-run still reads as raced;
		// Codex skips that floor because the parsed File.Mtime also folds
		// in the shared index. If that same index-folded stored mtime hides
		// a newer transcript mtime, Codex falls back to the transcript
		// hash/size fingerprint to prove the source file changed. The
		// guard runs only for
		// reliable, literal-file sources (see
		// parseDiffSourceReliableForRaced); virtual and DB-backed sources
		// are gated out and keep their real changed/unchanged verdict. A
		// re-stat error -- the source was replaced/removed mid-run -- is
		// treated as unreadable -> raced, matching the conservative
		// re-stat policy.
		raced := false
		if realDiffs > 0 && compare &&
			sourceSessionCount[pw.sess.File.Path] == 1 &&
			e.parseDiffSourceReliableForRaced(pw.sess.Agent, pw.sess.File.Path) {
			var storedMtime *int64
			if stored != nil {
				storedMtime = stored.FileMtime
			}
			liveMtime, err := parseDiffLiveMtime(
				pw.sess.Agent, pw.sess.File.Path,
			)
			liveOK := err == nil
			if liveOK && pw.sess.Agent != parser.AgentCodex &&
				pw.sess.File.Mtime > liveMtime {
				liveMtime = pw.sess.File.Mtime
			}
			raced = parseDiffSourceRaced(
				storedMtime, liveMtime, liveOK,
			)
			if !raced && liveOK &&
				parseDiffCodexTranscriptChangedSinceStored(stored, pw.sess) {
				raced = true
			}
		}

		class, reason := classifyParseDiffSession(
			pw.needsRetry,
			ok,
			stored != nil,
			stored != nil && stored.DeletedAt != nil,
			stored != nil && stored.DataVersion < db.CurrentDataVersion(),
			realDiffs,
			raced,
		)

		entry := SessionDiff{
			SessionID: id,
			Agent:     string(pw.sess.Agent),
			FilePath:  pw.sess.File.Path,
			Class:     class,
			Reason:    reason,
			Fields:    fields,
		}
		if stored != nil {
			entry.StoredDataVersion = stored.DataVersion
		}

		switch class {
		case DiffNeedsRetry:
			report.Totals.NeedsRetry++
			report.Sessions = append(report.Sessions, entry)
		case DiffExcluded:
			report.Totals.ExcludedByParser++
			report.Sessions = append(report.Sessions, entry)
		case DiffNewOnDisk:
			report.Totals.NewOnDisk++
			report.Sessions = append(report.Sessions, entry)
		case DiffPendingResync:
			// Field diffs are attached for drill-down but never
			// counted as parser drift (FieldCounts excluded).
			report.Totals.PendingResync++
			report.Sessions = append(report.Sessions, entry)
		case DiffChanged:
			report.Totals.Changed++
			for _, f := range fields {
				if !f.Informational {
					report.FieldCounts[f.Field]++
				}
			}
			report.Sessions = append(report.Sessions, entry)
		case DiffRaced:
			// Inconclusive (live-write skew): listed for visibility with
			// its field diffs attached for drill-down, but excluded from
			// FieldCounts and from HasFailures so --fail-on-change stays
			// trustworthy.
			report.Totals.Raced++
			report.Sessions = append(report.Sessions, entry)
		case DiffSkipped:
			// A re-parsed but trashed session: counted with the rest
			// of the skipped (not-re-parsed) trashed rows.
			report.Totals.Skipped++
			report.Sessions = append(report.Sessions, entry)
		case DiffIdentical:
			report.Totals.Identical++
			if len(fields) > 0 {
				// Informational-only: counted identical, but
				// listed so the explanation is visible.
				report.Totals.InformationalOnly++
				report.Sessions = append(report.Sessions, entry)
			}
		}
	}

	// Per-session parse failures inside a shared SQLite store: the db
	// itself opened fine, so job.err is nil, but individual sessions
	// could not be parsed. Each becomes a DiffParseError; matching the
	// stored row by its virtual source path marks it visited so the
	// presence sweep does not double-report it as "not emitted".
	for _, se := range job.sessionErrs {
		entry := SessionDiff{
			Agent:    string(fileAgents[job.path]),
			FilePath: se.virtualPath,
			Class:    DiffParseError,
			Reason:   se.err.Error(),
		}
		if entry.FilePath == "" {
			entry.FilePath = job.path
		}
		for _, s := range storedByPath[base] {
			if derefString(s.FilePath) == se.virtualPath {
				visited[s.ID] = true
				entry.SessionID = s.ID
				entry.Agent = s.Agent
				entry.StoredDataVersion = s.DataVersion
				break
			}
		}
		report.Sessions = append(report.Sessions, entry)
		report.Totals.ParseErrors++
	}

	for _, exID := range e.applyIDPrefixToSessionIDs(
		job.excludedSessionIDs,
	) {
		stored := storedByID[exID]
		if stored == nil {
			continue
		}
		visited[stored.ID] = true
		report.Sessions = append(report.Sessions, SessionDiff{
			SessionID:         stored.ID,
			Agent:             stored.Agent,
			FilePath:          job.path,
			Class:             DiffExcluded,
			Reason:            "parser exclusion (would delete)",
			StoredDataVersion: stored.DataVersion,
		})
		report.Totals.ExcludedByParser++
	}

	// Legacy source-wide needsRetry output and incomplete provider
	// result sets are transient and low fidelity; missing sessions
	// there are expected, not parser drift. Provider per-session
	// retry state is handled above and should not hide unrelated
	// missing sessions from the same complete source.
	if !job.suppressesPresenceSweepForRetry() && !job.suppressPresenceSweep {
		*presencePaths = append(*presencePaths, base)
	}
	return nil
}

// parseDiffPresenceSweep flags stored sessions whose source file
// parsed cleanly but no longer emits them. Runs after every file has
// been collected so a session re-emitted from a different file (by
// ID) is never falsely reported missing.
func (e *Engine) parseDiffPresenceSweep(
	report *ParseDiffReport,
	presencePaths []string,
	storedByPath map[string][]*db.Session,
	visited map[string]bool,
) {
	for _, base := range presencePaths {
		for _, s := range storedByPath[base] {
			if visited[s.ID] {
				continue
			}
			if s.DeletedAt != nil {
				// Trashed rows are intentionally absent; the final
				// sweep reports them as skipped/trashed.
				continue
			}
			visited[s.ID] = true
			diff := SessionDiff{
				SessionID:         s.ID,
				Agent:             s.Agent,
				FilePath:          derefString(s.FilePath),
				Class:             DiffChanged,
				StoredDataVersion: s.DataVersion,
				Fields: []FieldDiff{{
					Field:  FieldPresence,
					Stored: "stored",
					Parsed: "(not emitted)",
				}},
			}
			// A row below the current data version is pipeline
			// history, not drift: incomplete writes (data_version 0)
			// and orphan-copied rows from earlier resyncs commonly
			// survive in the archive under IDs the current parser no
			// longer derives. Only a current-version row that vanished
			// from its file's parse output is a real presence change.
			if s.DataVersion < db.CurrentDataVersion() {
				diff.Class = DiffPendingResync
				diff.Reason = "stale row; parser no longer emits this ID"
				report.Sessions = append(report.Sessions, diff)
				report.Totals.PendingResync++
				continue
			}
			report.Sessions = append(report.Sessions, diff)
			report.Totals.Changed++
			report.FieldCounts[FieldPresence]++
		}
	}
}

// parseDiffSweepStored classifies every stored session that no
// re-parsed file accounted for. With an explicit agent filter the
// sweep is restricted to those agents; an unrestricted run accounts
// for every stored session, including import-only and
// database-backed agents.
func (e *Engine) parseDiffSweepStored(
	report *ParseDiffReport,
	storedSessions []db.Session,
	resolvedSet map[parser.AgentType]bool,
	unrestricted bool,
	cutPaths map[string]bool,
	visited map[string]bool,
) {
	for i := range storedSessions {
		s := &storedSessions[i]
		if visited[s.ID] {
			continue
		}
		def, defOK := parser.AgentByPrefix(s.ID)
		if !unrestricted && (!defOK || !resolvedSet[def.Type]) {
			continue
		}
		host, _ := parser.StripHostPrefix(s.ID)

		var reason string
		switch {
		case host != "":
			reason = "remote session"
		case s.DeletedAt != nil:
			reason = "trashed"
		case defOK && !def.FileBased &&
			def.EnvVar == "" && def.ConfigKey == "":
			reason = "import-only agent"
		case defOK && !def.FileBased && !e.parseDiffAgentDiscoverable(def):
			// A DB-backed agent parse-diff cannot re-parse (no provider
			// factory). Provider-authoritative DB-backed agents
			// (Forge/Piebald/Warp) are discoverable, so they fall through
			// to the source-based reasons below (not sampled / source
			// missing / not discovered) just like literal-file agents.
			reason = "database-backed agent"
		case s.FilePath == nil || *s.FilePath == "":
			reason = "source missing"
		default:
			sourceKey := parseDiffSourceKey(*s.FilePath)
			sourcePath := stripVirtualSourceSuffix(*s.FilePath)
			switch {
			case cutPaths[sourceKey]:
				reason = "not sampled (--limit)"
			case !statExists(sourcePath):
				reason = "source missing"
			default:
				reason = "not discovered"
			}
		}

		report.Sessions = append(report.Sessions, SessionDiff{
			SessionID:         s.ID,
			Agent:             s.Agent,
			FilePath:          derefString(s.FilePath),
			Class:             DiffSkipped,
			Reason:            reason,
			StoredDataVersion: s.DataVersion,
		})
		report.Totals.Skipped++
	}
}

func statExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
