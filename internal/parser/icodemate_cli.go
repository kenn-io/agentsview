package parser

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Icodemate exposes two storage families under the single icodemate agent:
// the VSCode-extension plugin path (OpenCode storage on
// ~/.local/share/icodemate, owned by the OpenCode-format source set) and the
// terminal CLI path (~/.icodemate/cli/projects), whose per-session JSONL
// transcripts match Claude Code's project layout
// (<root>/<project>/<session>.jsonl). The CLI source set below enumerates and
// parses that Claude-format layout independently, then relabels every parsed
// session onto Icodemate's own agent and icodemate: ID prefix, mirroring how
// the OpenCode-format path relabels its sessions. Discovery, watch, changed
// path, find, fingerprint, and parsing all mirror claudeSourceSet, with the
// Icodemate agent label substituted throughout so the two families collide
// only where the format genuinely differs.

var _ SourceSet = (*icodemateCLISourceSet)(nil)

type icodemateCLISource struct {
	Root string
	Path string
}

type icodemateCLISourceSet struct {
	roots []string
}

func newIcodemateCLISourceSet(roots []string) *icodemateCLISourceSet {
	return &icodemateCLISourceSet{roots: cleanJSONLRoots(roots)}
}

func (s *icodemateCLISourceSet) Discover(ctx context.Context) ([]SourceRef, error) {
	var sources []SourceRef
	seen := make(map[string]struct{})
	for _, root := range s.roots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for _, file := range IcodemateCLIProjectSessionFiles(root) {
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

// DiscoverEach implements StreamingDiscoverer.
func (s *icodemateCLISourceSet) DiscoverEach(
	ctx context.Context, yield func(SourceRef) error,
) error {
	for _, root := range s.roots {
		if err := ctx.Err(); err != nil {
			return err
		}
		if strings.HasPrefix(root, "s3://") {
			for _, file := range IcodemateCLIProjectSessionFiles(root) {
				source, ok := s.discoveredSourceRef(root, file)
				if ok {
					if err := yield(source); err != nil {
						return err
					}
				}
			}
			continue
		}
		if err := s.streamLocalRoot(ctx, root, yield); err != nil {
			return err
		}
	}
	return nil
}

// streamLocalRoot enumerates one local IcodeMate CLI projects root, following
// symlinked project directories exactly as Discover's walk does so a symlink
// whose target cannot be resolved surfaces as an incomplete discovery instead
// of reading as absent (reconciliation would otherwise tombstone every session
// beneath the symlink).
func (s *icodemateCLISourceSet) streamLocalRoot(
	ctx context.Context, root string, yield func(SourceRef) error,
) error {
	var incomplete error
	err := streamDirectoryEntries(ctx, root, func(project os.DirEntry) error {
		isProjectDir, dirErr := streamingDirCandidateOrIncomplete(
			AgentIcodemate, "IcodeMate CLI project directory", project, root,
		)
		if dirErr != nil {
			incomplete = errors.Join(incomplete, dirErr)
			return nil
		}
		if !isProjectDir {
			return nil
		}
		projectRoot := filepath.Join(root, project.Name())
		err := streamDirectoryTreeRecursive(ctx, projectRoot, func(
			path string, entry os.DirEntry,
		) error {
			if !strings.HasSuffix(entry.Name(), ".jsonl") {
				return nil
			}
			source, ok := s.sourceRef(root, path)
			if !ok {
				return nil
			}
			return yield(source)
		})
		if err == nil {
			return nil
		}
		if _, ok := discoveryYieldCause(err); ok {
			return err
		}
		if ctx.Err() != nil {
			return err
		}
		incomplete = errors.Join(incomplete, err)
		return nil
	})
	if cause, ok := discoveryYieldCause(err); ok {
		return cause
	}
	return errors.Join(incomplete, err)
}

// discoveredSourceRef builds the SourceRef for one enumerated session file.
// Local files resolve through the regular file-backed source ref; s3://
// objects (enumerated by IcodemateCLIProjectSessionFiles) carry their durable
// object metadata in the Opaque payload, because the IsRegularFile gate
// sourceRef applies to a local path would otherwise drop every remote object.
func (s *icodemateCLISourceSet) discoveredSourceRef(
	root string, file DiscoveredFile,
) (SourceRef, bool) {
	if strings.HasPrefix(file.Path, "s3://") {
		return s3SourceRefFromDiscoveredFile(root, file), true
	}
	return s.sourceRef(root, file.Path)
}

func (s *icodemateCLISourceSet) Parse(
	ctx context.Context,
	req ParseRequest,
) (ParseOutcome, error) {
	if err := ctx.Err(); err != nil {
		return ParseOutcome{}, err
	}
	path, ok := s.pathFromSource(req.Source)
	if !ok {
		return ParseOutcome{}, fmt.Errorf("icodemate cli source path unavailable")
	}
	machine := firstNonEmptyJSONLString(req.Machine)
	project := claudeProviderProject(ctx, firstNonEmptyJSONLString(
		req.Source.ProjectHint,
		filepath.Base(filepath.Dir(path)),
	), path)
	results, excludedIDs, err := parseIcodemateCLISession(
		path, project, machine,
	)
	if err != nil {
		return ParseOutcome{}, err
	}
	if slices.ContainsFunc(results, func(result ParseResult) bool {
		return result.Session.IsTruncated
	}) {
		return ParseOutcome{ResultSetComplete: false}, nil
	}
	for i := range results {
		if req.Fingerprint.Size != 0 {
			results[i].Session.File.Size = req.Fingerprint.Size
		}
		if req.Fingerprint.MTimeNS != 0 {
			results[i].Session.File.Mtime = req.Fingerprint.MTimeNS
		}
		if req.Fingerprint.Hash != "" {
			results[i].Session.File.Hash = req.Fingerprint.Hash
		}
	}
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
		ForceReplace:       true,
	}, nil
}

func (s *icodemateCLISourceSet) WatchPlan(context.Context) (WatchPlan, error) {
	roots := make([]WatchRoot, 0, len(s.roots))
	for _, root := range s.roots {
		roots = append(roots, WatchRoot{
			Path:        root,
			Recursive:   true,
			DebounceKey: string(AgentIcodemate) + ":cli-projects:" + root,
		})
	}
	return WatchPlan{Roots: roots}, nil
}

func (s *icodemateCLISourceSet) SourcesForChangedPath(
	ctx context.Context,
	req ChangedPathRequest,
) ([]SourceRef, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Mirror the legacy classifier: treat a stat failure as absent only when
	// the path is known missing; otherwise fall back to path-shape
	// classification so a transient unreadable parent does not drop the change.
	allowMissing := jsonlMissingPathFallbackAllowed(req) ||
		claudeChangedPathPresentButUnstatable(req.Path)
	if req.WatchRoot != "" {
		root := filepath.Clean(req.WatchRoot)
		if !s.hasRoot(root) {
			return nil, nil
		}
		if sources, err := s.sourcesForToolResultPath(root, req.Path); err != nil || len(sources) > 0 {
			return sources, err
		}
		source, ok := s.sourceForChangedPath(root, req.Path, allowMissing)
		if !ok {
			return nil, nil
		}
		return []SourceRef{source}, nil
	}
	for _, root := range s.roots {
		if sources, err := s.sourcesForToolResultPath(root, req.Path); err != nil || len(sources) > 0 {
			return sources, err
		}
		source, ok := s.sourceForChangedPath(root, req.Path, allowMissing)
		if ok {
			return []SourceRef{source}, nil
		}
	}
	return nil, nil
}

func (s *icodemateCLISourceSet) FindSource(
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

func (s *icodemateCLISourceSet) Fingerprint(
	ctx context.Context,
	source SourceRef,
) (SourceFingerprint, error) {
	if err := ctx.Err(); err != nil {
		return SourceFingerprint{}, err
	}
	path, ok := s.pathFromSource(source)
	if !ok {
		return SourceFingerprint{}, fmt.Errorf("icodemate cli source path unavailable")
	}
	info, err := os.Stat(path)
	if err != nil {
		return SourceFingerprint{}, fmt.Errorf("stat %s: %w", path, err)
	}
	if info.IsDir() {
		return SourceFingerprint{}, fmt.Errorf("stat %s: source is a directory", path)
	}
	hash, size, mtime, err := icodemateCLICompositeFingerprint(path, info)
	if err != nil {
		return SourceFingerprint{}, err
	}
	inode, device := sourceFileIdentity(info)
	return SourceFingerprint{
		Key:     firstNonEmptyJSONLString(source.FingerprintKey, source.Key, path),
		Size:    size,
		MTimeNS: mtime,
		Inode:   inode,
		Device:  device,
		Hash:    hash,
	}, nil
}

func (s *icodemateCLISourceSet) sourcesForToolResultPath(
	root, changedPath string,
) ([]SourceRef, error) {
	root = filepath.Clean(root)
	changedPath = filepath.Clean(changedPath)
	rel, err := filepath.Rel(root, changedPath)
	if err != nil || rel == ".." ||
		strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, nil
	}
	parts := strings.Split(rel, string(filepath.Separator))
	toolResultsAt := slices.Index(parts, "tool-results")
	if toolResultsAt < 2 || toolResultsAt == len(parts)-1 {
		return nil, nil
	}
	toolDir := filepath.Join(append([]string{root}, parts[:toolResultsAt+1]...)...)
	ownerBase := filepath.Dir(toolDir)
	var sources []SourceRef
	seen := make(map[string]struct{})
	add := func(path string) {
		source, ok := s.sourceRef(root, path)
		if !ok {
			return
		}
		if _, ok := seen[source.Key]; ok {
			return
		}
		seen[source.Key] = struct{}{}
		sources = append(sources, source)
	}
	add(ownerBase + ".jsonl")
	if slices.Contains(parts[:toolResultsAt], "subagents") {
		return sources, nil
	}

	subagentsRoot := filepath.Join(ownerBase, "subagents")
	err = filepath.WalkDir(subagentsRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "agent-") ||
			!strings.HasSuffix(entry.Name(), ".jsonl") {
			return nil
		}
		add(path)
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	return sources, err
}

func icodemateCLICompositeFingerprint(
	path string, transcriptInfo os.FileInfo,
) (hash string, size, mtime int64, err error) {
	transcriptHash, err := hashJSONLSourceFile(path)
	if err != nil {
		return "", 0, 0, err
	}
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "transcript\x00%s\x00", transcriptHash)
	sidecars, size, mtime, err := icodemateCLICompositeFileInfo(
		path, transcriptInfo,
	)
	if err != nil {
		return "", 0, 0, err
	}
	transcriptDir := filepath.Dir(path)
	for _, sidecar := range sidecars {
		relativePath, relErr := filepath.Rel(transcriptDir, sidecar)
		if relErr != nil {
			return "", 0, 0, fmt.Errorf(
				"resolve sidecar %s relative to %s: %w",
				sidecar, path, relErr,
			)
		}
		sidecarHash, hashErr := hashJSONLSourceFile(sidecar)
		if hashErr != nil {
			return "", 0, 0, hashErr
		}
		_, _ = fmt.Fprintf(
			h, "sidecar\x00%s\x00%s\x00",
			filepath.ToSlash(relativePath), sidecarHash,
		)
	}
	return fmt.Sprintf("%x", h.Sum(nil)), size, mtime, nil
}

