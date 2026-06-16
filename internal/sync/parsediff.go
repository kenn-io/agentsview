package sync

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

// ParseDiffOptions configures a report-only re-parse comparison.
type ParseDiffOptions struct {
	// Agents restricts the run; empty means every file-based agent with
	// a DiscoverFunc. Agents without an on-disk source to re-parse
	// (database-backed or import-only) are rejected with an error.
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

	resolved, err := resolveParseDiffAgents(opts.Agents)
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

	// Discovery mirrors syncAllLocked's file phase: per-agent
	// DiscoverFunc over the configured dirs, then dedupe and the
	// legacy-Kiro shadow filter.
	var files []parser.DiscoveredFile
	for _, def := range resolved {
		for _, d := range e.agentDirs[def.Type] {
			files = append(files, def.DiscoverFunc(d)...)
		}
	}
	// DiscoverFunc does not emit the shared-SQLite source for Kiro
	// (data.sqlite3) or db-mode OpenCode (opencode.db) — normal sync
	// reaches those through dedicated phases. Synthesize them here so
	// their sessions are actually re-parsed; processKiro/processOpenCode
	// fan one db path out to every contained session under forceParse.
	files = append(files, e.parseDiffDatabaseSources(resolved)...)
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
			base := stripVirtualSourceSuffix(*s.FilePath)
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
	parseDiffSweepStored(
		report, storedSessions, resolvedSet,
		len(opts.Agents) == 0, cutPaths, visited,
	)
	report.Totals.Examined = report.Totals.Identical +
		report.Totals.Changed + report.Totals.PendingResync

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

// resolveParseDiffAgents validates the requested agent set against
// the registry and returns the matching defs in registry order. Only
// file-based agents with a DiscoverFunc have an on-disk source to
// re-parse.
func resolveParseDiffAgents(
	requested []parser.AgentType,
) ([]parser.AgentDef, error) {
	var allowed []parser.AgentDef
	allowedSet := make(map[parser.AgentType]bool)
	var names []string
	for _, def := range parser.Registry {
		if def.FileBased && def.DiscoverFunc != nil {
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

// parseDiffDatabaseSources synthesizes DiscoveredFile entries for the
// shared-SQLite agent stores that DiscoverFunc does not emit: Kiro's
// data.sqlite3, OpenCode's opencode.db, and Kilo's kilo.db. The
// corresponding process functions recognize those base filenames and fan
// one db path out to every contained session, so routing them through the
// normal worker loop re-parses every CLI Kiro / DB-backed OpenCode /
// DB-backed Kilo session.
// Without this, those sessions fall to the "not discovered" sweep and
// an --agent kiro / --agent opencode run would pass while comparing
// nothing.
//
// The OpenCode db is added whenever it exists, regardless of which
// source mode ResolveOpenCodeSource picks: normal sync reads
// opencode.db in storage-mode roots too (openCodePendingSessionIDs),
// because a migrated root can still hold DB-only legacy sessions. Kilo
// uses the same hybrid storage model. The storage-ID filtering in each
// process function keeps file-backed sessions from being compared twice.
func (e *Engine) parseDiffDatabaseSources(
	resolved []parser.AgentDef,
) []parser.DiscoveredFile {
	var extra []parser.DiscoveredFile
	for _, def := range resolved {
		switch def.Type {
		case parser.AgentKiro:
			for _, dir := range e.agentDirs[def.Type] {
				if dir == "" {
					continue
				}
				if dbPath := parser.FindKiroSQLiteDBPath(dir); dbPath != "" {
					extra = append(extra, parser.DiscoveredFile{
						Path: dbPath, Agent: parser.AgentKiro,
					})
				}
			}
		case parser.AgentOpenCode, parser.AgentKilo, parser.AgentMiMoCode:
			for _, dir := range e.agentDirs[def.Type] {
				if dir == "" {
					continue
				}
				dbPath := filepath.Join(
					dir, openCodeFormatDBName(def.Type),
				)
				if info, err := os.Stat(dbPath); err == nil &&
					!info.IsDir() {
					extra = append(extra, parser.DiscoveredFile{
						Path: dbPath, Agent: def.Type,
					})
				}
			}
		}
	}
	return extra
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
			cutPaths[stripVirtualSourceSuffix(f.Path)] = true
		}
		files = files[:limit]
	}
	return files, cutPaths, limited
}

// stripVirtualSourceSuffix maps a stored file_path to its on-disk
// base file by removing the "#rawID" suffix Kiro, Zed, OpenCode, and
// Kilo SQLite-backed sessions append to their shared database path.
func stripVirtualSourceSuffix(path string) string {
	if dbPath, _, ok := parser.ParseKiroSQLiteVirtualPath(path); ok {
		return dbPath
	}
	if dbPath, _, ok := parser.ParseZedSQLiteVirtualPath(path); ok {
		return dbPath
	}
	if dbPath, _, ok := parser.ParseOpenCodeSQLiteVirtualPath(path); ok {
		return dbPath
	}
	if dbPath, _, ok := parser.ParseKiloSQLiteVirtualPath(path); ok {
		return dbPath
	}
	if dbPath, _, ok := parser.ParseMiMoCodeSQLiteVirtualPath(path); ok {
		return dbPath
	}
	return path
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
	base := stripVirtualSourceSuffix(job.path)

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

	for _, pr := range job.results {
		pw := pendingWrite{
			sess:        pr.Session,
			msgs:        pr.Messages,
			usageEvents: pr.UsageEvents,
			needsRetry:  job.needsRetry,
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
			events := toDBUsageEvents(id, pw.usageEvents)
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

		class, reason := classifyParseDiffSession(
			pw.needsRetry,
			ok,
			stored != nil,
			stored != nil && stored.DeletedAt != nil,
			stored != nil && stored.DataVersion < db.CurrentDataVersion(),
			realDiffs,
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

	// needsRetry output is transient and low fidelity; missing
	// sessions there are expected, not parser drift.
	if !job.needsRetry {
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
func parseDiffSweepStored(
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
		case defOK && !def.FileBased:
			reason = "database-backed agent"
		case s.FilePath == nil || *s.FilePath == "":
			reason = "source missing"
		default:
			base := stripVirtualSourceSuffix(*s.FilePath)
			switch {
			case cutPaths[base]:
				reason = "not sampled (--limit)"
			case !statExists(base):
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
