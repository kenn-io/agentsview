package sync

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"go.kenn.io/agentsview/internal/parser"
)

// ChangedPathPlan is the bounded import projection for a set of physical
// mirror paths. Attribution remains private because paths are debug-only data.
type ChangedPathPlan struct {
	Files             []parser.DiscoveredFile
	FallbackProviders []parser.AgentType
	attribution       map[string]changedPathAttribution
}

type changedPathAttribution struct {
	files             []parser.DiscoveredFile
	fallbackProviders []parser.AgentType
	provenIrrelevant  bool
}

// ChangedPathPruneScope is the cache-invalidation projection of a plan.
type ChangedPathPruneScope struct {
	Files             []parser.DiscoveredFile
	FallbackProviders []parser.AgentType
}

// PlanChangedPathsContext classifies physical mirror paths without widening a
// provider-local uncertainty into an all-provider import.
func (e *Engine) PlanChangedPathsContext(
	ctx context.Context,
	physicalPaths []string,
) (ChangedPathPlan, error) {
	plan := ChangedPathPlan{attribution: make(map[string]changedPathAttribution, len(physicalPaths))}
	for _, rawPath := range physicalPaths {
		path, err := normalizeChangedPhysicalPath(rawPath)
		if err != nil {
			return ChangedPathPlan{}, err
		}
		if _, exists := plan.attribution[path]; exists {
			continue
		}
		attribution, claimed, err := e.planOneChangedPath(ctx, path)
		if err != nil {
			return ChangedPathPlan{}, err
		}
		if !claimed {
			attribution.provenIrrelevant = true
		}
		plan.attribution[path] = attribution
	}
	if err := e.resolveClaudeDuplicateAttribution(ctx, &plan); err != nil {
		return ChangedPathPlan{}, err
	}
	plan.rebuildAggregates()
	return plan, nil
}

func normalizeChangedPhysicalPath(path string) (string, error) {
	if path == "" || strings.IndexByte(path, 0) >= 0 || !filepath.IsAbs(path) {
		return "", fmt.Errorf("changed path is not a trusted absolute physical path")
	}
	return filepath.Clean(path), nil
}

