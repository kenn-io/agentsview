package parser

import (
	"context"
	"errors"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/tidwall/gjson"
)

var (
	_ RawCaptureProvider                = (*piProvider)(nil)
	_ StreamingRawCaptureSourceProvider = (*piProvider)(nil)
	_ RawCaptureSourceProvider          = (*piProvider)(nil)
)

// Resolve only aliases supplied by the materializer, including equivalent
// client path spellings. Cleaning is lexical and never probes the worker.
func resolvePiStoredPath(resolve func(string) (string, bool), stored string) (string, bool) {
	if local, ok := resolve(stored); ok {
		return local, true
	}
	normalized := strings.ReplaceAll(stored, `\`, "/")
	cleaned := path.Clean(normalized)
	if strings.HasPrefix(normalized, "//") && !strings.HasPrefix(cleaned, "//") {
		cleaned = "/" + cleaned
	}
	if local, ok := resolve(cleaned); ok {
		return local, true
	}
	return resolve(strings.ReplaceAll(cleaned, "/", `\`))
}

func (p *piProvider) DiscoverRawCaptureSourcesEach(ctx context.Context, yield func(SourceRef) error) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	ctx = withRawCaptureStreamingTraversal(ctx)
	// Unlike ordinary discovery, a missing configured root cannot prove deletions.
	var incomplete error
	var completedRoots []string
	for _, root := range p.sources.roots {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if err := ReportRawCaptureDiscoveryProgress(ctx); err != nil {
			return false, err
		}
		info, err := os.Stat(root)
		if err == nil && !info.IsDir() {
			err = errors.New("raw capture root is not a directory")
		}
		if err != nil {
			incomplete = errors.Join(
				incomplete,
				incompleteDiscoveryError(p.Def.Type, "stat raw capture root", err),
			)
			continue
		}
		canonicalRoot, err := filepath.EvalSymlinks(root)
		if err != nil {
			rootErr, isIncomplete := rawCaptureIncompleteRootError(p.Def.Type, root, err)
			if !isIncomplete {
				return false, errors.Join(incomplete, err)
			}
			incomplete = errors.Join(incomplete, rootErr)
			continue
		}
		reader := ctx.Value(streamingDirectoryReaderContextKey{}).(streamingDirectoryReader)
		active := map[string]bool{}
		confined := withStreamingDirectoryReader(ctx, func(readCtx context.Context, dir string, yield func(os.DirEntry) error) error {
			resolved, err := filepath.EvalSymlinks(dir)
			if err != nil {
				return err
			}
			if resolved != canonicalRoot && !pathUnderRoot(canonicalRoot, resolved) {
				return invalidRawCapturePlan("pi raw discovery directory escapes its configured root")
			}
			if active[resolved] {
				return invalidRawCapturePlan("pi raw discovery directory contains a symlink cycle")
			}
			active[resolved] = true
			defer delete(active, resolved)
			return reader(readCtx, dir, yield)
		})
		err = p.sources.discoverDirEach(confined, root, root, func(source SourceRef) error {
			src, ok := source.Opaque.(JSONLSource)
			if !ok {
				return invalidRawCapturePlan("pi raw source has no physical path")
			}
			// Only a completed traversal proves that an earlier overlapping root
			// already covered this source. An incomplete parent scan may leave a
			// nested configured root as the only route to its transcripts.
			for _, earlier := range completedRoots {
				if p.sources.pathAllowedByRoot(earlier, src.Path) && p.sources.pathIncluded(earlier, src.Path) && p.sources.sourcePathAllowedByDescendPath(earlier, src.Path) {
					return nil
				}
			}
			return yield(source)
		})
		if err == nil {
			completedRoots = append(completedRoots, root)
		}
		if err != nil {
			if cause, ok := discoveryYieldCause(err); ok {
				return false, errors.Join(incomplete, cause)
			}
			rootErr, isIncomplete := rawCaptureIncompleteRootError(p.Def.Type, root, err)
			if !isIncomplete {
				return false, errors.Join(incomplete, err)
			}
			incomplete = errors.Join(incomplete, rootErr)
		}
	}
	return incomplete == nil, incomplete
}

func (p *piProvider) RawCaptureSourcesForChangedPath(ctx context.Context, req ChangedPathRequest) ([]SourceRef, error) {
	// Raw custody includes invalid physical bytes; normalized discovery still filters headers.
	return p.sources.SourcesForChangedPath(ctx, req)
}

// sameCaseInsensitivePathAlias reports whether two differently cased paths
// resolve through one directory entry at every differing component. Requiring
// one matching entry keeps distinct POSIX names separate even when they are
// hard links to the same file.
func sameCaseInsensitivePathAlias(root, first, second string) bool {
	firstRel, err := filepath.Rel(root, first)
	if err != nil {
		return false
	}
	secondRel, err := filepath.Rel(root, second)
	if err != nil {
		return false
	}
	firstRel = filepath.ToSlash(firstRel)
	secondRel = filepath.ToSlash(secondRel)
	if firstRel == secondRel || !strings.EqualFold(firstRel, secondRel) {
		return false
	}
	firstParts := strings.Split(firstRel, "/")
	secondParts := strings.Split(secondRel, "/")
	if len(firstParts) != len(secondParts) {
		return false
	}
	current := root
	for i, firstPart := range firstParts {
		secondPart := secondParts[i]
		if firstPart == secondPart {
			current = filepath.Join(current, firstPart)
			continue
		}
		entries, err := os.ReadDir(current)
		if err != nil {
			return false
		}
		matchedName := ""
		matches := 0
		for _, entry := range entries {
			if strings.EqualFold(entry.Name(), firstPart) && strings.EqualFold(entry.Name(), secondPart) {
				matchedName = entry.Name()
				matches++
			}
		}
		if matches != 1 {
			return false
		}
		current = filepath.Join(current, matchedName)
	}
	return true
}

func piRawCaptureRootRelativePath(configuredRoot, canonicalRoot, candidate string) (string, bool) {
	if rel, ok := relUnder(configuredRoot, candidate); ok {
		return rel, true
	}
	return relUnder(canonicalRoot, candidate)
}

func (p *piProvider) PlanRawCapture(ctx context.Context, source SourceRef) (RawCapturePlan, error) {
	if err := ctx.Err(); err != nil {
		return RawCapturePlan{}, err
	}
	src, ok := source.Opaque.(JSONLSource)
	if !ok || src.Root == "" || src.Path == "" {
		return RawCapturePlan{}, invalidRawCapturePlan("pi source is not a discovered local transcript")
	}
	configured := slices.Contains(p.sources.roots, src.Root)
	if !configured || !p.sources.pathAllowedByRoot(src.Root, src.Path) || !p.sources.pathIncluded(src.Root, src.Path) || !p.sources.sourcePathAllowedByDescendPath(src.Root, src.Path) {
		return RawCapturePlan{}, invalidRawCapturePlan("pi source is outside its configured layout")
	}
	root, err := filepath.EvalSymlinks(src.Root)
	if err != nil {
		return RawCapturePlan{}, err
	}
	directory, err := os.OpenRoot(src.Root)
	if err != nil {
		return RawCapturePlan{}, err
	}
	defer directory.Close()
	plan := RawCapturePlan{ConfiguredRoot: src.Root, CaptureRoot: src.Root, SourceKey: source.Key}
	seen := map[string]bool{}
	structuralParentPath := ""
	switch p.Def.Type {
	case AgentOMP:
		structuralParentPath = filepath.Dir(src.Path) + ".jsonl"
	case AgentPi, AgentOMO:
		if isPiSubagentPath(src.Path) {
			structuralParentPath = piSubagentParentPath(src.Path)
		}
	default:
		// Other Pi-family providers do not use a structural parent path.
	}
	structuralParentLogical := ""
	if pathUnderRoot(src.Root, structuralParentPath) {
		if rel, err := filepath.Rel(src.Root, structuralParentPath); err == nil {
			structuralParentLogical = filepath.ToSlash(rel)
		}
	}
	type capturedFile struct {
		path string
		info os.FileInfo
	}
	var capturedFiles []capturedFile
	var readErr error
	read := piHeaderLookup(func(path string) (string, bool) {
		if readErr != nil {
			return "", false
		}
		if resolver := p.Config.StoredPathResolver; resolver != nil {
			if local, ok := resolvePiStoredPath(resolver, path); ok {
				path = local
			}
		}
		// An uncaptured client reference is not permission to probe the worker.
		if p.Config.PathRewriter != nil {
			if _, ok := piRawCaptureRootRelativePath(src.Root, root, path); !ok {
				return "", false
			}
		}
		if err := ctx.Err(); err != nil {
			readErr = err
			return "", false
		}
		info, err := os.Stat(path)
		if os.IsNotExist(err) {
			return "", false
		}
		if err != nil {
			readErr = err
			return "", false
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			readErr = err
			return "", false
		}
		rel, inConfiguredRoot := piRawCaptureRootRelativePath(src.Root, root, path)
		if !info.Mode().IsRegular() || !pathUnderRoot(root, resolved) || !inConfiguredRoot {
			readErr = invalidRawCapturePlan("pi parent or primary is outside its configured root or is not a regular file")
			return "", false
		}
		logical := filepath.ToSlash(rel)
		if !seen[logical] {
			duplicate := -1
			for i, captured := range capturedFiles {
				capturedPath := filepath.Join(src.Root, filepath.FromSlash(captured.path))
				if strings.EqualFold(captured.path, logical) && os.SameFile(captured.info, info) &&
					sameCaseInsensitivePathAlias(src.Root, capturedPath, path) {
					duplicate = i
					break
				}
			}
			if duplicate >= 0 {
				// Keep one manifest entry for the physical file. Prefer the
				// structural spelling so hosted replay can resolve it without
				// folding case-sensitive client paths.
				seen[logical] = true
				if logical == structuralParentLogical && plan.Entries[duplicate].Path != structuralParentLogical {
					plan.Entries[duplicate].Path = structuralParentLogical
					capturedFiles[duplicate].path = structuralParentLogical
				}
			} else {
				seen[logical] = true
				plan.Entries = append(plan.Entries, RawCaptureEntry{Path: logical, LocalPath: path, Appendable: true})
				capturedFiles = append(capturedFiles, capturedFile{path: logical, info: info})
			}
		}
		// os.Root rejects absolute symlink targets. Open the already validated
		// target beneath the root, retaining the original logical entry name.
		resolvedRel, err := filepath.Rel(root, resolved)
		if err != nil {
			readErr = err
			return "", false
		}
		file, err := directory.Open(resolvedRel)
		if err != nil {
			readErr = err
			return "", false
		}
		defer file.Close()
		header, valid, err := scanPiSessionHeader(file)
		if err != nil {
			readErr = err
		}
		return header, valid
	})
	header, valid := read(src.Path)
	if valid && gjson.Valid(header) {
		resolvePiLineage(src.Path, header, p.Def.Type, p.Def.IDPrefix, read, p.Config.StoredPathResolver)
	}
	if readErr != nil {
		return RawCapturePlan{}, readErr
	}
	if len(plan.Entries) == 0 {
		return RawCapturePlan{}, invalidRawCapturePlan("pi primary is missing")
	}
	// Manifest v1 identifies its primary by the longest source-key suffix.
	// Reject ambiguous membership before it can be uploaded under a wrong primary.
	primary := plan.Entries[0].Path
	key := strings.ReplaceAll(source.Key, `\`, "/")
	for _, entry := range plan.Entries[1:] {
		if len(entry.Path) > len(primary) && strings.HasSuffix(key, "/"+entry.Path) {
			return RawCapturePlan{}, invalidRawCapturePlan("pi source has an ambiguous primary path")
		}
	}
	return plan, nil
}

// parentHeaderLookup preserves local parent resolution, while remote imports
// can read only captured entries beneath their materialized roots.
func (p *piProvider) parentHeaderLookup(resolve func(string) (string, bool)) piHeaderLookup {
	return func(path string) (string, bool) {
		if resolve != nil {
			if local, ok := resolvePiStoredPath(resolve, path); ok {
				path = local
			}
		}
		if p.Config.PathRewriter == nil {
			return piSessionHeader(path)
		}
		for _, root := range p.Config.Roots {
			rel, ok := relUnder(root, path)
			if !ok {
				continue
			}
			directory, err := os.OpenRoot(root)
			if err != nil {
				return "", false
			}
			defer directory.Close()
			file, err := directory.Open(rel)
			if err != nil {
				return "", false
			}
			defer file.Close()
			info, err := file.Stat()
			if err != nil || !info.Mode().IsRegular() {
				return "", false
			}
			header, ok, _ := scanPiSessionHeader(file)
			return header, ok
		}
		return "", false
	}
}
