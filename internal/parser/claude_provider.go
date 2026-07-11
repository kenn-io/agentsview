package parser

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var _ Provider = (*claudeProvider)(nil)

type claudeProviderFactory struct {
	def AgentDef
}

func newClaudeProviderFactory(def AgentDef) ProviderFactory {
	return claudeProviderFactory{def: cloneAgentDef(def)}
}

func (f claudeProviderFactory) Definition() AgentDef {
	return cloneAgentDef(f.def)
}

func (f claudeProviderFactory) Capabilities() Capabilities {
	return claudeProviderCapabilities()
}

func (f claudeProviderFactory) NewProvider(cfg ProviderConfig) Provider {
	cfg = cfg.Clone()
	return &claudeProvider{
		ProviderBase: ProviderBase{
			Def:    cloneAgentDef(f.def),
			Caps:   claudeProviderCapabilities(),
			Config: cfg,
		},
		sources: newClaudeSourceSet(cfg.Roots),
	}
}

type claudeProvider struct {
	ProviderBase
	sources claudeSourceSet
}

func (p *claudeProvider) Discover(ctx context.Context) ([]SourceRef, error) {
	return p.sources.Discover(ctx)
}

func (p *claudeProvider) WatchPlan(ctx context.Context) (WatchPlan, error) {
	return p.sources.WatchPlan(ctx)
}

func (p *claudeProvider) SourcesForChangedPath(
	ctx context.Context,
	req ChangedPathRequest,
) ([]SourceRef, error) {
	return p.sources.SourcesForChangedPath(ctx, req)
}

func (p *claudeProvider) FindSource(
	ctx context.Context,
	req FindSourceRequest,
) (SourceRef, bool, error) {
	req = ProviderFindRequestWithRawSessionID(p.Def, req)
	return p.sources.FindSource(ctx, req)
}

func (p *claudeProvider) Fingerprint(
	ctx context.Context,
	source SourceRef,
) (SourceFingerprint, error) {
	return p.sources.Fingerprint(ctx, source)
}

func (p *claudeProvider) Parse(
	ctx context.Context,
	req ParseRequest,
) (ParseOutcome, error) {
	if err := ctx.Err(); err != nil {
		return ParseOutcome{}, err
	}
	path, ok := p.sources.pathFromSource(req.Source)
	if !ok {
		return ParseOutcome{}, fmt.Errorf("claude source path unavailable")
	}
	machine := firstNonEmptyJSONLString(req.Machine, p.Config.Machine)
	project := claudeProviderProject(ctx, req.Source.ProjectHint, path)
	results, excludedIDs, err := claudeParseWithExclusions(path, project, machine)
	if err != nil {
		return ParseOutcome{}, err
	}
	if req.Fingerprint.Hash != "" {
		for i := range results {
			results[i].Session.File.Hash = req.Fingerprint.Hash
		}
	}
	InferRelationshipTypes(results)
	out := make([]ParseResultOutcome, 0, len(results))
	for _, result := range results {
		out = append(out, ParseResultOutcome{
			Result:      result,
			DataVersion: DataVersionCurrent,
		})
	}
	return ParseOutcome{
		Results:            out,
		ExcludedSessionIDs: excludedIDs,
		ResultSetComplete:  true,
	}, nil
}

// ClaudeUploadParser is implemented by the Claude provider to parse a
// standalone, out-of-root Claude transcript file (such as an HTTP upload)
// under a caller-supplied project name. Uploads do not live under a
// configured root, so the normal discovery/source-resolution path does not
// apply; callers obtain this via NewProvider(AgentClaude, ...) and a type
// assertion.
type ClaudeUploadParser interface {
	// ParseUploadedTranscript parses the transcript at path and files the
	// resulting sessions under project. The project is authoritative: unlike
	// the discovered-session Parse path, it is not overridden by any cwd
	// recorded in the transcript, because an upload is filed under a
	// user-chosen project rather than a workspace path on this machine.
	ParseUploadedTranscript(path, project, machine string) ([]ParseResult, error)
}

func (p *claudeProvider) ParseUploadedTranscript(
	path, project, machine string,
) ([]ParseResult, error) {
	machine = firstNonEmptyJSONLString(machine, p.Config.Machine)
	results, _, err := claudeParseWithExclusions(path, project, machine)
	if err != nil {
		return nil, err
	}
	InferRelationshipTypes(results)
	return results, nil
}