func (e *Engine) planOneChangedPath(
	ctx context.Context,
	path string,
) (changedPathAttribution, bool, error) {
	var attribution changedPathAttribution
	claimed := false
	agents := e.sortedAuthoritativeProviderAgents()
	for _, agent := range agents {
		roots := e.agentDirs[agent]
		if len(roots) == 0 {
			continue
		}
		factory := e.providerFactories[agent]
		if factory == nil {
			continue
		}
		if factory.Definition().Type != agent {
			return changedPathAttribution{}, false, fmt.Errorf(
				"changed-path provider ownership mismatch for %s", agent,
			)
		}
		provider := factory.NewProvider(parser.ProviderConfig{
			Roots: roots, Machine: e.machine,
			SourceMachines: e.sourceMachines[agent], PathRewriter: e.pathRewriter,
		})
		watchRoots, err := e.providerChangedPathWatchRoots(ctx, agent, provider, roots)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return changedPathAttribution{}, false, err
			}
			if changedPathWithinAnyRoot(path, roots) {
				claimed = true
				attribution.addFallback(agent)
			}
			continue
		}
		if !changedPathWithinAnyRoot(path, roots) &&
			!changedPathWithinAnyRoot(path, watchRoots) {
			continue
		}
		claimed = true
		if agent == parser.AgentCodex &&
			filepath.Base(path) == parser.CodexSessionIndexFilename {
			// The index is global to every Codex rollout. Reuse the DB-aware
			// classifier so title-only changes select only changed sessions and
			// preserve the live/archive copy already tracked by the archive.
			attribution.files = append(
				attribution.files, e.classifyCodexIndexPath(path)...,
			)
			continue
		}
		matchingWatchRoots := matchingChangedPathRoots(path, watchRoots)
		if len(matchingWatchRoots) == 0 {
			matchingWatchRoots = matchingChangedPathRoots(path, roots)
		}
		if len(matchingWatchRoots) == 0 {
			return changedPathAttribution{}, false, fmt.Errorf(
				"cannot determine %s provider ownership for changed path", agent,
			)
		}
		providerUnproven := false
		for _, watchRoot := range matchingWatchRoots {
			request := parser.ChangedPathRequest{
				Path: path, EventKind: providerChangedPathEventKind(path), WatchRoot: watchRoot,
				AllowWatermarkOnlySources: true,
			}
			relevance, relevanceErr := parser.ResolveChangedPathRelevance(ctx, provider, request)
			if errors.Is(relevanceErr, context.Canceled) ||
				errors.Is(relevanceErr, context.DeadlineExceeded) {
				return changedPathAttribution{}, false, relevanceErr
			}
			if relevanceErr != nil {
				providerUnproven = true
				continue
			}
			if relevance == parser.ChangedPathNonData {
				continue
			}
			// Relevance classification is an optional prefilter. An
			// unclassified path may still map exactly through the provider's
			// changed-path source classifier.
			ok, hintErr := e.addPhysicalStoredSourceHints(ctx, provider, &request)
			if errors.Is(hintErr, context.Canceled) ||
				errors.Is(hintErr, context.DeadlineExceeded) {
				return changedPathAttribution{}, false, hintErr
			}
			if hintErr != nil || !ok {
				providerUnproven = true
				continue
			}
			sources, sourceErr := provider.SourcesForChangedPath(ctx, request)
			if errors.Is(sourceErr, context.Canceled) ||
				errors.Is(sourceErr, context.DeadlineExceeded) {
				return changedPathAttribution{}, false, sourceErr
			}
			if sourceErr != nil {
				providerUnproven = true
				continue
			}
			if len(sources) == 0 {
				providerUnproven = true
				continue
			}
			if agent == parser.AgentOmnigent {
				sources, sourceErr = e.expandOmnigentInheritedMetadataSources(
					ctx, provider, sources,
				)
				if errors.Is(sourceErr, context.Canceled) ||
					errors.Is(sourceErr, context.DeadlineExceeded) {
					return changedPathAttribution{}, false, sourceErr
				}
				if sourceErr != nil {
					providerUnproven = true
					continue
				}
			}
			exactSources := 0
			for _, source := range sources {
				if file, ok := e.changedPathDiscoveredFile(provider, source); ok {
					// An unclassified provider that maps a removal back to the
					// removed path has not produced importable exact work. A
					// distinct owning source (for example a database companion)
					// remains valid without requiring the deleted path to exist.
					if relevance == parser.ChangedPathUnclassified &&
						request.EventKind == "remove" &&
						filepath.Clean(file.Path) == request.Path {
						continue
					}
					attribution.files = append(attribution.files, file)
					exactSources++
				}
			}
			if exactSources == 0 {
				providerUnproven = true
			}
		}
		if providerUnproven {
			// Unclassified is uncertainty, not proof that the path is metadata.
			attribution.addFallback(agent)
		}
	}
	attribution.files = dedupeDiscoveredFiles(attribution.files)
	attribution.normalize()
	return attribution, claimed, nil
}

func (e *Engine) resolveClaudeDuplicateAttribution(
	ctx context.Context,
	plan *ChangedPathPlan,
) error {
	var files []parser.DiscoveredFile
	for _, attribution := range plan.attribution {
		files = append(files, attribution.files...)
	}
	expanded, err := e.expandAffectedClaudeDuplicateCandidates(ctx, files)
	if err != nil {
		return err
	}
	expanded = dedupeDiscoveredFiles(expanded)
	preferredFiles := e.dedupeClaudeDiscoveredFiles(expanded)
	preferredBySession := make(map[string]parser.DiscoveredFile)
	for _, file := range preferredFiles {
		if !isClaudeFormatTranscriptFile(file) {
			continue
		}
		sessionID := claudeSessionIDFromPath(file.Path)
		if sessionID == "" {
			continue
		}
		preferredBySession[claudeDiscoveredFileKey(file, sessionID)] = file
	}
	if len(preferredBySession) == 0 {
		return nil
	}
	for path, attribution := range plan.attribution {
		for i, file := range attribution.files {
			if !isClaudeFormatTranscriptFile(file) {
				continue
			}
			sessionID := claudeSessionIDFromPath(file.Path)
			key := claudeDiscoveredFileKey(file, sessionID)
			preferred, ok := preferredBySession[key]
			if ok {
				attribution.files[i] = preferred
			}
		}
		attribution.files = dedupeDiscoveredFiles(attribution.files)
		attribution.normalize()
		plan.attribution[path] = attribution
	}
	return nil
}

