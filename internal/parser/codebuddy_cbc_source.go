package parser

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
)

// codebuddyCBCSource is the opaque payload of one CBC transcript source.
type codebuddyCBCSource struct {
	Root string
	Path string
}

// codebuddyCBCSourceSet discovers CBC transcripts under projects roots:
// <root>/<project>/<session>.jsonl and
// <root>/<project>/<session>/subagents/agent-*.jsonl.
type codebuddyCBCSourceSet struct {
	roots     []string
	assetsDir string
	// assetsReadOnly reads verified copies in assetsDir without saving
	// new images there, for report-only parses.
	assetsReadOnly bool
	// legacyRoots are the configured 4.x roots. A converted conversation
	// reads its pre-conversion usage from them.
	legacyRoots []string
}

func newCodeBuddyCBCSourceSet(roots []string, assetsDir string) codebuddyCBCSourceSet {
	return codebuddyCBCSourceSet{roots: cleanJSONLRoots(roots), assetsDir: assetsDir}
}

// codebuddyCBCSourcePath reports whether path is a main or subagent CBC
// transcript under root.
func codebuddyCBCSourcePath(root, path string) bool {
	rel, ok := relUnder(filepath.Clean(root), filepath.Clean(path))
	return ok && codebuddyCBCTranscriptRel(strings.Split(filepath.ToSlash(rel), "/"))
}

// codebuddyCBCTranscriptRel reports whether parts, the components of a path
// relative to a projects root, name a main or subagent CBC transcript.
func codebuddyCBCTranscriptRel(parts []string) bool {
	if len(parts) == 0 || !strings.HasSuffix(parts[len(parts)-1], ".jsonl") {
		return false
	}
	switch len(parts) {
	case 2:
		stem := strings.TrimSuffix(parts[1], ".jsonl")
		return parts[0] != "" && IsValidSessionID(stem) && !strings.HasPrefix(stem, "agent-")
	case 4:
		stem := strings.TrimSuffix(parts[3], ".jsonl")
		return parts[2] == "subagents" && IsValidSessionID(parts[1]) &&
			strings.HasPrefix(stem, "agent-") && IsValidSessionID(stem)
	}
	return false
}

func (s codebuddyCBCSourceSet) Discover(ctx context.Context) ([]SourceRef, error) {
	return collectDiscoveredSources(ctx, s.DiscoverEach)
}

func (s codebuddyCBCSourceSet) DiscoverEach(
	ctx context.Context, yield func(SourceRef) error,
) error {
	var incomplete error
	for _, root := range s.roots {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := streamDirectoryEntries(ctx, root, func(project os.DirEntry) error {
			isDir, dirErr := streamingDirCandidateOrIncomplete(
				AgentCodeBuddy, "CodeBuddy project directory", project, root,
			)
			if dirErr != nil {
				incomplete = errors.Join(incomplete, dirErr)
				return nil
			}
			if !isDir {
				return nil
			}
			err := s.discoverProject(ctx, root, filepath.Join(root, project.Name()), yield)
			if err == nil {
				return nil
			}
			if _, ok := discoveryYieldCause(err); ok || ctx.Err() != nil {
				return err
			}
			// One unreadable project must not hide the projects after it.
			incomplete = errors.Join(incomplete, err)
			return nil
		})
		if cause, ok := discoveryYieldCause(err); ok {
			return cause
		}
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			// An unreadable projects root must not hide the other roots or
			// the 4.x history searched after them.
			incomplete = errors.Join(incomplete,
				incompleteDiscoveryError(AgentCodeBuddy, "read "+root, err))
		}
	}
	return incomplete
}