// IcodemateCLICompositeFileInfo reports the same aggregate size and mtime used
// by the CLI source fingerprint. The sync duplicate resolver uses it to compare
// a candidate against the committed source without ignoring tool-result
// sidecars.
func IcodemateCLICompositeFileInfo(path string) (size, mtime int64, err error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, 0, err
	}
	_, size, mtime, err = icodemateCLICompositeFileInfo(path, info)
	return size, mtime, err
}

func icodemateCLICompositeFileInfo(
	path string, transcriptInfo os.FileInfo,
) (sidecars []string, size, mtime int64, err error) {
	sidecars, err = icodemateCLISidecarFiles(path)
	if err != nil {
		return nil, 0, 0, err
	}
	size = transcriptInfo.Size()
	mtime = transcriptInfo.ModTime().UnixNano()
	for _, sidecar := range sidecars {
		info, statErr := os.Stat(sidecar)
		if statErr != nil {
			return nil, 0, 0, statErr
		}
		size += info.Size()
		mtime = max(mtime, info.ModTime().UnixNano())
	}
	return sidecars, size, mtime, nil
}

func icodemateCLISidecarFiles(sessionPath string) ([]string, error) {
	seenDirs := make(map[string]struct{})
	var files []string
	for _, dir := range claudeToolResultDirs(sessionPath) {
		dir = filepath.Clean(dir)
		if _, ok := seenDirs[dir]; ok {
			continue
		}
		seenDirs[dir] = struct{}{}
		err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !entry.IsDir() {
				files = append(files, path)
			}
			return nil
		})
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	slices.Sort(files)
	return slices.Compact(files), nil
}

