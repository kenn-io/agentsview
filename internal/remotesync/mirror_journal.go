package remotesync

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"os"
	pathpkg "path"
	"path/filepath"
	"sort"
	"strings"

	"go.kenn.io/agentsview/internal/parser"
)

const (
	mirrorJournalVersion      = 1
	mirrorJournalMaxEntries   = 8192
	mirrorJournalMaxPathBytes = 2 << 20
)

func (reason FullImportReason) Valid() bool {
	switch reason {
	case FullImportLegacy, FullImportBootstrap, FullImportExplicit,
		FullImportDataRebuild, FullImportJournalOverflow,
		FullImportJournalRecovery:
		return true
	default:
		return false
	}
}

var (
	ErrUnsupportedMirrorJournal = errors.New("unsupported mirror journal")
	ErrUnreadableMirrorJournal  = errors.New("unreadable mirror journal")
	ErrMalformedMirrorJournal   = errors.New("malformed mirror journal")
)

type FullImportReason string

const (
	FullImportLegacy          FullImportReason = "legacy"
	FullImportBootstrap       FullImportReason = "bootstrap"
	FullImportExplicit        FullImportReason = "explicit-full"
	FullImportDataRebuild     FullImportReason = "data-rebuild"
	FullImportJournalOverflow FullImportReason = "journal-overflow"
	FullImportJournalRecovery FullImportReason = "journal-recovery"
)

type MirrorChangeEntry struct {
	Path            string `json:"path"`
	InvalidateCache bool   `json:"invalidate_cache,omitempty"`
	ForceFullParse  bool   `json:"force_full_parse,omitempty"`
}

type MirrorChangeJournal struct {
	Version                 int                           `json:"version"`
	FullImport              bool                          `json:"full_import,omitempty"`
	FullImportReason        FullImportReason              `json:"full_import_reason,omitempty"`
	InvalidateAll           bool                          `json:"invalidate_all,omitempty"`
	ForceFullParseAll       bool                          `json:"force_full_parse_all,omitempty"`
	RequiredDataVersion     int                           `json:"required_data_version,omitempty"`
	DataRebuildCacheVersion int                           `json:"data_rebuild_cache_version,omitempty"`
	FileScopedDirs          map[parser.AgentType][]string `json:"file_scoped_dirs,omitempty"`
	Entries                 []MirrorChangeEntry           `json:"entries,omitempty"`
}

type JournalMergeStats struct {
	New      int
	Rearmed  int
	Replayed int
}

func mirrorJournalPath(mirrorRoot string) string {
	return filepath.Clean(mirrorRoot) + ".changes.json"
}

func mirrorRelativeRemoteChangePath(
	mirrorRoot, remotePath string,
) (string, error) {
	localPath, err := safeRemappedRemotePath(mirrorRoot, remotePath)
	if err != nil {
		return "", fmt.Errorf("normalize remote mirror change: %w", err)
	}
	return mirrorRelativeLocalChangePath(mirrorRoot, localPath)
}

func mirrorRelativeLocalChangePath(
	mirrorRoot, localPath string,
) (string, error) {
	root := filepath.Clean(mirrorRoot)
	local := filepath.Clean(localPath)
	if local == root || !within(root, local) {
		return "", fmt.Errorf(
			"mirror change %q escapes mirror root %q", localPath, mirrorRoot,
		)
	}
	rel, err := filepath.Rel(root, local)
	if err != nil {
		return "", fmt.Errorf("make mirror change relative: %w", err)
	}
	return normalizeMirrorJournalPath(filepath.ToSlash(rel))
}