// expandAffectedClaudeDuplicateCandidates resolves the archive's stored source
// for each exact Claude-compatible session in the plan. The changed source and stored
// source are sufficient for DB-aware preference without invoking corpus-wide
// project or nested-subagent discovery.
func (e *Engine) expandAffectedClaudeDuplicateCandidates(
	ctx context.Context,
	files []parser.DiscoveredFile,
) ([]parser.DiscoveredFile, error) {
	providers := make(map[parser.AgentType]parser.Provider)
	for _, agent := range []parser.AgentType{parser.AgentClaude, parser.AgentIcodemate} {
		factory := e.providerFactories[agent]
		roots := e.agentDirs[agent]
		if factory == nil || len(roots) == 0 {
			continue
		}
		providers[agent] = factory.NewProvider(parser.ProviderConfig{
			Roots: roots, Machine: e.machine,
			SourceMachines: e.sourceMachines[agent],
			PathRewriter:   e.pathRewriter,
		})
	}
	if len(providers) == 0 {
		return files, nil
	}
	out := append([]parser.DiscoveredFile(nil), files...)
	seenSources := make(map[string]struct{}, len(files))
	seenSessions := make(map[string]struct{})
	for _, file := range files {
		seenSources[changedPathSourceKey(file)] = struct{}{}
	}
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !isClaudeFormatTranscriptFile(file) {
			continue
		}
		provider := providers[file.Agent]
		if provider == nil {
			continue
		}
		sessionID := claudeSessionIDFromPath(file.Path)
		if sessionID == "" {
			continue
		}
		idPrefix := e.idPrefix
		if isS3SourcePath(file.Path) {
			idPrefix = s3SessionIDPrefix(file.Machine)
		}
		fullID := applyIDPrefixToID(
			idPrefix, claudeFormatArchiveSessionID(file.Agent, sessionID),
		)
		sessionKey := discoveredFileIDPrefix(file) + "\x00" + fullID
		if _, seen := seenSessions[sessionKey]; seen {
			continue
		}
		seenSessions[sessionKey] = struct{}{}

		storedPath := e.db.GetSessionFilePath(fullID)
		if storedPath == "" {
			continue
		}
		physicalStoredPath := storedPath
		if e.pathRewriter != nil {
			if e.storedPathResolver == nil {
				continue
			}
			var resolved bool
			physicalStoredPath, resolved = e.storedPathResolver(storedPath)
			if !resolved {
				continue
			}
		}
		if physicalStoredPath == "" {
			continue
		}
		request := parser.FindSourceRequest{
			StoredFilePath:     physicalStoredPath,
			RequireFreshSource: true, PreferStoredSource: true,
		}
		source, found, findErr := provider.FindSource(ctx, request)
		if errors.Is(findErr, context.Canceled) ||
			errors.Is(findErr, context.DeadlineExceeded) {
			return nil, findErr
		}
		if findErr != nil || !found {
			continue
		}
		candidate, ok := e.changedPathDiscoveredFile(provider, source)
		if !ok {
			continue
		}
		key := changedPathSourceKey(candidate)
		if _, seen := seenSources[key]; seen {
			continue
		}
		seenSources[key] = struct{}{}
		out = append(out, candidate)
	}
	return out, nil
}

func (e *Engine) sortedAuthoritativeProviderAgents() []parser.AgentType {
	agents := make([]parser.AgentType, 0, len(e.providerFactories))
	for agent := range e.providerFactories {
		if e.providerMigrationModes[agent] == parser.ProviderMigrationProviderAuthoritative {
			agents = append(agents, agent)
		}
	}
	slices.SortFunc(agents, func(a, b parser.AgentType) int {
		return strings.Compare(string(a), string(b))
	})
	return agents
}

