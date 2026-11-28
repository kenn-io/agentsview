package parser

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// CodeBuddyCutoffMtime returns the stat-only freshness signal an incremental
// sync compares with its cutoff for a CodeBuddy CLI or IDE 5.x transcript.
// Each input the parse reads counts by the later of its modification and
// change times, so a rewrite that restores the modification time, or a file
// restored from a backup with its old time, still looks recent. The inputs
// are the transcript and its saved tool outputs, the IDE catalog beside the
// projects folder (titles and renames live there), every blob-store path the
// transcript's images can be read from (a missing image counts as nothing,
// so one that appears later looks recent), and, for a conversation the IDE
// converted from 4.x, the 4.x directory whose request totals and pasted
// images the parse still reads. A branch whose catalog row names a fork
// parent also counts the parent's transcript, so a parent that arrives under
// a filename other than its recorded id makes the branch look recent and the
// branch's link is repaired. A saved-output folder that cannot be listed
// counts by its own times, which change when its permissions are restored.
// Listing the images reads the transcript only when its metadata changed
// since this process last scanned it. Copies of a conversation are ranked by
// the transcript time alone, so this value only decides whether a quick sync
// looks at the file again. roots are the configured CodeBuddy roots.
//
// Cost of the fork-parent input: a transcript under a catalog with no fork
// rows pays a cached catalog lookup. Under a catalog with fork rows, each
// main transcript also reads its header, and only a branch with a fork
// parent resolves it: a stat and a header read when the parent is named after
// its id, otherwise a lookup in the cached recorded-id index (see
// codebuddyCBCRawIDForRuntime).
func CodeBuddyCutoffMtime(ctx context.Context, path string, roots []string) (int64, error) {
	if _, err := os.Stat(path); err != nil {
		return 0, err
	}
	paths := []string{path}
	for _, dir := range codebuddyCBCToolResultDirs(path) {
		// The session directory outlives a removed tool-results directory,
		// and directory times record removed outputs.
		paths = append(paths, filepath.Dir(dir))
		unreadable, err := codebuddyWalkToolResultDir(ctx, dir, func(p string, _ fs.DirEntry) {
			paths = append(paths, p)
		})
		if err != nil {
			return 0, err
		}
		paths = append(paths, unreadable...)
	}
	if root, ok := codebuddyCBCRootFromPath(path); ok {
		if catalog := codebuddyCatalogPathForProjects(root); catalog != "" {
			paths = append(paths, catalog, catalog+"-wal")
			parent, err := codebuddyForkParentCutoffPaths(ctx, root, catalog, path)
			if err != nil {
				return 0, err
			}
			paths = append(paths, parent...)
		}
		blobs, err := codebuddyTranscriptBlobPaths(ctx, filepath.Dir(root), path)
		if err != nil {
			return 0, err
		}
		paths = append(paths, blobs...)
	}
	legacy, err := codebuddyLegacyCutoffPaths(ctx, path, roots)
	if err != nil {
		return 0, err
	}
	paths = append(paths, legacy...)
	var mtime int64
	for _, p := range paths {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		info, err := os.Stat(p)
		if err != nil {
			continue
		}
		ctime, _ := codexIndexChangeTime(p, info)
		mtime = max(mtime, info.ModTime().UnixNano(), ctime)
	}
	return mtime, nil
}