// discoverProject yields the main and subagent transcripts of one project
// directory. An unreadable subagents directory makes the result incomplete
// without stopping the rest of the project.
func (s codebuddyCBCSourceSet) discoverProject(
	ctx context.Context, root, projectDir string, yield func(SourceRef) error,
) error {
	entries, err := os.ReadDir(projectDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return incompleteDiscoveryError(AgentCodeBuddy, "read "+projectDir, err)
	}
	var incomplete error
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		path := filepath.Join(projectDir, entry.Name())
		if entry.IsDir() {
			subagents := filepath.Join(path, "subagents")
			subs, err := os.ReadDir(subagents)
			if err != nil {
				if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) {
					incomplete = errors.Join(incomplete,
						incompleteDiscoveryError(AgentCodeBuddy, "read "+subagents, err))
				}
				continue
			}
			for _, sub := range subs {
				subPath := filepath.Join(subagents, sub.Name())
				if sub.Type().IsRegular() && codebuddyCBCSourcePath(root, subPath) {
					if err := yield(s.sourceRef(root, subPath)); err != nil {
						return discoveryYieldError{cause: err}
					}
				}
			}
			continue
		}
		if entry.Type().IsRegular() && codebuddyCBCSourcePath(root, path) {
			if err := yield(s.sourceRef(root, path)); err != nil {
				return discoveryYieldError{cause: err}
			}
		}
	}
	return incomplete
}

func (s codebuddyCBCSourceSet) sourceRef(root, path string) SourceRef {
	projectDir, _, _ := codebuddyCBCPathParts(path)
	return SourceRef{
		Provider:       AgentCodeBuddy,
		ConfiguredRoot: root,
		Key:            path,
		DisplayPath:    path,
		FingerprintKey: path,
		// Copies of one conversation under different roots or projects name
		// the same session; the engine ranks each against the archived copy.
		ReconciliationIdentity: codebuddyCBCIdentityForPath(path).rawID,
		ProjectHint:            filepath.Base(projectDir),
		Opaque:                 codebuddyCBCSource{Root: root, Path: path},
	}
}

func (s codebuddyCBCSourceSet) rootFor(path string) (string, bool) {
	for _, root := range s.roots {
		if codebuddyCBCSourcePath(root, path) {
			return root, true
		}
	}
	return "", false
}

func (s codebuddyCBCSourceSet) WatchRoots() []WatchRoot {
	roots := make([]WatchRoot, 0, len(s.roots)*2)
	for _, root := range s.roots {
		roots = append(roots, WatchRoot{
			Path: root, Recursive: true,
			DebounceKey: string(AgentCodeBuddy) + ":cbc:" + root,
		})
		if catalog := codebuddyCatalogPathForProjects(root); catalog != "" {
			roots = append(roots, WatchRoot{
				Path: filepath.Dir(catalog), Recursive: false,
				IncludeGlobs: []string{CodeBuddyCatalogDBName, CodeBuddyCatalogDBName + "-wal"},
				DebounceKey:  string(AgentCodeBuddy) + ":catalog:" + catalog,
			})
		}
	}
	return roots
}

// SourcesForChangedPath maps a transcript, a persisted tool output, or a
// catalog write back to the transcripts it affects. A catalog write selects
// only the conversations whose catalog rows changed since the previous
// catalog event, so the work follows the changed rows rather than the size
// of the archive.
func (s codebuddyCBCSourceSet) SourcesForChangedPath(
	ctx context.Context, req ChangedPathRequest,
) ([]SourceRef, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path := filepath.Clean(req.Path)
	for _, root := range s.roots {
		if req.WatchRoot != "" && !samePath(req.WatchRoot, root) &&
			!samePath(req.WatchRoot, filepath.Dir(codebuddyCatalogPathForProjects(root))) {
			continue
		}
		if codebuddyCBCSourcePath(root, path) {
			if IsRegularFile(path) || jsonlMissingPathFallbackAllowed(req) {
				return []SourceRef{s.sourceRef(root, path)}, nil
			}
			return nil, nil
		}
		if readers, ok := codebuddyCBCToolResultReaders(root, path); ok {
			var out []SourceRef
			for _, reader := range readers {
				if IsRegularFile(reader) {
					out = append(out, s.sourceRef(root, reader))
				}
			}
			return out, nil
		}
		if catalog := codebuddyCatalogPathForProjects(root); catalog != "" &&
			strings.TrimSuffix(path, "-wal") == catalog {
			return s.sourcesForCatalogChange(ctx, root, catalog)
		}
	}
	return nil, nil
}