func (s *icodemateCLISourceSet) pathFromSource(source SourceRef) (string, bool) {
	switch src := source.Opaque.(type) {
	case icodemateCLISource:
		return src.Path, src.Path != ""
	case *icodemateCLISource:
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
				src := ref.Opaque.(icodemateCLISource)
				return src.Path, true
			}
		}
	}
	return "", false
}

func (s *icodemateCLISourceSet) sourceForPath(root, path string) (SourceRef, bool) {
	return s.sourceForChangedPath(root, path, false)
}

func (s *icodemateCLISourceSet) sourceForChangedPath(
	root, path string, allowMissing bool,
) (SourceRef, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	if allowMissing {
		return s.sourceRefFromPath(root, path)
	}
	return s.sourceRef(root, path)
}

func (s *icodemateCLISourceSet) sourceRef(root, path string) (SourceRef, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	if !IsRegularFile(path) {
		return SourceRef{}, false
	}
	return s.sourceRefFromPath(root, path)
}

func (s *icodemateCLISourceSet) sourceRefFromPath(
	root, path string,
) (SourceRef, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	project, ok := claudeProjectHintFromPath(root, path)
	if !ok {
		return SourceRef{}, false
	}
	return SourceRef{
		Provider:       AgentIcodemate,
		Key:            path,
		DisplayPath:    path,
		FingerprintKey: path,
		ProjectHint:    project,
		Opaque: icodemateCLISource{
			Root: root,
			Path: path,
		},
	}, true
}

