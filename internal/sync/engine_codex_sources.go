package sync

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

// shouldSkipCodexFingerprint reproduces the legacy shouldSkipCodex decision in
// terms of a provider SourceFingerprint. For Codex, fingerprint MTimeNS folds
// in session_index.jsonl via CodexEffectiveMtime; TraeX uses the transcript
// mtime only. Therefore:
//   - a stored size/hash mismatch or stale data version forces a reparse;
//   - an exact effective-mtime match skips;
//   - an effective mtime ahead of the stored mtime driven only by the index
//     (the raw transcript mtime is still at or below the stored mtime) skips
//     unless this session's stored title differs from the current index title.
//   - an effective mtime below the stored mtime skips when the index is absent,
//     because removing a newer index reveals the unchanged transcript mtime.
func (e *Engine) shouldSkipCodexFingerprint(ctx context.Context,
	agent parser.AgentType,
	path string,
	fingerprint parser.SourceFingerprint,
	semantics parser.ProviderSyncSemantics,
) bool {
	fresh, _ := e.codexFingerprintFreshness(ctx,
		agent, path, fingerprint, semantics,
	)
	return fresh
}

func (e *Engine) codexFingerprintFreshness(ctx context.Context,
	agent parser.AgentType,
	path string,
	fingerprint parser.SourceFingerprint,
	semantics parser.ProviderSyncSemantics,
) (fresh, metadataVerified bool) {
	lookupPath := path
	if e.pathRewriter != nil {
		lookupPath = e.pathRewriter(path)
	}
	storedSize, storedMtime, ok := e.db.GetFileInfoByAgentPath(ctx,
		lookupPath, string(agent),
	)
	if !ok || storedSize != fingerprint.Size {
		return false, false
	}
	if !e.providerFingerprintHashMatchesDB(ctx,
		agent, lookupPath,
		fingerprint,
		semantics.FingerprintHashRequiredForFreshness,
	) {
		return false, false
	}
	if project, ok := e.db.GetProjectByAgentPath(ctx,
		lookupPath, string(agent),
	); ok &&
		parser.NeedsProjectReparse(project) {
		return false, false
	}
	if e.db.GetDataVersionByAgentPath(ctx, lookupPath, string(agent)) <
		db.CurrentDataVersion() {
		return false, false
	}
	effectiveMtime := fingerprint.MTimeNS
	if agent != parser.AgentCodex {
		return storedMtime == effectiveMtime, true
	}
	statFresh := storedMtime == effectiveMtime
	if effectiveMtime < storedMtime {
		// Only an index that is absent from every home means the stored
		// mtime came from a since-removed sidecar.
		indexPaths := e.codexMetadata().IndexPaths(path)
		allAbsent := len(indexPaths) > 0
		for _, indexPath := range indexPaths {
			if _, err := os.Stat(indexPath); !errors.Is(err, os.ErrNotExist) {
				allAbsent = false
				break
			}
		}
		if allAbsent {
			statFresh = true
		}
	}
	if effectiveMtime > storedMtime {
		fileMtime := effectiveMtime
		if info, err := os.Stat(path); err == nil {
			fileMtime = info.ModTime().UnixNano()
		}
		statFresh = fileMtime <= storedMtime
	}
	if !statFresh {
		return false, false
	}
	changed, verified := e.codexIndexSessionNameState(ctx, path)
	if !verified {
		return true, false
	}
	return !changed, true
}