func (s codebuddyCBCSourceSet) sourcesForCatalogChange(
	ctx context.Context, root, catalog string,
) ([]SourceRef, error) {
	changed, known, err := codebuddyCatalogChangedRuntimes(ctx, catalog)
	if err != nil {
		return nil, err
	}
	one := codebuddyCBCSourceSet{
		roots: []string{root}, assetsDir: s.assetsDir, assetsReadOnly: s.assetsReadOnly,
	}
	if !known {
		// No earlier read to compare against: fingerprints still skip
		// sessions whose rows did not change.
		return one.Discover(ctx)
	}
	var out []SourceRef
	// A row is keyed by the session id a transcript records. The filename
	// stem usually matches it; ids no stem matches are looked up by the
	// recorded id.
	unmatched := make(map[string]bool)
	for _, runtimeID := range changed {
		source, ok, err := one.FindSource(ctx, FindSourceRequest{RawSessionID: runtimeID})
		if err != nil {
			return nil, err
		}
		if !ok {
			unmatched[runtimeID] = true
			continue
		}
		out = append(out, source)
		if readCodeBuddyCBCHeader(ctx, source.DisplayPath).catalogKey(source.DisplayPath) != runtimeID {
			unmatched[runtimeID] = true
		}
	}
	if len(unmatched) == 0 {
		return out, nil
	}
	recorded, err := one.sourcesRecordingSessionIDs(ctx, unmatched)
	if err != nil {
		return nil, err
	}
	for _, source := range recorded {
		if !slices.ContainsFunc(out, func(s SourceRef) bool { return s.Key == source.Key }) {
			out = append(out, source)
		}
	}
	return out, nil
}

// sourcesRecordingSessionIDs returns the main transcripts whose catalog key,
// the session id they record, is one of ids. It reads only the start of each
// transcript.
func (s codebuddyCBCSourceSet) sourcesRecordingSessionIDs(
	ctx context.Context, ids map[string]bool,
) ([]SourceRef, error) {
	var out []SourceRef
	for _, root := range s.roots {
		projects, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, project := range projects {
			if !isDirOrSymlink(project, root) {
				continue
			}
			entries, err := os.ReadDir(filepath.Join(root, project.Name()))
			if err != nil {
				continue
			}
			for _, entry := range entries {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				path := filepath.Join(root, project.Name(), entry.Name())
				if !entry.Type().IsRegular() || !codebuddyCBCSourcePath(root, path) {
					continue
				}
				if ids[readCodeBuddyCBCHeader(ctx, path).catalogKey(path)] {
					out = append(out, s.sourceRef(root, path))
				}
			}
		}
	}
	return out, nil
}