func matchingChangedPathRoots(path string, roots []string) []string {
	matching := make([]string, 0, len(roots))
	for _, root := range roots {
		if changedPathWithinRoot(path, root) {
			matching = append(matching, root)
		}
	}
	return matching
}

func (e *Engine) addPhysicalStoredSourceHints(
	ctx context.Context,
	provider parser.Provider,
	request *parser.ChangedPathRequest,
) (bool, error) {
	if provider.Capabilities().Source.StoredSourceHints != parser.CapabilitySupported {
		return true, nil
	}
	resolver, ok := provider.(parser.StoredSourceHintScopeProvider)
	if !ok {
		return false, nil
	}
	scopes := storedSourceDBHintScopes(resolver.StoredSourceHintScopes(*request))
	if len(scopes) == 0 {
		return true, nil
	}
	if e.pathRewriter != nil {
		for i := range scopes {
			scopes[i].Path = e.pathRewriter(scopes[i].Path)
		}
	}
	stored, err := e.db.ListStoredSourcePathHintsContext(
		ctx, string(provider.Definition().Type), scopes,
	)
	if err != nil {
		return false, err
	}
	physical := make([]string, 0, len(stored))
	for _, storedPath := range stored {
		if e.storedPathResolver == nil {
			return false, nil
		}
		path, translated := e.storedPathResolver(storedPath)
		if !translated {
			return false, nil
		}
		normalized, normalizeErr := normalizeChangedPhysicalPath(path)
		if normalizeErr != nil {
			return false, normalizeErr
		}
		physical = append(physical, normalized)
	}
	request.StoredSourcePaths = physical
	return true, nil
}

func (e *Engine) changedPathDiscoveredFile(
	provider parser.Provider,
	source parser.SourceRef,
) (parser.DiscoveredFile, bool) {
	path := providerDiscoveredPath(source)
	if path == "" {
		return parser.DiscoveredFile{}, false
	}
	agent := source.Provider
	if agent == "" {
		agent = provider.Definition().Type
	}
	sourceCopy := source
	file := parser.DiscoveredFile{
		Path: path, Project: source.ProjectHint, Agent: agent,
		ProviderSource: &sourceCopy, ProviderProcess: true,
	}
	if !isS3SourcePath(path) {
		file.Machine = e.machineForProviderSource(agent, source, path)
	}
	return file, true
}

func (a *changedPathAttribution) addFallback(agent parser.AgentType) {
	if !slices.Contains(a.fallbackProviders, agent) {
		a.fallbackProviders = append(a.fallbackProviders, agent)
	}
}

func (a *changedPathAttribution) normalize() {
	a.files = sortAndDedupeChangedPathFiles(a.files)
	slices.SortFunc(a.fallbackProviders, func(x, y parser.AgentType) int {
		return strings.Compare(string(x), string(y))
	})
}

func (plan *ChangedPathPlan) rebuildAggregates() {
	var files []parser.DiscoveredFile
	var providers []parser.AgentType
	for _, attribution := range plan.attribution {
		files = append(files, attribution.files...)
		providers = append(providers, attribution.fallbackProviders...)
	}
	plan.Files = dedupeDiscoveredFiles(sortAndDedupeChangedPathFiles(files))
	slices.SortFunc(providers, func(a, b parser.AgentType) int {
		return strings.Compare(string(a), string(b))
	})
	plan.FallbackProviders = slices.Compact(providers)
}

func sortAndDedupeChangedPathFiles(files []parser.DiscoveredFile) []parser.DiscoveredFile {
	merged := make(map[string]parser.DiscoveredFile, len(files))
	keys := make([]string, 0, len(files))
	for _, file := range files {
		key := changedPathSourceKey(file)
		if current, ok := merged[key]; ok {
			merged[key] = mergeChangedPathDiscoveredFile(current, file)
			continue
		}
		merged[key] = file
		keys = append(keys, key)
	}
	slices.Sort(keys)
	out := make([]parser.DiscoveredFile, 0, len(keys))
	for _, key := range keys {
		out = append(out, merged[key])
	}
	return out
}

func changedPathSourceKey(file parser.DiscoveredFile) string {
	return string(file.Agent) + "\x00" + file.Path
}