// codexIndexNeedsRefreshSince reports whether a Codex session whose transcript
// predates the cutoff still needs a refresh because its session_index.jsonl
// title changed at or after the cutoff. It compares the index title to the
// stored session_name directly rather than gating on indexMtime > storedMtime:
// the incremental write folds the index mtime into the stored file_mtime, so a
// title-only rename whose index mtime is <= that stored value would otherwise
// be filtered out and the stale title would never resolve.
func (e *Engine) codexIndexNeedsRefreshSince(ctx context.Context,
	path string, cutoffNs int64,
) bool {
	indexMtime := e.codexMetadata().EffectiveMtime(path, 0)
	if indexMtime == 0 || indexMtime < cutoffNs {
		return false
	}
	lookupPath := path
	if e.pathRewriter != nil {
		lookupPath = e.pathRewriter(path)
	}
	if _, _, ok := e.db.GetFileInfoByPath(ctx, lookupPath); !ok {
		return false
	}
	return e.codexIndexSessionNameChanged(ctx, path)
}

func (e *Engine) codexIndexSessionNameChanged(
	ctx context.Context, path string,
) bool {
	changed, _ := e.codexIndexSessionNameState(ctx, path)
	return changed
}

func (e *Engine) codexIndexSessionNameState(
	ctx context.Context, path string,
) (changed, verified bool) {
	uuid := parser.CodexSessionUUIDFromFilename(filepath.Base(path))
	if uuid == "" {
		return false, false
	}
	currentName, ok, err := e.codexMetadata().ReadThreadName(path, uuid)
	if err != nil {
		return false, false
	}
	if !ok {
		// No index entry means no rename signal, not a rename to empty.
		// Modern Codex releases stopped writing session_index.jsonl; a
		// stored title compared against the absent index would force a
		// full re-parse of every titled session on every sync, and the
		// rewrite preserves the title, so the loop could never converge.
		return false, true
	}
	if e.db.ArchiveContent().UsageOnly() {
		// Usage-only archives store no titles, so a rename has nothing to
		// refresh and the empty stored title must not read as stale.
		return false, true
	}
	storedName, found, err := e.db.GetSessionName(
		ctx, e.idPrefix+"codex:"+uuid,
	)
	if err != nil || !found {
		return true, true
	}
	return codexSessionNameDiffers(storedName, currentName), true
}

// codexCachedIndexSessionNameState limits title-based cache invalidation to
// sources that already have stored session state. A cached parse failure has
// no title to refresh and its missing row counts as verified for cache-only
// retry suppression; no digest can be stamped without a stored row hash.
func (e *Engine) codexCachedIndexSessionNameState(ctx context.Context,
	path string,
) (changed, verified bool) {
	lookupPath := path
	if e.pathRewriter != nil {
		lookupPath = e.pathRewriter(path)
	}
	if _, _, ok := e.db.GetFileInfoByPath(ctx, lookupPath); !ok {
		return false, true
	}
	return e.codexIndexSessionNameState(ctx, path)
}

// classifyCodexIndexPath maps a Codex session_index.jsonl change to the
// session files whose stored title no longer matches the index. The live
// watcher sees this file only because its parent directory is watched
// shallowly (see ResolveCodexShallowWatchRoots); without this translation a
// title-only rename would not refresh until the next periodic sync, since the
// session transcript itself is untouched.
func (e *Engine) classifyCodexIndexPath(ctx context.Context,
	path string,
) []parser.DiscoveredFile {
	if filepath.Base(path) != parser.CodexSessionIndexFilename {
		return nil
	}
	var sessionRoots []string
	for _, agDir := range e.sources().agentDirs[parser.AgentCodex] {
		if agDir == "" {
			continue
		}
		if slices.Contains(e.codexMetadata().IndexFiles(agDir), path) {
			sessionRoots = append(sessionRoots, agDir)
		}
	}
	if len(sessionRoots) == 0 {
		return nil
	}
	parser.EvictCodexSessionIndex(path)
	titles := parser.CodexSessionIndexTitles(path)
	if len(titles) == 0 {
		return nil
	}

	var out []parser.DiscoveredFile
	for uuid, title := range titles {
		for _, rawID := range e.codexIndexRenamedRawIDs(ctx, uuid, title) {
			if chosen, ok := e.codexIndexSourceForRawID(
				ctx, sessionRoots, rawID,
			); ok {
				out = append(out, chosen)
			}
		}
	}
	return out
}