// codebuddyCBCToolResultReaders maps a file under a tool-results directory
// to the transcripts that read it. An agent reads its own directory and its
// parent session's, so a parent output refreshes the parent and every agent
// transcript of that session.
func codebuddyCBCToolResultReaders(root, path string) ([]string, bool) {
	rel, ok := relUnder(filepath.Clean(root), filepath.Clean(path))
	if !ok {
		return nil, false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	switch {
	case len(parts) == 4 && parts[2] == "tool-results":
		readers := []string{filepath.Join(root, parts[0], parts[1]+".jsonl")}
		subagents := filepath.Join(root, parts[0], parts[1], "subagents")
		entries, _ := os.ReadDir(subagents)
		for _, entry := range entries {
			if agent := filepath.Join(subagents, entry.Name()); codebuddyCBCSourcePath(root, agent) {
				readers = append(readers, agent)
			}
		}
		return readers, true
	case len(parts) == 6 && parts[2] == "subagents" && parts[4] == "tool-results":
		return []string{filepath.Join(root, parts[0], parts[1], "subagents", parts[3]+".jsonl")}, true
	}
	return nil, false
}

func (s codebuddyCBCSourceSet) FindSource(
	ctx context.Context, req FindSourceRequest,
) (SourceRef, bool, error) {
	if err := ctx.Err(); err != nil {
		return SourceRef{}, false, err
	}
	for _, stored := range []string{req.StoredFilePath, req.FingerprintKey} {
		if stored == "" {
			continue
		}
		if root, ok := s.rootFor(stored); ok && IsRegularFile(stored) {
			return s.sourceRef(root, filepath.Clean(stored)), true, nil
		}
	}
	raw := req.RawSessionID
	if raw == "" {
		return SourceRef{}, false, nil
	}
	sessionID, agentID, subagent := strings.Cut(raw, ":subagent:")
	if !IsValidSessionID(sessionID) || subagent && !IsValidSessionID(agentID) {
		return SourceRef{}, false, nil
	}
	var copies []SourceRef
	for _, root := range s.roots {
		projects, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, project := range projects {
			if !isDirOrSymlink(project, root) {
				continue
			}
			candidate := filepath.Join(root, project.Name(), sessionID+".jsonl")
			if subagent {
				candidate = filepath.Join(root, project.Name(), sessionID, "subagents", agentID+".jsonl")
			}
			if IsRegularFile(candidate) {
				copies = append(copies, s.sourceRef(root, candidate))
			}
		}
	}
	if len(copies) == 0 {
		return SourceRef{}, false, nil
	}
	return codebuddyPreferredCBCCopy(ctx, copies), true, nil
}

// codebuddyPreferredCBCCopy picks the copy a lookup by session id returns
// when configured roots or project directories hold several transcripts of
// one conversation: the highest-ranked copy that holds a turn, otherwise the
// highest-ranked copy. An empty or metadata-only copy thus cannot hide a
// usable one, and the 4.x history stays only while no copy holds a turn.
func codebuddyPreferredCBCCopy(ctx context.Context, copies []SourceRef) SourceRef {
	if len(copies) == 1 {
		return copies[0]
	}
	best, bestHolds := 0, false
	var bestRank ReconciliationSourceRank
	for i, source := range copies {
		path := source.DisplayPath
		holds := codebuddyCBCHasConversation(ctx, path)
		rank := codebuddyRank(2, path)
		if i == 0 || holds && !bestHolds || holds == bestHolds && bestRank.Lower(rank) {
			best, bestHolds, bestRank = i, holds, rank
		}
	}
	return copies[best]
}

func (s codebuddyCBCSourceSet) pathFromSource(source SourceRef) (string, string, bool) {
	switch src := source.Opaque.(type) {
	case codebuddyCBCSource:
		return src.Root, src.Path, src.Path != ""
	case *codebuddyCBCSource:
		if src != nil {
			return src.Root, src.Path, src.Path != ""
		}
	case MaterializedFileSource:
		return "", src.Path, src.Path != ""
	}
	for _, candidate := range []string{source.DisplayPath, source.FingerprintKey, source.Key} {
		if root, ok := s.rootFor(candidate); ok {
			return root, filepath.Clean(candidate), true
		}
	}
	return "", "", false
}

// Fingerprint hashes the transcript, the contents of its persisted tool
// outputs and which of their folders cannot be listed, which of its
// blob-store images are present, its catalog row, the archive id of the fork
// parent that row names, and, for a conversation converted from 4.x, the 4.x
// directory it reads usage and images from, so a change to any of them
// reparses the session while unrelated catalog writes leave it untouched.
// Output contents count because a rewrite can keep a file's size and
// modification time. A blob is named by its content digest, so its presence,
// size, and readability are enough.
// The size and modification time are the transcript's own: the archive ranks
// a deleted copy by them against the transcripts of the copies that remain,
// so a saved output written later must not make that copy look newer.
func (s codebuddyCBCSourceSet) Fingerprint(
	ctx context.Context, source SourceRef,
) (SourceFingerprint, error) {
	root, path, ok := s.pathFromSource(source)
	if !ok {
		return SourceFingerprint{}, errors.New("codebuddy source path unavailable")
	}
	info, err := os.Stat(path)
	if err != nil {
		return SourceFingerprint{}, fmt.Errorf("stat %s: %w", path, err)
	}
	transcriptHash, err := hashJSONLSourceFileContext(ctx, path)
	if err != nil {
		return SourceFingerprint{}, err
	}
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "transcript\x00%s\x00", transcriptHash)
	sidecars, unreadableDirs, err := codebuddyCBCSidecarFiles(ctx, path)
	if err != nil {
		return SourceFingerprint{}, err
	}
	for _, sidecar := range sidecars {
		sidecarInfo, err := os.Stat(sidecar)
		if err != nil {
			continue
		}
		rel, _ := filepath.Rel(filepath.Dir(path), sidecar)
		content, err := codebuddySidecarContentHash(ctx, sidecar, sidecarInfo)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return SourceFingerprint{}, ctxErr
			}
			// The parse keeps an unreadable output's placeholder; the
			// output becoming readable changes this entry.
			content = "unreadable"
		}
		_, _ = fmt.Fprintf(h, "sidecar\x00%s\x00%d\x00%d\x00%s\x00",
			filepath.ToSlash(rel), sidecarInfo.Size(), sidecarInfo.ModTime().UnixNano(), content)
	}
	// The parse keeps the placeholders, or the archived text, of outputs in
	// a folder it cannot list; the folder becoming readable changes this.
	for _, dir := range unreadableDirs {
		rel, _ := filepath.Rel(filepath.Dir(path), dir)
		_, _ = fmt.Fprintf(h, "sidecar-dir\x00%s\x00unreadable\x00", filepath.ToSlash(rel))
	}
	header := readCodeBuddyCBCHeader(ctx, path)
	if root != "" {
		blobs, err := codebuddyTranscriptBlobPaths(ctx, filepath.Dir(filepath.Clean(root)), path)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return SourceFingerprint{}, ctxErr
			}
			_, _ = io.WriteString(h, "blobs\x00unreadable\x00")
		}
		for _, blob := range blobs {
			name := filepath.Base(blob)
			blobInfo, err := os.Stat(blob)
			if err != nil {
				_, _ = fmt.Fprintf(h, "blob\x00%s\x00absent\x00", name)
				continue
			}
			// An image unreadable at import stays a placeholder until a
			// parse after its permissions are repaired, which keep its size.
			readable := false
			if f, err := os.Open(blob); err == nil {
				readable = true
				_ = f.Close()
			}
			_, _ = fmt.Fprintf(h, "blob\x00%s\x00%d\x00%t\x00", name, blobInfo.Size(), readable)
		}
		catalog, err := loadCodeBuddyCatalog(ctx, codebuddyCatalogPathForProjects(root))
		if err == nil {
			_, _ = fmt.Fprintf(h, "catalog\x00%s\x00",
				catalog.fingerprint(header.catalogKey(path)))
			// A parent that arrives later under another filename changes
			// the link while the branch's own files stay the same.
			if catalog != nil {
				parent, err := codebuddyCBCForkParent(ctx, path,
					catalog.byRuntime[header.catalogKey(path)])
				if err != nil {
					return SourceFingerprint{}, err
				}
				if parent != "" {
					_, _ = fmt.Fprintf(h, "fork-parent\x00%s\x00", parent)
				}
			}
		}
	}
	if header.converted && len(s.legacyRoots) > 0 {
		err := writeCodeBuddyLegacyCompanionState(ctx, h, s.legacyRoots, header.cwd,
			codebuddyCBCIdentityForPath(path).rawID)
		if err != nil {
			return SourceFingerprint{}, err
		}
	}
	inode, device := sourceFileIdentity(info)
	return SourceFingerprint{
		Key:     firstNonEmptyJSONLString(source.FingerprintKey, source.Key, path),
		Size:    info.Size(),
		MTimeNS: info.ModTime().UnixNano(),
		Inode:   inode,
		Device:  device,
		Hash:    hex.EncodeToString(h.Sum(nil)),
	}, nil
}