// PruneScope projects only armed input attribution into invalidation work.
func (plan ChangedPathPlan) PruneScope(
	armedPhysicalPaths map[string]struct{},
) ChangedPathPruneScope {
	armedPhysicalPaths = normalizeChangedPathSet(armedPhysicalPaths)
	var files []parser.DiscoveredFile
	var providers []parser.AgentType
	for path, attribution := range plan.attribution {
		if _, armed := armedPhysicalPaths[filepath.Clean(path)]; !armed {
			continue
		}
		files = append(files, attribution.files...)
		providers = append(providers, attribution.fallbackProviders...)
	}
	files = sortAndDedupeChangedPathFiles(files)
	slices.SortFunc(providers, func(a, b parser.AgentType) int {
		return strings.Compare(string(a), string(b))
	})
	return ChangedPathPruneScope{Files: files, FallbackProviders: slices.Compact(providers)}
}

// CountCachedSuppressedInputs maps cached source results back to distinct
// disarmed pending inputs without exposing their paths.
func (plan ChangedPathPlan) CountCachedSuppressedInputs(
	armedPhysicalPaths map[string]struct{},
	cachedSourceKeys map[string]struct{},
	cachedFallbackProviders map[parser.AgentType]int,
) int {
	armedPhysicalPaths = normalizeChangedPathSet(armedPhysicalPaths)
	count := 0
	paths := make([]string, 0, len(plan.attribution))
	for path := range plan.attribution {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	for _, path := range paths {
		if _, armed := armedPhysicalPaths[path]; armed {
			continue
		}
		attribution := plan.attribution[path]
		suppressed := false
		for _, file := range attribution.files {
			if _, ok := cachedSourceKeys[changedPathSourceKey(file)]; ok {
				suppressed = true
				break
			}
		}
		if !suppressed {
			for _, agent := range attribution.fallbackProviders {
				if cachedFallbackProviders[agent] > 0 {
					suppressed = true
					break
				}
			}
		}
		if suppressed {
			count++
		}
	}
	return count
}

func normalizeChangedPathSet(paths map[string]struct{}) map[string]struct{} {
	normalized := make(map[string]struct{}, len(paths))
	for path := range paths {
		if clean, err := normalizeChangedPhysicalPath(path); err == nil {
			normalized[clean] = struct{}{}
		}
	}
	return normalized
}

func (e *Engine) discoverChangedPathFallbackProviders(
	ctx context.Context,
	agents []parser.AgentType,
) ([]parser.DiscoveredFile, map[parser.AgentType]int, error) {
	selected := append([]parser.AgentType(nil), agents...)
	slices.SortFunc(selected, func(a, b parser.AgentType) int {
		return strings.Compare(string(a), string(b))
	})
	selected = slices.Compact(selected)
	var files []parser.DiscoveredFile
	counts := make(map[parser.AgentType]int, len(selected))
	for _, agent := range selected {
		if e.providerMigrationModes[agent] != parser.ProviderMigrationProviderAuthoritative {
			return nil, nil, fmt.Errorf("fallback provider %s is not authoritative", agent)
		}
		factory := e.providerFactories[agent]
		roots := e.agentDirs[agent]
		if factory == nil || len(roots) == 0 {
			return nil, nil, fmt.Errorf("fallback provider %s is not configured", agent)
		}
		provider := factory.NewProvider(parser.ProviderConfig{
			Roots: roots, Machine: e.machine, PathRewriter: e.pathRewriter,
			SourceMachines: e.sourceMachines[agent],
		})
		sources, err := provider.Discover(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("discover fallback provider %s: %w", agent, err)
		}
		var providerFiles []parser.DiscoveredFile
		for _, source := range sources {
			if file, ok := e.changedPathDiscoveredFile(provider, source); ok {
				providerFiles = append(providerFiles, file)
			}
		}
		providerFiles = sortAndDedupeChangedPathFiles(providerFiles)
		providerFiles = e.dedupeClaudeDiscoveredFiles(providerFiles)
		providerFiles = sortAndDedupeChangedPathFiles(providerFiles)
		counts[agent] = len(providerFiles)
		files = append(files, providerFiles...)
	}
	return sortAndDedupeChangedPathFiles(files), counts, nil
}