func normalizeMirrorJournalPath(value string) (string, error) {
	if value == "" || strings.IndexByte(value, 0) >= 0 {
		return "", errors.New("mirror journal path is empty or contains NUL")
	}
	raw := strings.ReplaceAll(value, `\`, "/")
	if pathpkg.IsAbs(raw) || hasDotDotPathComponent(raw) {
		return "", fmt.Errorf("unsafe mirror journal path %q", value)
	}
	clean := pathpkg.Clean(raw)
	if clean == "." || clean == "" || clean == ".." ||
		strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("unsafe mirror journal path %q", value)
	}
	return clean, nil
}

func mergeMirrorChanges(
	journal MirrorChangeJournal,
	observedPaths []string,
) (MirrorChangeJournal, JournalMergeStats, error) {
	return mergeMirrorChangesWithForce(journal, observedPaths, observedPaths)
}

func mergeMirrorChangesWithForce(
	journal MirrorChangeJournal,
	observedPaths []string,
	forceFullParsePaths []string,
) (MirrorChangeJournal, JournalMergeStats, error) {
	if err := validateMirrorChangeJournal(journal); err != nil {
		return MirrorChangeJournal{}, JournalMergeStats{}, err
	}
	fileScopedPathCount := 0
	fileScopedPathBytes := 0
	if journal.FileScopedDirs != nil {
		var err error
		fileScopedPathCount, fileScopedPathBytes, err =
			validateFileScopedJournalDirs(journal.FileScopedDirs)
		if err != nil {
			return MirrorChangeJournal{}, JournalMergeStats{}, err
		}
	}

	observed := make(map[string]struct{}, len(observedPaths))
	for _, value := range observedPaths {
		path, err := normalizeMirrorJournalPath(value)
		if err != nil {
			return MirrorChangeJournal{}, JournalMergeStats{}, err
		}
		observed[path] = struct{}{}
	}
	forceFullParse := make(map[string]struct{}, len(forceFullParsePaths))
	for _, value := range forceFullParsePaths {
		path, err := normalizeMirrorJournalPath(value)
		if err != nil {
			return MirrorChangeJournal{}, JournalMergeStats{}, err
		}
		if _, ok := observed[path]; !ok {
			return MirrorChangeJournal{}, JournalMergeStats{}, fmt.Errorf(
				"force-full-parse path %q was not observed", value,
			)
		}
		forceFullParse[path] = struct{}{}
	}

	if journal.FullImport {
		if len(observed) > 0 {
			journal.InvalidateAll = true
			if len(forceFullParse) > 0 {
				journal.ForceFullParseAll = true
			}
		}
		return journal, JournalMergeStats{Rearmed: len(observed)}, nil
	}

	entries := make(map[string]MirrorChangeEntry, len(journal.Entries)+len(observed))
	for _, entry := range journal.Entries {
		entries[entry.Path] = entry
	}
	stats := JournalMergeStats{}
	for path := range observed {
		entry, exists := entries[path]
		if exists {
			stats.Rearmed++
		} else {
			stats.New++
		}
		entry.Path = path
		entry.InvalidateCache = true
		_, entry.ForceFullParse = forceFullParse[path]
		entries[path] = entry
	}
	for _, entry := range journal.Entries {
		if _, exists := observed[entry.Path]; !exists {
			stats.Replayed++
		}
	}

	paths := make([]string, 0, len(entries))
	pathBytes := 0
	for path := range entries {
		paths = append(paths, path)
		pathBytes += len(path)
	}
	if len(paths)+fileScopedPathCount > mirrorJournalMaxEntries ||
		pathBytes+fileScopedPathBytes > mirrorJournalMaxPathBytes {
		overflow := overflowMirrorChangeJournal()
		overflow.FileScopedDirs = journal.FileScopedDirs
		return overflow, stats, nil
	}
	sort.Strings(paths)
	journal.Entries = make([]MirrorChangeEntry, 0, len(paths))
	for _, path := range paths {
		journal.Entries = append(journal.Entries, entries[path])
	}
	return journal, stats, nil
}

func overflowMirrorChangeJournal() MirrorChangeJournal {
	return MirrorChangeJournal{
		Version:           mirrorJournalVersion,
		FullImport:        true,
		FullImportReason:  FullImportJournalOverflow,
		InvalidateAll:     true,
		ForceFullParseAll: true,
	}
}

func attachFileScopedJournalDirs(
	journal MirrorChangeJournal,
	dirs map[parser.AgentType][]string,
) (MirrorChangeJournal, error) {
	pathCount, pathBytes, err := validateFileScopedJournalDirs(dirs)
	if err != nil {
		return MirrorChangeJournal{}, err
	}
	if pathCount > mirrorJournalMaxEntries || pathBytes > mirrorJournalMaxPathBytes {
		return MirrorChangeJournal{}, errors.New(
			"file-scoped journal ownership exceeds path limits",
		)
	}
	for _, entry := range journal.Entries {
		pathCount++
		pathBytes += len(entry.Path)
	}
	if pathCount > mirrorJournalMaxEntries || pathBytes > mirrorJournalMaxPathBytes {
		overflow := overflowMirrorChangeJournal()
		overflow.FileScopedDirs = dirs
		return overflow, nil
	}
	journal.FileScopedDirs = dirs
	return journal, nil
}

func disarmMirrorChanges(journal MirrorChangeJournal) MirrorChangeJournal {
	disarmed := journal
	disarmed.InvalidateAll = false
	disarmed.Entries = append([]MirrorChangeEntry(nil), journal.Entries...)
	for i := range disarmed.Entries {
		disarmed.Entries[i].InvalidateCache = false
	}
	return disarmed
}

func loadMirrorChangeJournal(path string) (MirrorChangeJournal, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return MirrorChangeJournal{Version: mirrorJournalVersion}, nil
	}
	if err != nil {
		return MirrorChangeJournal{}, fmt.Errorf(
			"%w: read %q: %v", ErrUnreadableMirrorJournal, path, err,
		)
	}

	var header struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return MirrorChangeJournal{}, fmt.Errorf(
			"%w: decode %q: %v", ErrMalformedMirrorJournal, path, err,
		)
	}
	if header.Version != mirrorJournalVersion {
		return MirrorChangeJournal{}, fmt.Errorf(
			"%w: version %d", ErrUnsupportedMirrorJournal, header.Version,
		)
	}

	var journal MirrorChangeJournal
	if err := json.Unmarshal(
		data, &journal, json.RejectUnknownMembers(true),
	); err != nil {
		return MirrorChangeJournal{}, fmt.Errorf(
			"%w: decode %q: %v", ErrMalformedMirrorJournal, path, err,
		)
	}
	if err := validateMirrorChangeJournal(journal); err != nil {
		return MirrorChangeJournal{}, fmt.Errorf(
			"%w: validate %q: %v", ErrMalformedMirrorJournal, path, err,
		)
	}
	return journal, nil
}

func validateMirrorChangeJournal(journal MirrorChangeJournal) error {
	if journal.Version != mirrorJournalVersion {
		return fmt.Errorf("unsupported version %d", journal.Version)
	}
	if journal.FullImport {
		switch journal.FullImportReason {
		case "":
			if journal.RequiredDataVersion == 0 {
				return errors.New("full journal has no reason or required data version")
			}
		case FullImportExplicit, FullImportJournalOverflow,
			FullImportJournalRecovery:
		default:
			return fmt.Errorf(
				"full journal has invalid reason %q", journal.FullImportReason,
			)
		}
		if len(journal.Entries) != 0 {
			return errors.New("full journal must not contain path entries")
		}
	} else if journal.FullImportReason != "" || journal.InvalidateAll ||
		journal.ForceFullParseAll {
		return errors.New("bounded journal has full-import state")
	}
	if journal.RequiredDataVersion < 0 || journal.DataRebuildCacheVersion < 0 {
		return errors.New("journal data versions must not be negative")
	}
	if journal.DataRebuildCacheVersion != 0 && journal.RequiredDataVersion == 0 {
		return errors.New("attempt cache version has no required data version")
	}
	fileScopedPathCount := 0
	fileScopedPathBytes := 0
	if journal.FileScopedDirs != nil {
		var err error
		fileScopedPathCount, fileScopedPathBytes, err =
			validateFileScopedJournalDirs(journal.FileScopedDirs)
		if err != nil {
			return err
		}
	}

	pathBytes := fileScopedPathBytes
	previous := ""
	for i, entry := range journal.Entries {
		normalized, err := normalizeMirrorJournalPath(entry.Path)
		if err != nil || normalized != entry.Path {
			return fmt.Errorf("invalid entry path %q", entry.Path)
		}
		if i > 0 && entry.Path <= previous {
			return errors.New("journal entries are not sorted and unique")
		}
		previous = entry.Path
		pathBytes += len(entry.Path)
	}
	if len(journal.Entries)+fileScopedPathCount > mirrorJournalMaxEntries ||
		pathBytes > mirrorJournalMaxPathBytes {
		return errors.New("bounded journal exceeds path limits")
	}
	return nil
}

func validateFileScopedJournalDirs(
	dirs map[parser.AgentType][]string,
) (int, int, error) {
	if len(dirs) == 0 {
		return 0, 0, errors.New("file-scoped journal ownership is empty")
	}
	pathCount := 0
	pathBytes := 0
	for agent, roots := range dirs {
		if _, known := parser.AgentByType(agent); !known ||
			verbatimFileScopedAgent(agent) ||
			parser.RemoteSyncExcludedAgent(agent) ||
			len(roots) == 0 {
			return 0, 0, fmt.Errorf(
				"journal agent %s is not sanitized file-scoped", agent,
			)
		}
		for _, root := range roots {
			name, err := safeRemotePathArchiveName(root)
			if err != nil {
				return 0, 0, fmt.Errorf(
					"invalid file-scoped journal directory %s %q: %w",
					agent, root, err,
				)
			}
			if name == "" {
				return 0, 0, fmt.Errorf(
					"file-scoped journal directory %s is empty", agent,
				)
			}
			pathCount++
			pathBytes += len(root)
		}
	}
	if pathCount > mirrorJournalMaxEntries || pathBytes > mirrorJournalMaxPathBytes {
		return 0, 0, errors.New("file-scoped journal ownership exceeds path limits")
	}
	return pathCount, pathBytes, nil
}

type mirrorJournalStore struct {
	createTemp func(string, string) (*os.File, error)
	rename     func(string, string) error
	remove     func(string) error
	open       func(string) (*os.File, error)
}

func newMirrorJournalStore() mirrorJournalStore {
	return mirrorJournalStore{
		createTemp: os.CreateTemp,
		rename:     os.Rename,
		remove:     os.Remove,
		open:       os.Open,
	}
}

func replaceMirrorChangeJournal(
	path string, journal MirrorChangeJournal,
) error {
	return newMirrorJournalStore().replace(path, journal)
}

func (store mirrorJournalStore) replace(
	path string, journal MirrorChangeJournal,
) (retErr error) {
	if err := validateMirrorChangeJournal(journal); err != nil {
		return fmt.Errorf("replace mirror journal: %w", err)
	}
	data, err := json.Marshal(journal)
	if err != nil {
		return fmt.Errorf("replace mirror journal: encode: %w", err)
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("replace mirror journal: create directory: %w", err)
	}
	temp, err := store.createTemp(dir, ".mirror-changes-*.tmp")
	if err != nil {
		return fmt.Errorf("replace mirror journal: create temp file: %w", err)
	}
	tempPath := temp.Name()
	defer func() {
		if tempPath != "" {
			_ = store.remove(tempPath)
		}
	}()
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return fmt.Errorf("replace mirror journal: chmod temp file: %w", err)
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return fmt.Errorf("replace mirror journal: write temp file: %w", err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return fmt.Errorf("replace mirror journal: sync temp file: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("replace mirror journal: close temp file: %w", err)
	}
	if err := store.rename(tempPath, path); err != nil {
		return fmt.Errorf("replace mirror journal: %w", err)
	}
	tempPath = ""
	if err := store.syncDirectory(dir); err != nil {
		return fmt.Errorf("replace mirror journal: sync directory: %w", err)
	}
	return nil
}

func retireMirrorChangeJournal(path string) error {
	return newMirrorJournalStore().retire(path)
}

func (store mirrorJournalStore) retire(path string) error {
	err := store.remove(path)
	if errors.Is(err, fs.ErrNotExist) {
		if err := store.syncDirectory(filepath.Dir(path)); err != nil {
			return fmt.Errorf("retire mirror journal: sync directory: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("retire mirror journal: %w", err)
	}
	if err := store.syncDirectory(filepath.Dir(path)); err != nil {
		return fmt.Errorf("retire mirror journal: sync directory: %w", err)
	}
	return nil
}

func (store mirrorJournalStore) syncDirectory(path string) (retErr error) {
	directory, err := store.open(path)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, directory.Close()) }()
	if err := directory.Sync(); err != nil &&
		!isMirrorJournalDirectorySyncUnsupported(err) {
		return err
	}
	return nil
}