// codebuddySidecarHashes remembers each saved output's content digest under
// its size, modification time, change time, and inode. Every rewrite moves
// the change time, even one that restores the modification time, so a
// matching entry proves the contents unchanged without reading them again.
// An agent transcript also reads its parent's outputs, so one session's
// outputs are fingerprinted once per transcript of the session. Without a
// change time or inode the output is read every time.
var codebuddySidecarHashes = struct {
	sync.Mutex
	byPath map[string]codebuddySidecarHash
}{byPath: make(map[string]codebuddySidecarHash)}

type codebuddySidecarHash struct {
	size, mtime, ctime int64
	inode              uint64
	hash               string
}

func codebuddySidecarContentHash(
	ctx context.Context, path string, info os.FileInfo,
) (string, error) {
	ctime, verified := codexIndexChangeTime(path, info)
	inode, _ := sourceFileIdentityForPath(path, info)
	key := codebuddySidecarHash{
		size: info.Size(), mtime: info.ModTime().UnixNano(), ctime: ctime, inode: inode,
	}
	cacheable := verified && inode != 0
	if cacheable {
		codebuddySidecarHashes.Lock()
		cached, ok := codebuddySidecarHashes.byPath[path]
		codebuddySidecarHashes.Unlock()
		if ok && cached.size == key.size && cached.mtime == key.mtime &&
			cached.ctime == key.ctime && cached.inode == key.inode {
			return cached.hash, nil
		}
	}
	hash, err := hashJSONLSourceFileContext(ctx, path)
	if err != nil || !cacheable {
		return hash, err
	}
	key.hash = hash
	codebuddySidecarHashes.Lock()
	if len(codebuddySidecarHashes.byPath) >= 1<<16 {
		clear(codebuddySidecarHashes.byPath)
	}
	codebuddySidecarHashes.byPath[path] = key
	codebuddySidecarHashes.Unlock()
	return hash, nil
}

