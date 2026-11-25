package sync

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

func dedupeDiscoveredFiles(
	files []parser.DiscoveredFile,
) []parser.DiscoveredFile {
	return dedupeDiscoveredFilesByPreference(files, preferDiscoveredFile)
}

func dedupeDiscoveredFilesByPreference(
	files []parser.DiscoveredFile,
	prefer func(candidate, current parser.DiscoveredFile) bool,
) []parser.DiscoveredFile {
	if len(files) < 2 {
		return files
	}

	bestByKey := make(map[string]parser.DiscoveredFile, len(files))
	for _, file := range files {
		key := discoveredFileKey(file)
		if current, ok := bestByKey[key]; ok {
			if prefer(file, current) {
				bestByKey[key] = file
			}
			continue
		}
		bestByKey[key] = file
	}

	out := make([]parser.DiscoveredFile, 0, len(bestByKey))
	for _, file := range files {
		key := discoveredFileKey(file)
		chosen, ok := bestByKey[key]
		if !ok || chosen.Path != file.Path || chosen.Agent != file.Agent {
			continue
		}
		out = append(out, file)
		delete(bestByKey, key)
	}
	return out
}

func discoveredFileKey(file parser.DiscoveredFile) string {
	if isCodexFormatAgent(file.Agent) {
		if id := parser.CodexSessionUUIDFromFilename(filepath.Base(file.Path)); id != "" {
			return string(file.Agent) + "\x00" +
				discoveredFileIDPrefix(file) + "\x00" + id
		}
	}
	return string(file.Agent) + "\x00" + file.Path
}

func discoveredFileIDPrefix(file parser.DiscoveredFile) string {
	if isS3SourcePath(file.Path) {
		return s3SessionIDPrefix(file.Machine)
	}
	return ""
}

func preferDiscoveredFile(
	candidate, current parser.DiscoveredFile,
) bool {
	if candidate.Agent == current.Agent && isCodexFormatAgent(candidate.Agent) {
		// Keep import ranking consistent with parser.preferCodexSource.
		// Across roots this is a selection policy, not evidence of freshness.
		candLayout := codexLayoutForPath(candidate.Path)
		currLayout := codexLayoutForPath(current.Path)
		if candLayout != currLayout {
			return candLayout == parser.CodexLayoutDated
		}
		candCompressed := strings.HasSuffix(candidate.Path, ".jsonl.zst")
		currCompressed := strings.HasSuffix(current.Path, ".jsonl.zst")
		if candCompressed != currCompressed {
			return !candCompressed
		}
		return candidate.Path < current.Path
	}
	return false
}

func discoveredFileMTime(path string) (int64, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	return info.ModTime().UnixNano(), true
}

// slowProviderDiscoveryThreshold is the per-provider discovery duration above
// which discovery timing is logged. Most providers finish in well under a
// millisecond; a provider over this bound is doing real per-source work worth
// surfacing.
const slowProviderDiscoveryThreshold = 100 * time.Millisecond