func (p *claudeProvider) ParseIncremental(
	ctx context.Context,
	req IncrementalRequest,
) (IncrementalOutcome, IncrementalStatus, error) {
	if err := ctx.Err(); err != nil {
		return IncrementalOutcome{}, IncrementalUnsupported, err
	}
	path, ok := p.sources.pathFromSource(req.Source)
	if !ok {
		return IncrementalOutcome{}, IncrementalUnsupported,
			fmt.Errorf("claude source path unavailable")
	}
	if req.Offset > 0 && req.Fingerprint.Size < req.Offset {
		return IncrementalOutcome{ForceReplace: true},
			IncrementalNeedsFullParse, nil
	}
	if req.Fingerprint.Size == req.Offset {
		return IncrementalOutcome{}, IncrementalNoNewData, nil
	}
	newMsgs, links, endedAt, consumed, err := claudeParseSessionFrom(
		path,
		req.Offset,
		req.StartOrdinal,
		req.LastEntryUUID,
	)
	if err != nil {
		if IsIncrementalFullParseFallback(err) || errorsIsClaudeDAG(err) {
			return IncrementalOutcome{ForceReplace: IsIncrementalFullParseFallback(err)},
				IncrementalNeedsFullParse, nil
		}
		return IncrementalOutcome{}, IncrementalNeedsFullParse, err
	}
	if len(newMsgs) == 0 {
		if consumed > 0 {
			return IncrementalOutcome{
				SessionID:     req.SessionID,
				SubagentLinks: links,
				EndedAt:       endedAt,
				ConsumedBytes: consumed,
			}, IncrementalApplied, nil
		}
		return IncrementalOutcome{}, IncrementalNoNewData, nil
	}
	totalOut, peakCtx, hasTotalOut, hasPeakCtx := claudeProviderTokenTotals(newMsgs)
	return IncrementalOutcome{
		SessionID:            req.SessionID,
		Messages:             newMsgs,
		SubagentLinks:        links,
		EndedAt:              endedAt,
		ConsumedBytes:        consumed,
		MessageCount:         len(newMsgs),
		UserMessageCount:     claudeProviderUserMessageCount(newMsgs),
		TotalOutputTokens:    totalOut,
		PeakContextTokens:    peakCtx,
		HasTotalOutputTokens: hasTotalOut,
		HasPeakContextTokens: hasPeakCtx,
	}, IncrementalApplied, nil
}

type claudeSource struct {
	Root string
	Path string
}

type claudeSourceSet struct {
	roots []string
}

func newClaudeSourceSet(roots []string) claudeSourceSet {
	return claudeSourceSet{roots: cleanJSONLRoots(roots)}
}

func (s claudeSourceSet) Discover(ctx context.Context) ([]SourceRef, error) {
	var sources []SourceRef
	seen := make(map[string]struct{})
	for _, root := range s.roots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for _, file := range ClaudeProjectSessionFiles(root) {
			source, ok := s.discoveredSourceRef(root, file)
			if !ok {
				continue
			}
			addJSONLSource(source, &sources, seen)
		}
	}
	sortJSONLSources(sources)
	return sources, nil
}

// discoveredSourceRef builds the SourceRef for one enumerated Claude session
// file. Local files resolve through the regular file-backed source ref; s3://
// objects (which ClaudeProjectSessionFiles enumerates via discoverClaudeS3)
// carry their durable object metadata in the Opaque payload, because the
// IsRegularFile gate that sourceRef applies to a local path would otherwise drop
// every remote object.
func (s claudeSourceSet) discoveredSourceRef(
	root string, file DiscoveredFile,
) (SourceRef, bool) {
	if strings.HasPrefix(file.Path, "s3://") {
		return s3SourceRefFromDiscoveredFile(file), true
	}
	return s.sourceRef(root, file.Path)
}

func (s claudeSourceSet) WatchPlan(context.Context) (WatchPlan, error) {
	roots := make([]WatchRoot, 0, len(s.roots))
	for _, root := range s.roots {
		roots = append(roots, WatchRoot{
			Path:         root,
			Recursive:    true,
			IncludeGlobs: []string{"*.jsonl"},
			DebounceKey:  string(AgentClaude) + ":projects:" + root,
		})
	}
	return WatchPlan{Roots: roots}, nil
}