// codexIndexRenamedRawIDs lists the raw session ids of one index entry whose
// stored title differs from it: the thread itself and each stored revert page
// of the thread (codex:<thread>_<rollout>), which carry the thread's title.
func (e *Engine) codexIndexRenamedRawIDs(ctx context.Context,
	uuid, title string,
) []string {
	var rawIDs []string
	if e.codexStoredNameDiffers(ctx, uuid, title) {
		rawIDs = append(rawIDs, uuid)
	}
	threadSessionID := e.idPrefix + "codex:" + uuid
	pageIDs, err := e.db.CodexRevertPageSessionIDs(ctx, threadSessionID)
	if err != nil {
		log.Printf("codex index rename: listing revert pages of %s: %v", threadSessionID, err)
		return rawIDs
	}
	for _, id := range pageIDs {
		raw := strings.TrimPrefix(id, e.idPrefix+"codex:")
		if parser.CodexThreadIDFromSessionKey(raw) != uuid {
			continue
		}
		if e.codexStoredNameDiffersBySessionID(ctx, id, title, false) {
			rawIDs = append(rawIDs, raw)
		}
	}
	return rawIDs
}

// codexIndexSourceForRawID resolves a Codex session key to its source file
// under the index's roots and pins the provider source to the chosen copy.
func (e *Engine) codexIndexSourceForRawID(ctx context.Context,
	sessionRoots []string, rawID string,
) (parser.DiscoveredFile, bool) {
	var candidates []parser.DiscoveredFile
	for _, root := range sessionRoots {
		if src := e.codexSourceFileForUUID(ctx, root, rawID); src != "" {
			candidates = append(candidates, parser.DiscoveredFile{
				Path:    src,
				Agent:   parser.AgentCodex,
				Machine: e.machineForPath(parser.AgentCodex, src),
			})
		}
	}
	if len(candidates) == 0 {
		return parser.DiscoveredFile{}, false
	}
	// A UUID can exist in both sessions/ and archived_sessions/.
	// Prefer the path the DB already tracks so a title rename does
	// not reparse a stale duplicate over the stored copy.
	chosen := e.pickPreferredCodexIndexDiscoveredFile(ctx, candidates)
	// Pin the provider source to the chosen path and route it through the
	// provider so processProviderFile parses exactly this copy instead of
	// re-canonicalizing the UUID to the preferred dated layout, which would
	// undo the DB-aware selection above.
	chosen.ProviderProcess = true
	chosen.ProviderSource = e.codexPinnedProviderSource(
		parser.AgentCodex, chosen.Path,
	)
	return chosen, true
}

func (e *Engine) pickPreferredCodexIndexDiscoveredFile(ctx context.Context,
	candidates []parser.DiscoveredFile,
) parser.DiscoveredFile {
	if len(candidates) > 0 {
		uuid := ""
		for _, candidate := range candidates {
			uuid = parser.CodexSessionUUIDFromFilename(filepath.Base(candidate.Path))
			if uuid != "" {
				break
			}
		}
		storedPath := e.db.GetSessionFilePath(ctx, e.idPrefix+"codex:"+uuid)
		if uuid != "" && storedPath != "" {
			storedPath = filepath.Clean(storedPath)
			for _, candidate := range candidates {
				if filepath.Clean(e.effectiveSourcePath(candidate.Path)) == storedPath {
					return candidate
				}
			}
		}
	}
	return pickPreferredCodexDiscoveredFile(ctx, e.db, candidates)
}