// discoverProviderSources runs full-sync discovery through the provider facade
// for every concrete provider that is authoritative. It is the sole on-disk
// discovery path: every file-based agent owns discovery through its provider.
func (e *Engine) discoverProviderSources(
	ctx context.Context,
	scope *rootSyncScope,
	preContainerStates map[string]parser.SQLiteContainerState,
) ([]parser.DiscoveredFile, int) {
	var files []parser.DiscoveredFile
	var failures int
	containerListsWatermarkOnly := e.sqliteContainerListsWatermarkOnly(
		preContainerStates,
	)

	agents := make([]parser.AgentType, 0, len(e.sources().providerFactories))
	for agent := range e.sources().providerFactories {
		agents = append(agents, agent)
	}
	slices.SortFunc(agents, func(a, b parser.AgentType) int {
		return strings.Compare(string(a), string(b))
	})

	for _, agentType := range agents {
		mode := e.providerMigrationModes[agentType]
		if mode != parser.ProviderMigrationProviderAuthoritative {
			continue
		}
		if !scope.matchesAgent(agentType) {
			continue
		}
		roots := e.sources().agentDirs[agentType]
		if len(roots) == 0 {
			continue
		}
		filteredRoots := make([]string, 0, len(roots))
		for _, root := range roots {
			if scope.includes(root) {
				filteredRoots = append(filteredRoots, root)
			}
		}
		if len(filteredRoots) == 0 {
			continue
		}
		factory, ok := e.sources().providerFactories[agentType]
		if !ok || factory == nil {
			continue
		}
		providerRoots := filteredRoots
		if agentType == parser.AgentKiro {
			// Kiro must arbitrate against out-of-scope roots before scoped
			// admission, otherwise a lower-ranked in-scope copy is imported.
			providerRoots = roots
		}
		provider := factory.NewProvider(parser.ProviderConfig{
			Roots:                             providerRoots,
			Machine:                           e.machine,
			SourceMachines:                    e.sources().sourceMachines[agentType],
			PathRewriter:                      e.pathRewriter,
			SQLiteContainerListsWatermarkOnly: containerListsWatermarkOnly,
		})
		// Shared-database providers are streamed source-by-source by their
		// dedicated sync phase. Calling Discover here would build an archive-sized
		// slice only to discard it, then traverse the same database again.
		if processFileUsesProvider(agentType) {
			continue
		}
		tDiscover := time.Now()
		sources, err := provider.Discover(ctx)
		// Log only providers whose discovery is slow enough to matter, so a
		// single pathological provider (e.g. a per-source map rebuild) stands
		// out instead of hiding inside the aggregate discovery timing.
		if d := time.Since(tDiscover); d >= slowProviderDiscoveryThreshold {
			log.Printf(
				"discovery: %s returned %d sources in %s",
				agentType, len(sources), d.Round(time.Millisecond),
			)
		}
		if err != nil {
			log.Printf("%s provider discovery: %v", agentType, err)
			failures++
			_, incomplete := errors.AsType[parser.DiscoveryIncompleteError](err)
			if !incomplete || ctx.Err() != nil || !provider.Capabilities().Sync.PartialDiscovery {
				continue
			}
		}
		if agentType == parser.AgentKiro {
			sources = filterProviderSourcesToScope(sources, scope)
		}
		forceParseSource := func(string) bool { return false }
		if agentType == parser.AgentVSCopilot {
			// Only Visual Studio Copilot recovery consumes the discovered
			// path set; building it for every provider would allocate an
			// archive-sized map per full discovery.
			currentSources := providerSourcePathSet(sources)
			missingSources, forceParseSources := e.visualStudioCopilotMissingVS2026PollSources(
				ctx, provider, filteredRoots, currentSources,
			)
			sources = append(sources, missingSources...)
			forceParseSource = func(sourcePath string) bool {
				_, ok := forceParseSources[filepath.Clean(sourcePath)]
				return ok
			}
		}
		def := provider.Definition()
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
			if !isS3SourcePath(sourcePath) {
				discovered.Machine = e.machineForProviderSource(
					agent, source, sourcePath,
				)
			}
			if forceParseSource(sourcePath) {
				discovered.ForceParse = true
			}
			// S3-aware source sets carry the durable object metadata in the
			// Opaque payload. Thread it into the DiscoveredFile so the S3 sync
			// path (object fetch, fingerprinting, machine-ID namespacing) and the
			// freshness/dedup/mtime-cutoff logic see the same source identity the
			// legacy s3:// discovery emitted directly. Providers read local files,
			// so clear ProviderProcess for s3:// objects: processProviderFile must
			// decline them so they route through processS3Session rather than the
			// provider Fingerprint/Parse path, which cannot read a remote object.
			if s3, ok := source.Opaque.(parser.S3DiscoveredSource); ok {
				discovered.Machine = s3.Machine
				discovered.SourceSize = s3.Size
				discovered.SourceMtime = s3.MtimeNS
				discovered.SourceFingerprint = s3.Fingerprint
				discovered.TranscriptSize = s3.TranscriptSize
				discovered.TranscriptMtime = s3.TranscriptMtimeNS
				discovered.ProviderProcess = false
				if discovered.Project == "" {
					discovered.Project = s3.Project
				}
			}
			files = append(files, discovered)
		}
	}
	return files, failures
}

func filterProviderSourcesToScope(
	sources []parser.SourceRef,
	scope *rootSyncScope,
) []parser.SourceRef {
	filtered := sources[:0]
	for _, source := range sources {
		// Overlapping roots can attribute a winner to an out-of-scope
		// ancestor; a physically in-scope source stays admitted.
		if scope.includes(source.ConfiguredRoot) ||
			scope.includes(validatedProviderSourceStatPath(
				providerDiscoveredPath(source),
			)) {
			filtered = append(filtered, source)
		}
	}
	return filtered
}