// codebuddyCBCSidecarFiles lists the persisted tool outputs a transcript can
// read, sorted for a stable fingerprint. An agent transcript also reads its
// parent session's outputs. It also returns each folder that exists but
// cannot be listed: the outputs it can list are still returned, and the
// parse shows the others as placeholders. Only cancellation is an error.
func codebuddyCBCSidecarFiles(ctx context.Context, path string) (files, unreadable []string, err error) {
	for _, dir := range codebuddyCBCToolResultDirs(path) {
		blocked, err := codebuddyWalkToolResultDir(ctx, dir, func(p string, d fs.DirEntry) {
			if d.Type().IsRegular() {
				files = append(files, p)
			}
		})
		if err != nil {
			return nil, nil, err
		}
		unreadable = append(unreadable, blocked...)
	}
	slices.Sort(files)
	slices.Sort(unreadable)
	return slices.Compact(files), slices.Compact(unreadable), nil
}

// codebuddyWalkToolResultDir calls visit for dir and each entry under it that
// can be listed, and returns the directories, dir included, that exist but
// cannot be read. A missing dir is not unreadable. Only cancellation is an
// error.
func codebuddyWalkToolResultDir(
	ctx context.Context, dir string, visit func(string, fs.DirEntry),
) ([]string, error) {
	var unreadable []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				unreadable = append(unreadable, p)
			}
			// Keep the entries a failed listing did return.
			return nil
		}
		visit(p, d)
		return nil
	})
	return unreadable, err
}