// codexSourceFileForUUID resolves a Codex session UUID to its on-disk
// transcript path under a single sessions root, preferring the live dated
// layout over a flat archived entry. It scopes a Codex provider to that one
// root so the provider's cross-root live-over-archived canonicalization does
// not collapse a per-root duplicate; classifyCodexIndexPath then applies its
// own DB-aware preference across the per-root candidates. Returns "" when the
// provider, source lookup, or path resolution fails.
func (e *Engine) codexSourceFileForUUID(
	ctx context.Context, root, uuid string,
) string {
	factory, ok := e.sources().providerFactories[parser.AgentCodex]
	if !ok || factory == nil {
		return ""
	}
	provider := factory.NewProvider(parser.ProviderConfig{
		Roots:   []string{root},
		Machine: e.machine,
	})
	source, found, err := provider.FindSource(
		ctx,
		parser.FindSourceRequest{RawSessionID: uuid},
	)
	if err != nil || !found {
		return ""
	}
	return providerDiscoveredPath(source)
}

// codexPinnedProviderSource builds a Codex-format provider SourceRef pinned to
// the exact path, bypassing the provider's live-over-archived canonicalization.
// It is used when the engine's DB-aware or mtime-aware logic has already chosen
// which on-disk copy of a duplicated UUID to parse, so processProviderFile
// parses that copy instead of the provider's preferred dated layout. The agent
// selects the provider so a fork's path is pinned under the fork's own roots.
// Returns nil when that provider or the path's source shape is unavailable.
func (e *Engine) codexPinnedProviderSource(
	agent parser.AgentType, path string,
) *parser.SourceRef {
	factory, ok := e.sources().providerFactories[agent]
	if !ok || factory == nil {
		return nil
	}
	provider := factory.NewProvider(parser.ProviderConfig{
		Roots:   e.sources().agentDirs[agent],
		Machine: e.machine,
	})
	pinner, ok := provider.(interface {
		SourceRefForPath(string) (parser.SourceRef, bool)
	})
	if !ok {
		return nil
	}
	source, ok := pinner.SourceRefForPath(path)
	if !ok {
		return nil
	}
	return &source
}

// codexStoredNameDiffers reports whether the stored session_name for a Codex
// session differs from the given index title. Unknown sessions return false:
// a brand-new session is synced through its own transcript event, not the
// index, so the index path only refreshes renames of already-synced sessions.
func (e *Engine) codexStoredNameDiffers(
	ctx context.Context, uuid, indexTitle string,
) bool {
	return e.codexStoredNameDiffersBySessionID(
		ctx, e.idPrefix+"codex:"+uuid, indexTitle, false,
	)
}

func (e *Engine) codexStoredNameDiffersBySessionID(
	ctx context.Context,
	sessionID, indexTitle string,
	missingDiffers bool,
) bool {
	if e.db.ArchiveContent().UsageOnly() {
		return false
	}
	storedName, found, err := e.db.GetSessionName(ctx, sessionID)
	if err != nil || !found {
		return missingDiffers
	}
	return codexSessionNameDiffers(storedName, indexTitle)
}

func codexSessionNameDiffers(storedName, indexTitle string) bool {
	return strings.TrimSpace(indexTitle) != strings.TrimSpace(storedName)
}

func pickPreferredCodexDiscoveredFile(ctx context.Context,
	database *db.DB, candidates []parser.DiscoveredFile,
) parser.DiscoveredFile {
	if len(candidates) == 0 {
		return parser.DiscoveredFile{}
	}
	if id := parser.CodexSessionUUIDFromFilename(
		filepath.Base(candidates[0].Path),
	); id != "" {
		sessionID := "codex:" + id
		for _, candidate := range candidates {
			storedPath := database.GetSessionFilePath(ctx, applyIDPrefixToID(
				discoveredFileIDPrefix(candidate), sessionID,
			))
			if storedPath == "" {
				continue
			}
			storedPath = filepath.Clean(storedPath)
			for _, candidate := range candidates {
				if filepath.Clean(candidate.Path) == storedPath {
					return candidate
				}
			}
		}
	}
	chosen := candidates[0]
	for _, candidate := range candidates[1:] {
		if preferDiscoveredFile(candidate, chosen) {
			chosen = candidate
		}
	}
	return chosen
}