func providerSourcePathSet(sources []parser.SourceRef) map[string]struct{} {
	seen := make(map[string]struct{}, len(sources))
	for _, source := range sources {
		path := providerDiscoveredPath(source)
		if path == "" {
			continue
		}
		seen[filepath.Clean(path)] = struct{}{}
	}
	return seen
}

func (e *Engine) visualStudioCopilotMissingVS2026PollSources(
	ctx context.Context,
	provider parser.Provider,
	roots []string,
	currentSources map[string]struct{},
) ([]parser.SourceRef, map[string]struct{}) {
	var out []parser.SourceRef
	seenHints := make(map[string]struct{})
	forceParseSources := make(map[string]struct{})
	watchRoots, err := e.providerChangedPathWatchRoots(
		ctx, parser.AgentVSCopilot, provider, roots,
	)
	if err != nil {
		log.Printf("%s provider poll watch roots: %v", parser.AgentVSCopilot, err)
		return out, forceParseSources
	}
	for _, watchRoot := range watchRoots {
		hints, err := e.db.ListStoredSourcePathHintsContext(ctx,
			string(parser.AgentVSCopilot), []db.StoredSourcePathHintScope{{Path: watchRoot}},
		)
		if err != nil {
			log.Printf(
				"%s provider poll stored hints: %v",
				parser.AgentVSCopilot, err,
			)
			continue
		}
		for _, hint := range hints {
			hint = filepath.Clean(hint)
			if _, seen := seenHints[hint]; seen {
				continue
			}
			seenHints[hint] = struct{}{}
			container, conversationID, ok := parser.SplitVisualStudioCopilotVirtualPath(hint)
			if !ok ||
				!parser.IsVisualStudioCopilotVS2026SessionPath(container) {
				continue
			}
			if _, ok := currentSources[hint]; ok {
				continue
			}
			if current, ok := e.visualStudioCopilotCurrentPollSource(
				ctx, provider, conversationID,
			); ok {
				sourcePath := providerDiscoveredPath(current)
				if sourcePath == "" {
					continue
				}
				path := filepath.Clean(sourcePath)
				forceParseSources[path] = struct{}{}
				if _, exists := currentSources[path]; !exists {
					currentSources[path] = struct{}{}
					out = append(out, current)
				}
				continue
			}
			if !visualStudioCopilotVS2026PollCanTombstone(
				roots, container,
			) {
				continue
			}
			tombstones, err := provider.SourcesForChangedPath(
				ctx,
				parser.ChangedPathRequest{
					Path:              hint,
					EventKind:         "remove",
					WatchRoot:         watchRoot,
					StoredSourcePaths: []string{hint},
				},
			)
			if err != nil {
				log.Printf(
					"%s provider poll tombstone: %v",
					parser.AgentVSCopilot, err,
				)
				continue
			}
			for _, tombstone := range tombstones {
				sourcePath := providerDiscoveredPath(tombstone)
				if sourcePath == "" {
					continue
				}
				path := filepath.Clean(sourcePath)
				if _, exists := currentSources[path]; exists {
					continue
				}
				currentSources[path] = struct{}{}
				forceParseSources[path] = struct{}{}
				out = append(out, tombstone)
			}
		}
	}
	return out, forceParseSources
}

func visualStudioCopilotVS2026PollCanTombstone(
	roots []string,
	container string,
) bool {
	if container == "" {
		return false
	}
	container = filepath.Clean(container)
	if parser.IsRegularFile(container) {
		return false
	}
	if !reachableDir(filepath.Dir(container)) {
		return false
	}
	return slices.ContainsFunc(roots, func(root string) bool {
		root = filepath.Clean(root)
		return samePathOrDescendant(container, root) && reachableDir(root)
	})
}

func reachableDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info != nil && info.IsDir()
}

func (e *Engine) visualStudioCopilotCurrentPollSource(
	ctx context.Context,
	provider parser.Provider,
	conversationID string,
) (parser.SourceRef, bool) {
	current, ok, err := provider.FindSource(
		ctx,
		parser.FindSourceRequest{
			RawSessionID:       conversationID,
			RequireFreshSource: true,
		},
	)
	if err != nil {
		log.Printf(
			"%s provider poll source lookup: %v",
			parser.AgentVSCopilot, err,
		)
		return parser.SourceRef{}, false
	}
	return current, ok
}