// codebuddyForkParentCutoffPaths lists the transcripts whose arrival or change
// can move the fork-parent link of the main transcript at path. For each of
// the parent's runtime ids it adds the transcript named after that id beside
// path, and, when that one does not record the id, the first other
// transcript the root's recorded-id index finds for it. The search order
// follows codebuddyCBCRawIDForRuntime. An id that resolves to nothing adds
// only its named path, which counts as nothing while it is missing.
func codebuddyForkParentCutoffPaths(
	ctx context.Context, root, catalogPath, path string,
) ([]string, error) {
	if codebuddyCBCIdentityForPath(path).subagent {
		return nil, nil
	}
	catalog, err := loadCodeBuddyCatalog(ctx, catalogPath)
	if err != nil || !codebuddyCatalogHasForks(catalogPath, catalog) {
		// An unreadable catalog counts by its own times above.
		return nil, ctx.Err()
	}
	entry := catalog.byRuntime[readCodeBuddyCBCHeader(ctx, path).catalogKey(path)]
	if entry == nil || entry.forkParentRuntimeID == "" {
		return nil, ctx.Err()
	}
	clean := filepath.Clean(path)
	projectDir, _, _ := codebuddyCBCPathParts(path)
	var paths []string
	// Every runtime id counts: a parent arriving under a later one can move
	// the link while an earlier one has no transcript.
	for _, runtimeID := range entry.forkParentRuntimeIDs {
		named := filepath.Join(projectDir, runtimeID+".jsonl")
		if named != clean && codebuddyCBCSourcePath(root, named) {
			// Counted even when it records another id, because recording
			// this one later moves the link to it.
			paths = append(paths, named)
			info, err := os.Stat(named)
			if err == nil && info.Mode().IsRegular() &&
				readCodeBuddyCBCHeader(ctx, named).catalogKey(named) == runtimeID {
				continue
			}
		}
		recorders, err := codebuddyCBCTranscriptsRecording(ctx, root, runtimeID)
		if err != nil {
			return nil, err
		}
		for _, recorder := range recorders {
			if recorder != clean {
				paths = append(paths, recorder)
				break
			}
		}
	}
	return paths, nil
}

// codebuddyCatalogForks remembers, per catalog path, whether the last catalog
// read names any fork parent, so transcripts under a catalog without forks
// skip the header read the row lookup needs. An entry is recomputed when the
// catalog cache returns a new read.
var codebuddyCatalogForks = struct {
	sync.Mutex
	byPath map[string]codebuddyCatalogForkFlag
}{byPath: make(map[string]codebuddyCatalogForkFlag)}

type codebuddyCatalogForkFlag struct {
	catalog  *codebuddyCatalog
	hasForks bool
}

// codebuddyCatalogHasForks reports whether catalog, the current read of the
// catalog at path, has a row naming a fork parent.
func codebuddyCatalogHasForks(path string, catalog *codebuddyCatalog) bool {
	if catalog == nil {
		return false
	}
	codebuddyCatalogForks.Lock()
	defer codebuddyCatalogForks.Unlock()
	if flag, ok := codebuddyCatalogForks.byPath[path]; ok && flag.catalog == catalog {
		return flag.hasForks
	}
	flag := codebuddyCatalogForkFlag{catalog: catalog}
	for _, entry := range catalog.byRuntime {
		if entry.forkParentRuntimeID != "" {
			flag.hasForks = true
			break
		}
	}
	if len(codebuddyCatalogForks.byPath) >= 16 {
		clear(codebuddyCatalogForks.byPath)
	}
	codebuddyCatalogForks.byPath[path] = flag
	return flag.hasForks
}

// codebuddyLegacyCutoffPaths lists the 4.x directory, manifest, and files a
// converted transcript's parse reads. The directory times record removed
// entries. Transcripts the IDE did not convert, and sync setups without 4.x
// roots, have none.
func codebuddyLegacyCutoffPaths(ctx context.Context, path string, roots []string) ([]string, error) {
	legacyRoots, _ := splitCodeBuddyRoots(roots)
	if len(legacyRoots) == 0 {
		return nil, nil
	}
	header := readCodeBuddyCBCHeader(ctx, path)
	if !header.converted {
		return nil, nil
	}
	dir := codebuddyLegacySessionDir(ctx, legacyRoots, header.cwd,
		codebuddyCBCIdentityForPath(path).rawID)
	if dir == "" {
		return nil, ctx.Err()
	}
	messages := filepath.Join(dir, "messages")
	assets := filepath.Join(dir, "assets")
	paths := []string{dir, filepath.Join(dir, "index.json"), messages, assets}
	for _, child := range []string{messages, assets} {
		entries, _ := os.ReadDir(child)
		for _, entry := range entries {
			if entry.Type().IsRegular() {
				paths = append(paths, filepath.Join(child, entry.Name()))
			}
		}
	}
	return paths, nil
}