func dedupeDiscoveredFilesPreferNewestCodex(
	files []parser.DiscoveredFile,
) []parser.DiscoveredFile {
	return dedupeDiscoveredFilesByPreference(files, preferNewestCodexDiscoveredFile)
}

func preferNewestCodexDiscoveredFile(
	candidate, current parser.DiscoveredFile,
) bool {
	if candidate.Agent == current.Agent && isCodexFormatAgent(candidate.Agent) {
		candMTime, candOK := discoveredFileMTime(candidate.Path)
		currMTime, currOK := discoveredFileMTime(current.Path)
		if candOK && currOK && candMTime != currMTime {
			return candMTime > currMTime
		}
	}
	return preferDiscoveredFile(candidate, current)
}

// expandCodexProviderDuplicates re-adds the on-disk duplicate paths of each
// discovered Codex-format source. The provider deduplicates a UUID's live and
// archived copies to the preferred layout at discovery time; this restores the
// dropped duplicates (scoped to the configured roots) so an mtime cutoff filter
// can judge each copy on its own mtime, matching the legacy discover-then-filter
// order. Files of other agents, and Codex-format files without a UUID-shaped
// name, pass through unchanged. Duplicates are keyed by path so nothing is added
// twice. Each agent is expanded against its own roots and re-added under its own
// identity: a fork's UUID must not be resolved through the Codex provider.
func (e *Engine) expandCodexProviderDuplicates(
	files []parser.DiscoveredFile, scope *rootSyncScope,
) []parser.DiscoveredFile {
	pathers := make(map[parser.AgentType]func(string) []string)
	seen := make(map[string]struct{}, len(files))
	for _, f := range files {
		seen[string(f.Agent)+"\x00"+filepath.Clean(f.Path)] = struct{}{}
	}
	out := files
	for _, f := range files {
		if !isCodexFormatAgent(f.Agent) {
			continue
		}
		pather, resolved := pathers[f.Agent]
		if !resolved {
			pather = e.codexUUIDPathLister(f.Agent, scope)
			pathers[f.Agent] = pather
		}
		if pather == nil {
			continue
		}
		uuid := parser.CodexSessionUUIDFromFilename(filepath.Base(f.Path))
		if uuid == "" {
			continue
		}
		for _, dup := range pather(uuid) {
			key := string(f.Agent) + "\x00" + filepath.Clean(dup)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, parser.DiscoveredFile{
				Path:            dup,
				Agent:           f.Agent,
				Machine:         e.machineForPath(f.Agent, dup),
				ProviderProcess: true,
				ProviderSource:  e.codexPinnedProviderSource(f.Agent, dup),
			})
		}
	}
	return out
}

// codexUUIDPathLister returns a function that lists every on-disk transcript
// path of the given Codex-format agent for a UUID under the in-scope roots, or
// nil when that provider is unavailable. It scopes a single provider to the
// in-scope roots so the returned paths cover both the live dated and flat
// archived copies of a duplicated UUID, including duplicates that share one
// root.
func (e *Engine) codexUUIDPathLister(
	agent parser.AgentType, scope *rootSyncScope,
) func(string) []string {
	factory, ok := e.sources().providerFactories[agent]
	if !ok || factory == nil {
		return nil
	}
	roots := make([]string, 0, len(e.sources().agentDirs[agent]))
	for _, root := range e.sources().agentDirs[agent] {
		if root == "" || !scope.includes(root) {
			continue
		}
		roots = append(roots, root)
	}
	if len(roots) == 0 {
		return nil
	}
	provider := factory.NewProvider(parser.ProviderConfig{
		Roots:          roots,
		Machine:        e.machine,
		SourceMachines: e.sources().sourceMachines[agent],
	})
	lister, ok := provider.(interface {
		AllSourcePathsForUUID(string) []string
	})
	if !ok {
		return nil
	}
	return lister.AllSourcePathsForUUID
}