func (s claudeSourceSet) SourcesForChangedPath(
	ctx context.Context,
	req ChangedPathRequest,
) ([]SourceRef, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// The legacy classifier resolved Claude paths purely from their
	// project/session shape and only treated a stat failure as
	// "missing" when it was a definitive IsNotExist. A transient stat
	// error (for example a parent directory the watcher cannot read this
	// instant) must still classify so the change is not silently dropped.
	// Fall back to path-shape classification whenever the path is not
	// known to be absent.
	allowMissing := jsonlMissingPathFallbackAllowed(req) ||
		claudeChangedPathPresentButUnstatable(req.Path)
	if req.WatchRoot != "" {
		root := filepath.Clean(req.WatchRoot)
		if !s.hasRoot(root) {
			return nil, nil
		}
		source, ok := s.sourceForChangedPath(root, req.Path, allowMissing)
		if !ok {
			return nil, nil
		}
		return []SourceRef{source}, nil
	}
	for _, root := range s.roots {
		source, ok := s.sourceForChangedPath(root, req.Path, allowMissing)
		if ok {
			return []SourceRef{source}, nil
		}
	}
	return nil, nil
}

func (s claudeSourceSet) FindSource(
	ctx context.Context,
	req FindSourceRequest,
) (SourceRef, bool, error) {
	if err := ctx.Err(); err != nil {
		return SourceRef{}, false, err
	}
	for _, path := range []string{req.StoredFilePath, req.FingerprintKey} {
		if path == "" {
			continue
		}
		for _, root := range s.roots {
			if source, ok := s.sourceForPath(root, path); ok {
				return source, true, nil
			}
		}
	}
	if req.RawSessionID == "" {
		return SourceRef{}, false, nil
	}
	for _, root := range s.roots {
		path := claudeFindSourceFile(root, req.RawSessionID)
		if path == "" {
			continue
		}
		if source, ok := s.sourceRef(root, path); ok {
			return source, true, nil
		}
	}
	return SourceRef{}, false, nil
}

func (s claudeSourceSet) Fingerprint(
	ctx context.Context,
	source SourceRef,
) (SourceFingerprint, error) {
	if err := ctx.Err(); err != nil {
		return SourceFingerprint{}, err
	}
	path, ok := s.pathFromSource(source)
	if !ok {
		return SourceFingerprint{}, fmt.Errorf("claude source path unavailable")
	}
	info, err := os.Stat(path)
	if err != nil {
		return SourceFingerprint{}, fmt.Errorf("stat %s: %w", path, err)
	}
	if info.IsDir() {
		return SourceFingerprint{}, fmt.Errorf("stat %s: source is a directory", path)
	}
	hash, err := hashJSONLSourceFile(path)
	if err != nil {
		return SourceFingerprint{}, err
	}
	inode, device := sourceFileIdentity(info)
	return SourceFingerprint{
		Key:     firstNonEmptyJSONLString(source.FingerprintKey, source.Key, path),
		Size:    info.Size(),
		MTimeNS: info.ModTime().UnixNano(),
		Inode:   inode,
		Device:  device,
		Hash:    hash,
	}, nil
}

func (s claudeSourceSet) pathFromSource(source SourceRef) (string, bool) {
	switch src := source.Opaque.(type) {
	case claudeSource:
		return src.Path, src.Path != ""
	case *claudeSource:
		if src != nil && src.Path != "" {
			return src.Path, true
		}
	case MaterializedFileSource:
		return src.Path, src.Path != ""
	}
	for _, candidate := range []string{
		source.DisplayPath,
		source.FingerprintKey,
		source.Key,
	} {
		for _, root := range s.roots {
			if ref, ok := s.sourceForPath(root, candidate); ok {
				src := ref.Opaque.(claudeSource)
				return src.Path, true
			}
		}
	}
	return "", false
}

func (s claudeSourceSet) sourceForPath(root, path string) (SourceRef, bool) {
	return s.sourceForChangedPath(root, path, false)
}

func (s claudeSourceSet) sourceForChangedPath(
	root,
	path string,
	allowMissing bool,
) (SourceRef, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	if allowMissing {
		return s.sourceRefFromPath(root, path)
	}
	return s.sourceRef(root, path)
}

func (s claudeSourceSet) sourceRef(root, path string) (SourceRef, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	if !IsRegularFile(path) {
		return SourceRef{}, false
	}
	return s.sourceRefFromPath(root, path)
}

func (s claudeSourceSet) sourceRefFromPath(root, path string) (SourceRef, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	project, ok := claudeProjectHintFromPath(root, path)
	if !ok {
		return SourceRef{}, false
	}
	return SourceRef{
		Provider:       AgentClaude,
		Key:            path,
		DisplayPath:    path,
		FingerprintKey: path,
		ProjectHint:    project,
		Opaque: claudeSource{
			Root: root,
			Path: path,
		},
	}, true
}