func (s *icodemateCLISourceSet) hasRoot(root string) bool {
	for _, configured := range s.roots {
		if samePath(root, configured) {
			return true
		}
	}
	return false
}

// parseIcodemateCLISession parses one ICodeMate CLI JSONL transcript through
// the shared Claude graph parser, then moves every result into ICodeMate's
// namespace. Reusing the graph parser is required for fork selection,
// streaming assistant snapshots, and persisted tool-result sidecars to keep
// the same semantics as the compatible Claude transcript format.
func parseIcodemateCLISession(
	path, project, machine string,
) ([]ParseResult, []string, error) {
	results, excluded, err := claudeParseFile(
		path, project, machine,
		claudeParseOptions{compatibleTitleEvents: true},
	)
	if err != nil {
		return nil, nil, fmt.Errorf("icodemate cli parse %s: %w", path, err)
	}

	InferRelationshipTypes(results)
	applyIcodemateCLIIdentity(results)
	for i := range excluded {
		excluded[i] = icodemateCLISessionID(excluded[i])
	}
	return results, excluded, nil
}

func icodemateCLISessionID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" || strings.HasPrefix(id, "icodemate:") {
		return id
	}
	return "icodemate:" + id
}

func applyIcodemateCLIIdentity(results []ParseResult) {
	for i := range results {
		result := &results[i]
		result.Session.Agent = AgentIcodemate
		result.Session.ID = icodemateCLISessionID(result.Session.ID)
		result.Session.ParentSessionID = icodemateCLISessionID(
			result.Session.ParentSessionID,
		)
		result.Session.SourceSessionID = icodemateCLISessionID(
			result.Session.SourceSessionID,
		)
		for j := range result.Messages {
			for k := range result.Messages[j].ToolCalls {
				call := &result.Messages[j].ToolCalls[k]
				call.SubagentSessionID = icodemateCLISessionID(
					call.SubagentSessionID,
				)
				for n := range call.ResultEvents {
					call.ResultEvents[n].SubagentSessionID =
						icodemateCLISessionID(
							call.ResultEvents[n].SubagentSessionID,
						)
				}
			}
		}
	}
}