// codebuddyCBCToolResultDirs lists the tool-results directories the
// persisted-output reader accepts for a transcript, without duplicates.
func codebuddyCBCToolResultDirs(path string) []string {
	var dirs []string
	for _, dir := range claudeToolResultDirs(path) {
		if !slices.Contains(dirs, dir) {
			dirs = append(dirs, dir)
		}
	}
	return dirs
}

func (s codebuddyCBCSourceSet) Parse(
	ctx context.Context, req ParseRequest,
) (ParseOutcome, error) {
	if err := ctx.Err(); err != nil {
		return ParseOutcome{}, err
	}
	root, path, ok := s.pathFromSource(req.Source)
	if !ok {
		return ParseOutcome{}, errors.New("codebuddy source path unavailable")
	}
	in := codebuddyCBCParseInput{
		path: path, project: req.Source.ProjectHint, machine: req.Machine,
		legacyRoots: s.legacyRoots,
	}
	var imageStoreFailed atomic.Bool
	if root != "" {
		catalog, err := loadCodeBuddyCatalog(ctx, codebuddyCatalogPathForProjects(root))
		if err != nil {
			return ParseOutcome{}, err
		}
		if catalog != nil {
			in.catalog = catalog.byRuntime[readCodeBuddyCBCHeader(ctx, path).catalogKey(path)]
		}
		in.opts.assets = codebuddyBlobAssets{
			home: filepath.Dir(filepath.Clean(root)), assetsDir: s.assetsDir,
			readOnly: s.assetsReadOnly, storeFailed: &imageStoreFailed,
		}
	}
	in.opts.persisted = codebuddyPersistedOutputReader(path, req.StoredPathResolver, req.Machine)
	results, err := parseCodeBuddyCBCFile(ctx, in)
	if err != nil {
		return ParseOutcome{}, err
	}
	if in.catalog != nil {
		results = append(results, codebuddySideQuestionResults(results, in.catalog)...)
	}
	if len(results) == 0 {
		return ParseOutcome{ResultSetComplete: true, SkipReason: SkipNoSession}, nil
	}
	out := make([]ParseResultOutcome, 0, len(results))
	for _, result := range results {
		if req.Fingerprint.Hash != "" {
			result.Session.File.Hash = req.Fingerprint.Hash
		}
		if req.Fingerprint.Size > 0 {
			result.Session.File.Size = req.Fingerprint.Size
		}
		if req.Fingerprint.MTimeNS > 0 {
			result.Session.File.Mtime = req.Fingerprint.MTimeNS
		}
		// An unreadable persisted output stays flagged on its tool result so
		// archive reconciliation keeps the text saved earlier. A late file
		// changes the fingerprint, which reparses the session.
		outcome := ParseResultOutcome{Result: result, DataVersion: DataVersionCurrent}
		if imageStoreFailed.Load() {
			// An image the asset store could not save shows a placeholder
			// for now. The transcript and its fingerprint stay the same
			// once the store is repaired, so only a retry saves it.
			outcome.DataVersion = DataVersionNeedsRetry
			outcome.RetryReason = "codebuddy image could not be saved to the asset store"
		}
		out = append(out, outcome)
	}
	return ParseOutcome{Results: out, ResultSetComplete: true, ForceReplace: true}, nil
}

// codebuddyPersistedOutputReader reads a persisted tool output only from the
// transcript's own tool-results directories.
func codebuddyPersistedOutputReader(
	transcript string, resolve func(string) (string, bool), machine string,
) codebuddyPersistedOutputs {
	return func(ctx context.Context, path string) (string, bool, error) {
		if resolve != nil {
			if local, ok := resolve(path); ok {
				path = local
			} else if local, ok := resolve(machine + ":" + path); ok {
				path = local
			}
		}
		return readClaudePersistedToolResultContext(ctx, transcript, path)
	}
}