func (s claudeSourceSet) hasRoot(root string) bool {
	for _, configured := range s.roots {
		if samePath(root, configured) {
			return true
		}
	}
	return false
}

// claudeChangedPathPresentButUnstatable reports whether a changed path
// resolves to something on disk that cannot be stat'd right now for a
// reason other than not existing (for example a parent directory with no
// read/exec permission). In that case the legacy classifier still
// recognized the path by shape, so the provider must classify it too.
func claudeChangedPathPresentButUnstatable(path string) bool {
	if path == "" {
		return false
	}
	if IsRegularFile(path) {
		return false
	}
	_, err := os.Lstat(path)
	if err == nil {
		// Present (lstat succeeded) but not a regular file via Stat,
		// e.g. stat blocked by parent-directory permissions.
		return true
	}
	return !os.IsNotExist(err)
}

func claudeProjectHintFromPath(root, path string) (string, bool) {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil || rel == "." || rel == "" {
		return "", false
	}
	if strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." {
		return "", false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) == 2 && strings.HasSuffix(parts[1], ".jsonl") {
		stem := strings.TrimSuffix(parts[1], ".jsonl")
		if strings.HasPrefix(stem, "agent-") {
			return "", false
		}
		return parts[0], true
	}
	if len(parts) >= 4 && parts[2] == "subagents" &&
		strings.HasSuffix(parts[len(parts)-1], ".jsonl") {
		stem := strings.TrimSuffix(parts[len(parts)-1], ".jsonl")
		if strings.HasPrefix(stem, "agent-") {
			return parts[0], true
		}
	}
	return "", false
}

func claudeProviderProject(ctx context.Context, projectHint, path string) string {
	project := GetProjectName(projectHint)
	cwd, gitBranch := ExtractClaudeProjectHints(path)
	if cwd != "" {
		if p := ExtractProjectFromCwdWithBranchContext(ctx, cwd, gitBranch); p != "" {
			project = p
		}
	}
	return project
}

func errorsIsClaudeDAG(err error) bool {
	return err == ErrDAGDetected
}

func claudeProviderUserMessageCount(msgs []ParsedMessage) int {
	count := 0
	for _, msg := range msgs {
		if msg.Role == RoleUser && !msg.IsSystem && len(msg.ToolResults) == 0 {
			count++
		}
	}
	return count
}

func claudeProviderTokenTotals(
	msgs []ParsedMessage,
) (totalOut int, peakCtx int, hasTotalOut bool, hasPeakCtx bool) {
	for _, msg := range msgs {
		msgHasCtx, msgHasOut := msg.TokenPresence()
		if msgHasOut {
			totalOut += msg.OutputTokens
			hasTotalOut = true
		}
		if msgHasCtx && (!hasPeakCtx || msg.ContextTokens > peakCtx) {
			peakCtx = msg.ContextTokens
			hasPeakCtx = true
		}
	}
	return totalOut, peakCtx, hasTotalOut, hasPeakCtx
}

func claudeProviderCapabilities() Capabilities {
	return Capabilities{
		Source: SourceCapabilities{
			DiscoverSources:      CapabilitySupported,
			WatchSources:         CapabilitySupported,
			ClassifyChangedPath:  CapabilitySupported,
			FindSource:           CapabilitySupported,
			CompositeFingerprint: CapabilitySupported,
			IncrementalAppend:    CapabilitySupported,
			MultiSessionSource:   CapabilitySupported,
			PerSessionErrors:     CapabilityNotApplicable,
			ExcludedSessions:     CapabilitySupported,
			ForceReplaceOnParse:  CapabilitySupported,
			VerifiedLocalStat:    CapabilitySupported,
		},
		Content: ContentCapabilities{
			FirstMessage:         CapabilitySupported,
			SessionName:          CapabilitySupported,
			Cwd:                  CapabilitySupported,
			GitBranch:            CapabilitySupported,
			Relationships:        CapabilitySupported,
			Subagents:            CapabilitySupported,
			Thinking:             CapabilitySupported,
			ToolCalls:            CapabilitySupported,
			ToolResults:          CapabilitySupported,
			PerMessageTokenUsage: CapabilitySupported,
			TerminationStatus:    CapabilitySupported,
			MalformedLineCount:   CapabilitySupported,
			Model:                CapabilitySupported,
			StopReason:           CapabilitySupported,
		},
	}
}
