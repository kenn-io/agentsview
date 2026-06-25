package parser

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/tidwall/gjson"
)

// uuidRe matches a standard UUID (8-4-4-4-12 hex) at the end of a rollout filename stem.
var uuidRe = regexp.MustCompile(
	`^rollout-.*-([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-` +
		`[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})$`,
)

// isDirOrSymlink reports whether the entry is a directory or a
// symlink that resolves to a directory. parentDir is needed to
// build the full path for symlink resolution.
func isDirOrSymlink(
	entry os.DirEntry, parentDir string,
) bool {
	if entry.IsDir() {
		return true
	}
	if entry.Type()&os.ModeSymlink == 0 {
		return false
	}
	fi, err := os.Stat(
		filepath.Join(parentDir, entry.Name()),
	)
	if err != nil || fi == nil {
		return false
	}
	return fi.IsDir()
}

// DiscoveredFile holds a discovered session file.
type DiscoveredFile struct {
	Path            string
	Project         string     // pre-extracted project name
	Agent           AgentType  // which agent this file belongs to
	ForceParse      bool       // caller requires a full source reparse
	ProviderSource  *SourceRef // provider-owned source identity, when known
	ProviderProcess bool       // true when this caller may parse via ProviderSource
}

// OpenCodeSourceMode identifies the usable OpenCode storage
// backend found under an OPENCODE_DIR root.
type OpenCodeSourceMode string

const (
	OpenCodeSourceNone    OpenCodeSourceMode = ""
	OpenCodeSourceStorage OpenCodeSourceMode = "storage"
	OpenCodeSourceSQLite  OpenCodeSourceMode = "sqlite"
)

// OpenCodeSource describes the resolved storage backend for an
// OpenCode root.
type OpenCodeSource struct {
	Mode        OpenCodeSourceMode
	Root        string
	SessionRoot string
	DBPath      string
}

// openCodeFormat parameterizes the shared OpenCode storage format by
// the per-agent SQLite filename, the storage/<sessionSubdir> that holds
// session JSON, and the agent label stamped on discovered sessions.
// Kilo is a fork of OpenCode with an identical on-disk layout; MiMoCode
// is a fork that stores sessions under storage/session_diff and a
// mimocode.db SQLite fallback. All share one implementation and differ
// only in these values.
type openCodeFormat struct {
	agent         AgentType
	dbName        string
	sessionSubdir string
}

var (
	openCodeFmt = openCodeFormat{
		agent: AgentOpenCode, dbName: "opencode.db", sessionSubdir: "session",
	}
	kiloFmt = openCodeFormat{
		agent: AgentKilo, dbName: "kilo.db", sessionSubdir: "session",
	}
	mimoFmt = openCodeFormat{
		agent: AgentMiMoCode, dbName: "mimocode.db",
		sessionSubdir: "session_diff",
	}
)

func resolveOpenCodeFormatSource(
	f openCodeFormat, root string,
) OpenCodeSource {
	if root == "" {
		return OpenCodeSource{}
	}

	sessionRoot := filepath.Join(root, "storage", f.sessionSubdir)
	if info, err := os.Stat(sessionRoot); err == nil && info.IsDir() {
		return OpenCodeSource{
			Mode:        OpenCodeSourceStorage,
			Root:        root,
			SessionRoot: sessionRoot,
			DBPath:      filepath.Join(root, f.dbName),
		}
	} else if err != nil && !os.IsNotExist(err) {
		storageRoot := filepath.Join(root, "storage")
		if info, serr := os.Stat(storageRoot); serr == nil && info.IsDir() {
			return OpenCodeSource{
				Mode:        OpenCodeSourceStorage,
				Root:        root,
				SessionRoot: sessionRoot,
				DBPath:      filepath.Join(root, f.dbName),
			}
		}
	}

	dbPath := filepath.Join(root, f.dbName)
	if info, err := os.Stat(dbPath); err == nil && !info.IsDir() {
		return OpenCodeSource{
			Mode:   OpenCodeSourceSQLite,
			Root:   root,
			DBPath: dbPath,
		}
	}

	return OpenCodeSource{Root: root}
}

func discoverOpenCodeFormatSessions(
	f openCodeFormat, root string,
) []DiscoveredFile {
	src := resolveOpenCodeFormatSource(f, root)
	if src.Mode != OpenCodeSourceStorage {
		return nil
	}

	var files []DiscoveredFile
	entries, err := os.ReadDir(src.SessionRoot)
	if err != nil {
		return nil
	}
	for _, entry := range entries {
		if !isDirOrSymlink(entry, src.SessionRoot) {
			continue
		}
		projectDir := filepath.Join(src.SessionRoot, entry.Name())
		sessionEntries, err := os.ReadDir(projectDir)
		if err != nil {
			continue
		}
		for _, sessionEntry := range sessionEntries {
			if sessionEntry.IsDir() ||
				!strings.HasSuffix(sessionEntry.Name(), ".json") {
				continue
			}
			path := filepath.Join(projectDir, sessionEntry.Name())
			files = append(files, DiscoveredFile{
				Path:    path,
				Project: openCodeSessionProject(path),
				Agent:   f.agent,
			})
		}
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].Path < files[j].Path
	})
	return files
}

func findOpenCodeFormatSourceFile(
	f openCodeFormat, root, sessionID string,
) string {
	if !IsValidSessionID(sessionID) {
		return ""
	}

	src := resolveOpenCodeFormatSource(f, root)
	switch src.Mode {
	case OpenCodeSourceStorage:
		if entries, err := os.ReadDir(src.SessionRoot); err == nil {
			for _, entry := range entries {
				if !isDirOrSymlink(entry, src.SessionRoot) {
					continue
				}
				path := filepath.Join(
					src.SessionRoot, entry.Name(),
					sessionID+".json",
				)
				if info, err := os.Stat(path); err == nil &&
					!info.IsDir() {
					return path
				}
			}
		}
		if OpenCodeSQLiteSessionExists(src.DBPath, sessionID) {
			return OpenCodeSQLiteVirtualPath(src.DBPath, sessionID)
		}
		return ""
	case OpenCodeSourceSQLite:
		if OpenCodeSQLiteSessionExists(src.DBPath, sessionID) {
			return OpenCodeSQLiteVirtualPath(src.DBPath, sessionID)
		}
		return ""
	default:
		return ""
	}
}

func openCodeFormatStorageSessionIDs(
	f openCodeFormat, root string,
) map[string]struct{} {
	src := resolveOpenCodeFormatSource(f, root)
	if src.Mode != OpenCodeSourceStorage {
		return nil
	}
	entries, err := os.ReadDir(src.SessionRoot)
	if err != nil {
		return nil
	}
	ids := make(map[string]struct{})
	for _, entry := range entries {
		if !isDirOrSymlink(entry, src.SessionRoot) {
			continue
		}
		projectDir := filepath.Join(src.SessionRoot, entry.Name())
		sessionEntries, err := os.ReadDir(projectDir)
		if err != nil {
			continue
		}
		for _, sessionEntry := range sessionEntries {
			name := sessionEntry.Name()
			if sessionEntry.IsDir() ||
				!strings.HasSuffix(name, ".json") {
				continue
			}
			id := strings.TrimSuffix(name, ".json")
			if id == "" {
				continue
			}
			ids[id] = struct{}{}
		}
	}
	return ids
}

func resolveOpenCodeFormatWatchRoots(
	f openCodeFormat, root string,
) []string {
	if root == "" {
		return nil
	}
	src := resolveOpenCodeFormatSource(f, root)
	switch src.Mode {
	case OpenCodeSourceStorage:
		if info, err := os.Stat(src.DBPath); err == nil &&
			!info.IsDir() {
			return []string{root}
		}
		return []string{filepath.Join(root, "storage")}
	case OpenCodeSourceSQLite:
		return []string{root}
	}
	if info, err := os.Stat(root); err == nil && info.IsDir() {
		return []string{root}
	}
	return nil
}

func parseOpenCodeFormatVirtualPath(
	dbName, sourcePath string,
) (dbPath, sessionID string, ok bool) {
	idx := strings.LastIndex(sourcePath, "#")
	if idx <= 0 || idx >= len(sourcePath)-1 {
		return "", "", false
	}
	dbPath = sourcePath[:idx]
	sessionID = sourcePath[idx+1:]
	if filepath.Base(dbPath) != dbName {
		return "", "", false
	}
	return dbPath, sessionID, true
}

// ResolveOpenCodeSource detects whether an OpenCode root is using
// file-backed storage or legacy SQLite storage.
func ResolveOpenCodeSource(root string) OpenCodeSource {
	return resolveOpenCodeFormatSource(openCodeFmt, root)
}

// DiscoverOpenCodeSessions finds all file-backed OpenCode session
// JSON files under storage/session.
func DiscoverOpenCodeSessions(root string) []DiscoveredFile {
	return discoverOpenCodeFormatSessions(openCodeFmt, root)
}

// FindOpenCodeSourceFile locates a single OpenCode session source
// path or SQLite backing file by raw session ID. Returns "" when
// the session is not present under this root so the caller
// (Engine.FindSourceFile) can continue searching later configured
// roots — important when an early hybrid root with an unrelated
// opencode.db could otherwise shadow a session in a later root.
func FindOpenCodeSourceFile(root, sessionID string) string {
	return findOpenCodeFormatSourceFile(openCodeFmt, root, sessionID)
}

// OpenCodeStorageSessionIDs returns the set of session IDs that
// have a JSON file under storage/session/*/ in the given root.
// Returns nil for non-storage roots. In hybrid roots (storage and
// SQLite both present) the storage transcript is canonical, so
// callers use this to skip duplicate SQLite metas during sync.
func OpenCodeStorageSessionIDs(root string) map[string]struct{} {
	return openCodeFormatStorageSessionIDs(openCodeFmt, root)
}

// ResolveOpenCodeWatchRoots returns the directories that should be
// watched for live OpenCode updates under a configured root. Pure
// storage mode targets the storage/ subtree so fsnotify does not
// recurse over unrelated opencode state (binaries, logs, caches),
// while still covering the session/message/part subdirs — including
// ones that OpenCode creates lazily after the watcher starts, since
// the watcher auto-adds new subdirectories on Create events. Hybrid
// storage+SQLite roots and pure SQLite mode watch the root so DB/WAL
// updates are observed too.
func ResolveOpenCodeWatchRoots(root string) []string {
	return resolveOpenCodeFormatWatchRoots(openCodeFmt, root)
}

func OpenCodeSQLiteVirtualPath(
	dbPath, sessionID string,
) string {
	return dbPath + "#" + sessionID
}

func ParseOpenCodeSQLiteVirtualPath(
	sourcePath string,
) (dbPath, sessionID string, ok bool) {
	return parseOpenCodeFormatVirtualPath(openCodeFmt.dbName, sourcePath)
}

func openCodeSessionProject(path string) string {
	data, err := os.ReadFile(path)
	if err == nil {
		if cwd := gjson.GetBytes(data, "directory").Str; cwd != "" {
			if project := ExtractProjectFromCwd(cwd); project != "" {
				return project
			}
		}
	}

	if project := NormalizeName(filepath.Base(filepath.Dir(path))); project != "" {
		return project
	}
	return "unknown"
}

// ResolveKiloSource detects whether a Kilo root is using file-backed
// storage or legacy SQLite storage.
func ResolveKiloSource(root string) OpenCodeSource {
	return resolveOpenCodeFormatSource(kiloFmt, root)
}

func DiscoverKiloSessions(root string) []DiscoveredFile {
	return discoverOpenCodeFormatSessions(kiloFmt, root)
}

func FindKiloSourceFile(root, sessionID string) string {
	return findOpenCodeFormatSourceFile(kiloFmt, root, sessionID)
}

func KiloStorageSessionIDs(root string) map[string]struct{} {
	return openCodeFormatStorageSessionIDs(kiloFmt, root)
}

func ResolveKiloWatchRoots(root string) []string {
	return resolveOpenCodeFormatWatchRoots(kiloFmt, root)
}

func KiloSQLiteVirtualPath(dbPath, sessionID string) string {
	return OpenCodeSQLiteVirtualPath(dbPath, sessionID)
}

func ParseKiloSQLiteVirtualPath(
	sourcePath string,
) (dbPath, sessionID string, ok bool) {
	return parseOpenCodeFormatVirtualPath(kiloFmt.dbName, sourcePath)
}

// ResolveMiMoCodeSource detects whether a MiMoCode root is using
// file-backed storage (storage/session_diff) or SQLite storage.
func ResolveMiMoCodeSource(root string) OpenCodeSource {
	return resolveOpenCodeFormatSource(mimoFmt, root)
}

func DiscoverMiMoCodeSessions(root string) []DiscoveredFile {
	return discoverOpenCodeFormatSessions(mimoFmt, root)
}

func FindMiMoCodeSourceFile(root, sessionID string) string {
	return findOpenCodeFormatSourceFile(mimoFmt, root, sessionID)
}

func MiMoCodeStorageSessionIDs(root string) map[string]struct{} {
	return openCodeFormatStorageSessionIDs(mimoFmt, root)
}

func ResolveMiMoCodeWatchRoots(root string) []string {
	return resolveOpenCodeFormatWatchRoots(mimoFmt, root)
}

func MiMoCodeSQLiteVirtualPath(dbPath, sessionID string) string {
	return OpenCodeSQLiteVirtualPath(dbPath, sessionID)
}

func ParseMiMoCodeSQLiteVirtualPath(
	sourcePath string,
) (dbPath, sessionID string, ok bool) {
	return parseOpenCodeFormatVirtualPath(mimoFmt.dbName, sourcePath)
}

// ResolveCodexShallowWatchRoots returns directories that should be watched
// shallowly (root only) for live Codex updates, in addition to the recursive
// watch on the configured sessions root. Codex writes title renames to
// session_index.jsonl in the parent of sessions/ and archived_sessions/, so
// that parent must be watched for renames to surface without waiting for the
// periodic sync. A shallow watch avoids recursing over unrelated Codex state
// such as logs.
func ResolveCodexShallowWatchRoots(root string) []string {
	parent := filepath.Dir(root)
	if parent == "" || parent == "." || parent == root {
		return nil
	}
	return []string{parent}
}

// DiscoverClaudeProjects finds all project directories under the
// Claude projects dir and returns their JSONL session files.
func DiscoverClaudeProjects(projectsDir string) []DiscoveredFile {
	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		return nil
	}

	var files []DiscoveredFile
	for _, entry := range entries {
		if !isDirOrSymlink(entry, projectsDir) {
			continue
		}

		projDir := filepath.Join(projectsDir, entry.Name())
		sessionFiles, err := os.ReadDir(projDir)
		if err != nil {
			continue
		}

		for _, sf := range sessionFiles {
			if sf.IsDir() {
				continue
			}
			name := sf.Name()
			if !strings.HasSuffix(name, ".jsonl") {
				continue
			}
			stem := strings.TrimSuffix(name, ".jsonl")
			if strings.HasPrefix(stem, "agent-") {
				continue
			}
			files = append(files, DiscoveredFile{
				Path:    filepath.Join(projDir, name),
				Project: entry.Name(),
				Agent:   AgentClaude,
			})
		}

		// Scan session directories for subagent files. Claude workflow
		// tools group subagents under nested paths such as
		// subagents/workflows/<workflow-id>/agent-<id>.jsonl, so walk the
		// whole subagents tree instead of assuming transcripts are direct
		// children of subagents/.
		for _, sf := range sessionFiles {
			if !sf.IsDir() {
				continue
			}
			subagentsDir := filepath.Join(
				projDir, sf.Name(), "subagents",
			)
			_ = filepath.WalkDir(
				subagentsDir,
				func(path string, sub os.DirEntry, err error) error {
					if err != nil || sub.IsDir() {
						return nil
					}
					name := sub.Name()
					if !strings.HasPrefix(name, "agent-") ||
						!strings.HasSuffix(name, ".jsonl") {
						return nil
					}
					files = append(files, DiscoveredFile{
						Path:    path,
						Project: entry.Name(),
						Agent:   AgentClaude,
					})
					return nil
				},
			)
		}
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].Path < files[j].Path
	})
	return files
}

// DiscoverCodexSessions finds all Codex JSONL session files under
// either the standard year/month/day layout or a flat archived dir.
func DiscoverCodexSessions(sessionsDir string) []DiscoveredFile {
	var files []DiscoveredFile

	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		return nil
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if !isCodexSessionFilename(entry.Name()) {
			continue
		}
		files = append(files, DiscoveredFile{
			Path:  filepath.Join(sessionsDir, entry.Name()),
			Agent: AgentCodex,
		})
	}

	walkCodexDayDirs(sessionsDir, func(dayPath string) bool {
		entries, err := os.ReadDir(dayPath)
		if err != nil {
			return true
		}
		for _, sf := range entries {
			if sf.IsDir() {
				continue
			}
			if !isCodexSessionFilename(sf.Name()) {
				continue
			}
			files = append(files, DiscoveredFile{
				Path:  filepath.Join(dayPath, sf.Name()),
				Agent: AgentCodex,
			})
		}
		return true
	})

	sort.Slice(files, func(i, j int) bool {
		return files[i].Path < files[j].Path
	})
	return files
}

// FindClaudeSourceFile finds the original JSONL file for a Claude
// session ID by searching all project directories.
func FindClaudeSourceFile(
	projectsDir, sessionID string,
) string {
	if !IsValidSessionID(sessionID) {
		return ""
	}

	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		return ""
	}

	target := sessionID + ".jsonl"
	for _, entry := range entries {
		if !isDirOrSymlink(entry, projectsDir) {
			continue
		}
		candidate := filepath.Join(
			projectsDir, entry.Name(), target,
		)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}

	// Subagent files live under session directories:
	// <project>/<session>/subagents/**/agent-<id>.jsonl
	if strings.HasPrefix(sessionID, "agent-") {
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			projDir := filepath.Join(
				projectsDir, entry.Name(),
			)
			sessionDirs, err := os.ReadDir(projDir)
			if err != nil {
				continue
			}
			for _, sd := range sessionDirs {
				if !sd.IsDir() {
					continue
				}
				var found string
				subagentsDir := filepath.Join(
					projDir, sd.Name(), "subagents",
				)
				_ = filepath.WalkDir(
					subagentsDir,
					func(path string, d os.DirEntry, err error) error {
						if err != nil || d.IsDir() || d.Name() != target {
							return nil
						}
						found = path
						return filepath.SkipAll
					},
				)
				if found != "" {
					return found
				}
			}
		}
	}

	return ""
}

// FindCodexSourceFile finds a Codex session file by UUID.
// Prefers the standard year/month/day live path when present,
// then falls back to a flat archived dir entry.
func FindCodexSourceFile(sessionsDir, sessionID string) string {
	if !IsValidSessionID(sessionID) {
		return ""
	}

	var archived string
	entries, err := os.ReadDir(sessionsDir)
	if err == nil {
		for _, f := range entries {
			if f.IsDir() {
				continue
			}
			name := f.Name()
			if !isCodexSessionFilename(name) {
				continue
			}
			if extractUUIDFromRollout(name) == sessionID {
				archived = filepath.Join(sessionsDir, name)
				break
			}
		}
	}

	var live string
	walkCodexDayDirs(sessionsDir, func(dayPath string) bool {
		if live != "" {
			return false
		}
		entries, err := os.ReadDir(dayPath)
		if err != nil {
			return true
		}
		for _, f := range entries {
			if f.IsDir() {
				continue
			}
			name := f.Name()
			if !isCodexSessionFilename(name) {
				continue
			}
			if extractUUIDFromRollout(name) == sessionID {
				live = filepath.Join(dayPath, name)
				return false
			}
		}
		return true
	})
	if live != "" {
		return live
	}
	return archived
}

func isCodexSessionFilename(name string) bool {
	return strings.HasPrefix(name, "rollout-") &&
		strings.HasSuffix(name, ".jsonl")
}

// CodexSessionUUIDFromFilename extracts the canonical session UUID
// from a Codex rollout filename. Returns "" when the filename does
// not match Codex session naming.
func CodexSessionUUIDFromFilename(name string) string {
	if !isCodexSessionFilename(name) {
		return ""
	}
	return extractUUIDFromRollout(name)
}

// CodexLayout reports which on-disk layout a Codex session path uses.
type CodexLayout int

const (
	CodexLayoutUnknown CodexLayout = iota
	CodexLayoutArchivedFlat
	CodexLayoutDated
)

// CodexSessionPathInfo parses a Codex path relative to a configured
// root and reports whether it is a valid session path plus its layout
// and canonical session UUID.
func CodexSessionPathInfo(root, path string) (CodexLayout, string, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return CodexLayoutUnknown, "", false
	}
	sep := string(filepath.Separator)
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+sep) {
		return CodexLayoutUnknown, "", false
	}
	if !strings.HasSuffix(path, ".jsonl") {
		return CodexLayoutUnknown, "", false
	}
	parts := strings.Split(rel, sep)
	switch len(parts) {
	case 1:
		if !isCodexSessionFilename(parts[0]) {
			return CodexLayoutUnknown, "", false
		}
		return CodexLayoutArchivedFlat,
			CodexSessionUUIDFromFilename(parts[0]), true
	case 4:
		if !IsDigits(parts[0]) || !IsDigits(parts[1]) || !IsDigits(parts[2]) {
			return CodexLayoutUnknown, "", false
		}
		if !isCodexSessionFilename(parts[3]) {
			return CodexLayoutUnknown, "", false
		}
		return CodexLayoutDated,
			CodexSessionUUIDFromFilename(parts[3]), true
	default:
		return CodexLayoutUnknown, "", false
	}
}

// walkCodexDayDirs traverses a Codex sessions directory with
// year/month/day structure, calling fn for each valid day directory.
// fn returns false to stop traversal.
func walkCodexDayDirs(
	root string, fn func(dayPath string) bool,
) {
	years, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, year := range years {
		if !year.IsDir() || !IsDigits(year.Name()) {
			continue
		}
		yearPath := filepath.Join(root, year.Name())
		months, err := os.ReadDir(yearPath)
		if err != nil {
			continue
		}
		for _, month := range months {
			if !month.IsDir() || !IsDigits(month.Name()) {
				continue
			}
			monthPath := filepath.Join(yearPath, month.Name())
			days, err := os.ReadDir(monthPath)
			if err != nil {
				continue
			}
			for _, day := range days {
				if !day.IsDir() || !IsDigits(day.Name()) {
					continue
				}
				if !fn(filepath.Join(monthPath, day.Name())) {
					return
				}
			}
		}
	}
}

// extractUUIDFromRollout extracts the UUID from a Codex filename
// like rollout-{timestamp}-{uuid}.jsonl using regex matching on the
// standard 8-4-4-4-12 hex format.
func extractUUIDFromRollout(filename string) string {
	stem := strings.TrimSuffix(filename, ".jsonl")
	match := uuidRe.FindStringSubmatch(stem)
	if len(match) < 2 {
		return ""
	}
	return match[1]
}

// IsDigits reports whether s is non-empty and contains only
// Unicode digit characters.
func IsDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

// IsValidSessionID reports whether id contains only
// alphanumeric characters, dashes, and underscores.
func IsValidSessionID(id string) bool {
	if id == "" {
		return false
	}
	for _, c := range id {
		if !isAlphanumOrDashUnderscore(c) {
			return false
		}
	}
	return true
}

func isAlphanumOrDashUnderscore(c rune) bool {
	return isAlphanum(c) ||
		c == '-' || c == '_'
}

func isAlphanum(c rune) bool {
	return (c >= 'a' && c <= 'z') ||
		(c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9')
}

func isValidAmpThreadID(id string) bool {
	if !strings.HasPrefix(id, "T-") {
		return false
	}
	if len(id) == len("T-") {
		return false
	}
	if !isAlphanum(rune(id[len("T-")])) {
		return false
	}
	return IsValidSessionID(id)
}

// IsAmpThreadFileName reports whether name matches the Amp
// thread file pattern (T-*.json).
func IsAmpThreadFileName(name string) bool {
	if !strings.HasSuffix(name, ".json") {
		return false
	}
	return isValidAmpThreadID(strings.TrimSuffix(name, ".json"))
}

func isGeminiSessionFilename(name string) bool {
	return strings.HasPrefix(name, "session-") &&
		(strings.HasSuffix(name, ".json") ||
			strings.HasSuffix(name, ".jsonl"))
}

// DiscoverGeminiSessions finds all Gemini session files under
// the Gemini directory (~/.gemini/tmp/*/chats/session-*).
func DiscoverGeminiSessions(
	geminiDir string,
) []DiscoveredFile {
	if geminiDir == "" {
		return nil
	}

	tmpDir := filepath.Join(geminiDir, "tmp")
	hashDirs, err := os.ReadDir(tmpDir)
	if err != nil {
		return nil
	}

	projectMap := BuildGeminiProjectMap(geminiDir)

	var files []DiscoveredFile
	for _, hd := range hashDirs {
		if !isDirOrSymlink(hd, tmpDir) {
			continue
		}
		hash := hd.Name()
		chatsDir := filepath.Join(tmpDir, hash, "chats")
		entries, err := os.ReadDir(chatsDir)
		if err != nil {
			continue
		}

		project := ResolveGeminiProject(hash, projectMap)

		for _, sf := range entries {
			if sf.IsDir() {
				continue
			}
			name := sf.Name()
			if !isGeminiSessionFilename(name) {
				continue
			}
			files = append(files, DiscoveredFile{
				Path:    filepath.Join(chatsDir, name),
				Project: project,
				Agent:   AgentGemini,
			})
		}
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].Path < files[j].Path
	})
	return files
}

// FindGeminiSourceFile locates a Gemini session file by its
// session UUID. Searches all project hash directories.
func FindGeminiSourceFile(
	geminiDir, sessionID string,
) string {
	if geminiDir == "" || !IsValidSessionID(sessionID) ||
		len(sessionID) < 8 {
		return ""
	}

	tmpDir := filepath.Join(geminiDir, "tmp")
	hashDirs, err := os.ReadDir(tmpDir)
	if err != nil {
		return ""
	}

	for _, hd := range hashDirs {
		if !isDirOrSymlink(hd, tmpDir) {
			continue
		}
		chatsDir := filepath.Join(tmpDir, hd.Name(), "chats")
		entries, err := os.ReadDir(chatsDir)
		if err != nil {
			continue
		}
		for _, sf := range entries {
			if sf.IsDir() {
				continue
			}
			name := sf.Name()
			if !isGeminiSessionFilename(name) {
				continue
			}
			if strings.Contains(name, sessionID[:8]) {
				path := filepath.Join(chatsDir, name)
				if confirmGeminiSessionID(
					path, sessionID,
				) {
					return path
				}
			}
		}
	}
	return ""
}

// confirmGeminiSessionID reads the sessionId field from a
// Gemini file to confirm it matches the expected ID.
func confirmGeminiSessionID(
	path, sessionID string,
) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return GeminiSessionID(data) == sessionID
}

// DiscoverCursorSessions finds all agent transcript files under
// the Cursor projects dir (<projectsDir>/<project>/agent-transcripts/<uuid>.txt).
// All discovered paths are validated to resolve within the
// canonical projectsDir, preventing symlink escapes.
// cursorAddSeen inserts a transcript path into the seen map,
// preferring .jsonl over .txt when both exist for the same stem.
func cursorAddSeen(
	seen map[string]string, name, fullPath string,
) {
	stem := strings.TrimSuffix(name, filepath.Ext(name))
	if prev, ok := seen[stem]; ok {
		if strings.HasSuffix(prev, ".txt") &&
			strings.HasSuffix(name, ".jsonl") {
			seen[stem] = fullPath
		}
		return
	}
	seen[stem] = fullPath
}

func DiscoverCursorSessions(
	projectsDir string,
) []DiscoveredFile {
	if projectsDir == "" {
		return nil
	}

	// Canonicalize root once for containment checks.
	resolvedRoot, err := filepath.EvalSymlinks(projectsDir)
	if err != nil {
		return nil
	}

	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		return nil
	}

	var files []DiscoveredFile
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		// Reject symlinked project directory entries.
		if entry.Type()&os.ModeSymlink != 0 {
			continue
		}

		transcriptsDir := filepath.Join(
			projectsDir, entry.Name(), "agent-transcripts",
		)

		// Verify the transcripts directory resolves within
		// the canonical root.
		resolvedDir, err := filepath.EvalSymlinks(
			transcriptsDir,
		)
		if err != nil {
			continue
		}
		if !isContainedIn(resolvedDir, resolvedRoot) {
			continue
		}

		transcripts, err := os.ReadDir(transcriptsDir)
		if err != nil {
			continue
		}

		project := DecodeCursorProjectDir(entry.Name())
		if project == "" {
			project = "unknown"
		}

		// Collect valid transcripts, deduping by basename
		// stem. When both .jsonl and .txt exist for the
		// same session, prefer .jsonl.
		//
		// Cursor uses two layouts:
		//   flat:   agent-transcripts/<uuid>.{txt,jsonl}
		//   nested: agent-transcripts/<uuid>/<uuid>.{txt,jsonl}
		seen := make(map[string]string) // stem -> path
		for _, sf := range transcripts {
			if !sf.IsDir() {
				// Flat layout: file directly in
				// agent-transcripts/.
				name := sf.Name()
				if !IsCursorTranscriptExt(name) {
					continue
				}
				fullPath := filepath.Join(
					transcriptsDir, name,
				)
				if !IsRegularFile(fullPath) {
					continue
				}
				cursorAddSeen(seen, name, fullPath)
				continue
			}

			// Nested layout: agent-transcripts/<uuid>/
			// containing <uuid>.{txt,jsonl}.
			subDir := filepath.Join(
				transcriptsDir, sf.Name(),
			)
			subEntries, err := os.ReadDir(subDir)
			if err != nil {
				continue
			}
			dirName := sf.Name()
			for _, sub := range subEntries {
				if sub.IsDir() {
					continue
				}
				name := sub.Name()
				if !IsCursorTranscriptExt(name) {
					continue
				}
				// Only accept files whose stem matches
				// the parent directory name, e.g.
				// <uuid>/<uuid>.jsonl.
				stem := strings.TrimSuffix(
					name, filepath.Ext(name),
				)
				if stem != dirName {
					continue
				}
				fullPath := filepath.Join(
					subDir, name,
				)
				if !IsRegularFile(fullPath) {
					continue
				}
				cursorAddSeen(seen, name, fullPath)
			}
		}
		for _, path := range seen {
			files = append(files, DiscoveredFile{
				Path:    path,
				Project: project,
				Agent:   AgentCursor,
			})
		}
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].Path < files[j].Path
	})
	return files
}

// FindCursorSourceFile finds a Cursor transcript file by
// session UUID. Prefers .jsonl over .txt.
func FindCursorSourceFile(
	projectsDir, sessionID string,
) string {
	if projectsDir == "" || !IsValidSessionID(sessionID) {
		return ""
	}

	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		return ""
	}

	resolvedRoot, err := filepath.EvalSymlinks(projectsDir)
	if err != nil {
		return ""
	}

	for _, ext := range []string{".jsonl", ".txt"} {
		target := sessionID + ext
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			// Nested layout first (matches discovery
			// precedence), then flat layout.
			candidates := []string{
				filepath.Join(
					projectsDir, entry.Name(),
					"agent-transcripts", sessionID, target,
				),
				filepath.Join(
					projectsDir, entry.Name(),
					"agent-transcripts", target,
				),
			}
			for _, candidate := range candidates {
				if !IsRegularFile(candidate) {
					continue
				}
				resolved, err := filepath.EvalSymlinks(
					candidate,
				)
				if err != nil {
					continue
				}
				rel, err := filepath.Rel(
					resolvedRoot, resolved,
				)
				sep := string(filepath.Separator)
				if err != nil || rel == ".." ||
					strings.HasPrefix(rel, ".."+sep) {
					continue
				}
				return candidate
			}
		}
	}
	return ""
}

// geminiProjectsFile holds the structure of
// ~/.gemini/projects.json.
type geminiProjectsFile struct {
	Projects map[string]string `json:"projects"`
}

// geminiTrustedFoldersFile holds the structure of
// ~/.gemini/trustedFolders.json.
type geminiTrustedFoldersFile struct {
	TrustedFolders []string `json:"trustedFolders"`
}

// buildGeminiProjectMap reads ~/.gemini/projects.json and
// ~/.gemini/trustedFolders.json to build a map from directory
// name to resolved project name.
// BuildGeminiProjectMap reads Gemini config files and returns
// a map from directory name to resolved project name.
func BuildGeminiProjectMap(
	geminiDir string,
) map[string]string {
	result := make(map[string]string)

	data, err := os.ReadFile(
		filepath.Join(geminiDir, "projects.json"),
	)
	if err == nil {
		var pf geminiProjectsFile
		if err := json.Unmarshal(data, &pf); err == nil {
			addProjectPaths(result, pf.Projects)
		}
	}

	tfData, err := os.ReadFile(
		filepath.Join(geminiDir, "trustedFolders.json"),
	)
	if err == nil {
		var tf geminiTrustedFoldersFile
		if err := json.Unmarshal(tfData, &tf); err == nil {
			paths := make(
				map[string]string, len(tf.TrustedFolders),
			)
			for _, p := range tf.TrustedFolders {
				paths[p] = ""
			}
			addProjectPaths(result, paths)
		}
	}

	return result
}

// addProjectPaths adds hash and name entries for the given
// absolute paths.
func addProjectPaths(
	result map[string]string,
	paths map[string]string,
) {
	sorted := make([]string, 0, len(paths))
	for absPath := range paths {
		sorted = append(sorted, absPath)
	}
	sort.Strings(sorted)

	for _, absPath := range sorted {
		name := paths[absPath]
		project := ExtractProjectFromCwd(absPath)
		if project == "" {
			project = "unknown"
		}
		hash := geminiPathHash(absPath)
		if _, exists := result[hash]; !exists {
			result[hash] = project
		}
		if name != "" {
			if _, exists := result[name]; !exists {
				result[name] = project
			}
		}
	}
}

// geminiPathHash computes the SHA-256 hex hash of a path,
// matching Gemini CLI's project hash algorithm.
func geminiPathHash(path string) string {
	h := sha256.Sum256([]byte(path))
	return fmt.Sprintf("%x", h)
}

// isHexHash reports whether s is a 64-character lowercase hex
// string (i.e. a SHA-256 hash).
func isHexHash(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// resolveGeminiProject maps a tmp/ subdirectory name to a
// project name.
// ResolveGeminiProject maps a tmp/ subdirectory name to a
// project name using the project map.
func ResolveGeminiProject(
	dirName string,
	projectMap map[string]string,
) string {
	if p := projectMap[dirName]; p != "" {
		return p
	}
	if isHexHash(dirName) {
		return "unknown"
	}
	return NormalizeName(dirName)
}

// DiscoverAmpSessions finds all thread JSON files under
// the Amp threads directory (~/.local/share/amp/threads/T-*.json).
func DiscoverAmpSessions(threadsDir string) []DiscoveredFile {
	if threadsDir == "" {
		return nil
	}

	entries, err := os.ReadDir(threadsDir)
	if err != nil {
		return nil
	}

	var files []DiscoveredFile
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !IsAmpThreadFileName(name) {
			continue
		}
		files = append(files, DiscoveredFile{
			Path:  filepath.Join(threadsDir, name),
			Agent: AgentAmp,
		})
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].Path < files[j].Path
	})
	return files
}

// FindAmpSourceFile locates an Amp thread file by its raw
// thread ID (without the "amp:" prefix).
func FindAmpSourceFile(threadsDir, threadID string) string {
	if threadsDir == "" || !isValidAmpThreadID(threadID) {
		return ""
	}
	candidate := filepath.Join(threadsDir, threadID+".json")
	if _, err := os.Stat(candidate); err == nil {
		return candidate
	}
	return ""
}

// DiscoverCopilotSessions finds all JSONL files under
// <copilotDir>/session-state/. Supports both bare format
// (<uuid>.jsonl) and directory format (<uuid>/events.jsonl).
func DiscoverCopilotSessions(
	copilotDir string,
) []DiscoveredFile {
	if copilotDir == "" {
		return nil
	}

	stateDir := filepath.Join(copilotDir, "session-state")
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		return nil
	}

	dirs := make(map[string]struct{})
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		eventsPath := filepath.Join(
			stateDir, entry.Name(), "events.jsonl",
		)
		if _, err := os.Stat(eventsPath); err == nil {
			dirs[entry.Name()] = struct{}{}
		}
	}

	var files []DiscoveredFile
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			candidate := filepath.Join(
				stateDir, name, "events.jsonl",
			)
			if _, err := os.Stat(candidate); err == nil {
				files = append(files, DiscoveredFile{
					Path:  candidate,
					Agent: AgentCopilot,
				})
			}
			continue
		}
		if stem, ok := strings.CutSuffix(name, ".jsonl"); ok {
			if _, dup := dirs[stem]; dup {
				continue
			}
			files = append(files, DiscoveredFile{
				Path:  filepath.Join(stateDir, name),
				Agent: AgentCopilot,
			})
		}
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].Path < files[j].Path
	})
	return files
}

// FindCopilotSourceFile locates a Copilot session file by
// UUID. Checks both bare (<uuid>.jsonl) and directory
// (<uuid>/events.jsonl) layouts.
func FindCopilotSourceFile(
	copilotDir, rawID string,
) string {
	if copilotDir == "" || !IsValidSessionID(rawID) {
		return ""
	}

	stateDir := filepath.Join(copilotDir, "session-state")

	dirFmt := filepath.Join(stateDir, rawID, "events.jsonl")
	if _, err := os.Stat(dirFmt); err == nil {
		return dirFmt
	}

	bare := filepath.Join(stateDir, rawID+".jsonl")
	if _, err := os.Stat(bare); err == nil {
		return bare
	}

	return ""
}

// IsPiSessionFile reads the first non-blank line of path and returns true
// when the JSON type field equals "session". The scanner buffer grows up to
// 64 MiB to match parser.maxLineSize. Leading blank lines are skipped to
// match lineReader behavior.
func IsPiSessionFile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 0, 64*1024), 64*1024*1024) // up to 64 MiB, matches parser.maxLineSize
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" {
			continue
		}
		return gjson.Get(line, "type").Str == "session"
	}
	return false
}

// DiscoverPiSessions finds JSONL files under piDir that are
// valid pi sessions. Pi sessions live in
// <piDir>/<encoded-cwd>/<session-id>.jsonl; the encoded-cwd
// format is ambiguous between pi versions, so discovery
// validates by reading the session header rather than parsing
// the directory name. Project is left empty so ParsePiSession
// can derive it from the header cwd field.
func DiscoverPiSessions(piDir string) []DiscoveredFile {
	return discoverPiLikeSessions(piDir, AgentPi)
}

// DiscoverOMPSessions finds JSONL files under an OhMyPi session root.
// OMP uses the same layout and file format as Pi, rooted by default at
// ~/.omp/agent/sessions.
func DiscoverOMPSessions(ompDir string) []DiscoveredFile {
	return discoverPiLikeSessions(ompDir, AgentOMP)
}

func discoverPiLikeSessions(piDir string, agent AgentType) []DiscoveredFile {
	if piDir == "" {
		return nil
	}
	entries, err := os.ReadDir(piDir)
	if err != nil {
		return nil
	}
	var files []DiscoveredFile
	for _, entry := range entries {
		if !isDirOrSymlink(entry, piDir) {
			continue
		}
		cwdDir := filepath.Join(piDir, entry.Name())
		sessionFiles, err := os.ReadDir(cwdDir)
		if err != nil {
			continue
		}
		for _, sf := range sessionFiles {
			if sf.IsDir() {
				continue
			}
			if !strings.HasSuffix(sf.Name(), ".jsonl") {
				continue
			}
			path := filepath.Join(cwdDir, sf.Name())
			if !IsPiSessionFile(path) {
				continue
			}
			files = append(files, DiscoveredFile{
				Path:  path,
				Agent: agent,
				// Project intentionally empty; ParsePiSession
				// derives project from the header cwd field.
			})
		}
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].Path < files[j].Path
	})
	return files
}

// FindPiSourceFile finds the original JSONL file for a pi
// session ID by searching all encoded-cwd subdirectories
// under piDir for a file named <sessionID>.jsonl.
func FindPiSourceFile(piDir, sessionID string) string {
	return findPiLikeSourceFile(piDir, sessionID)
}

// FindOMPSourceFile finds the original JSONL file for an OMP session ID.
func FindOMPSourceFile(ompDir, sessionID string) string {
	return findPiLikeSourceFile(ompDir, sessionID)
}

func findPiLikeSourceFile(piDir, sessionID string) string {
	if piDir == "" || !IsValidSessionID(sessionID) {
		return ""
	}
	entries, err := os.ReadDir(piDir)
	if err != nil {
		return ""
	}
	target := sessionID + ".jsonl"
	for _, entry := range entries {
		if !isDirOrSymlink(entry, piDir) {
			continue
		}
		candidate := filepath.Join(piDir, entry.Name(), target)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return ""
}

// isRegularFile returns true if path exists and is a regular
// file (not a symlink, directory, or other special file).
// IsRegularFile reports whether path is a regular file (not
// a symlink, directory, or special file).
func IsRegularFile(path string) bool {
	info, err := os.Lstat(path)
	if err != nil {
		return false
	}
	return info.Mode().IsRegular()
}

// isCursorTranscriptExt returns true if the filename has a
// recognized Cursor transcript extension (.txt or .jsonl).
// IsCursorTranscriptExt reports whether the filename has a
// recognized Cursor transcript extension (.txt or .jsonl).
func IsCursorTranscriptExt(name string) bool {
	return strings.HasSuffix(name, ".txt") ||
		strings.HasSuffix(name, ".jsonl")
}

// isContainedIn returns true if child is a path strictly
// under root. Both paths must be absolute / canonical.
func isContainedIn(child, root string) bool {
	rel, err := filepath.Rel(root, child)
	if err != nil {
		return false
	}
	return rel != "." && rel != ".." &&
		!strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// DiscoverVSCodeCopilotSessions traverses the VSCode
// workspaceStorage directory to find chatSessions/*.json
// and *.jsonl files. When both formats exist for the same
// session UUID, the .jsonl file takes priority.
// It also checks globalStorage/emptyWindowChatSessions.
// The vscodeUserDir should point to e.g.
//
//	~/Library/Application Support/Code/User (macOS)
//	~/.config/Code/User (Linux)
func DiscoverVSCodeCopilotSessions(
	vscodeUserDir string,
) []DiscoveredFile {
	if vscodeUserDir == "" {
		return nil
	}

	var files []DiscoveredFile

	// 1. Scan workspaceStorage/<hash>/chatSessions/*.{json,jsonl}
	wsDir := filepath.Join(vscodeUserDir, "workspaceStorage")
	hashDirs, err := os.ReadDir(wsDir)
	if err == nil {
		for _, entry := range hashDirs {
			if !entry.IsDir() {
				continue
			}

			hashPath := filepath.Join(wsDir, entry.Name())
			chatDir := filepath.Join(hashPath, "chatSessions")
			sessionFiles, err := os.ReadDir(chatDir)
			if err != nil {
				continue
			}

			// Read workspace.json to get project name
			project := ReadVSCodeWorkspaceManifest(hashPath)
			if project == "" {
				project = "unknown"
			}

			files = append(files,
				discoverVSCodeSessionFiles(
					chatDir, sessionFiles, project,
				)...,
			)
		}
	}

	// 2. Scan globalStorage/emptyWindowChatSessions/*.{json,jsonl}
	for _, subdir := range []string{
		"globalStorage/emptyWindowChatSessions",
		"globalStorage/transferredChatSessions",
	} {
		globalDir := filepath.Join(vscodeUserDir, subdir)
		globalFiles, err := os.ReadDir(globalDir)
		if err != nil {
			continue
		}
		files = append(files,
			discoverVSCodeSessionFiles(
				globalDir, globalFiles, "empty-window",
			)...,
		)
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].Path < files[j].Path
	})
	return files
}

// discoverVSCodeSessionFiles collects .json and .jsonl
// session files from a directory, preferring .jsonl when
// both exist for the same UUID.
func discoverVSCodeSessionFiles(
	dir string, entries []os.DirEntry, project string,
) []DiscoveredFile {
	// Collect UUIDs that have .jsonl files
	hasJSONL := make(map[string]bool)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if uuid, ok := strings.CutSuffix(
			e.Name(), ".jsonl",
		); ok {
			hasJSONL[uuid] = true
		}
	}

	var files []DiscoveredFile
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()

		if strings.HasSuffix(name, ".jsonl") {
			files = append(files, DiscoveredFile{
				Path:    filepath.Join(dir, name),
				Project: project,
				Agent:   AgentVSCodeCopilot,
			})
		} else if uuid, ok := strings.CutSuffix(name, ".json"); ok {
			// Skip .json if a .jsonl exists for the same UUID
			if hasJSONL[uuid] {
				continue
			}
			files = append(files, DiscoveredFile{
				Path:    filepath.Join(dir, name),
				Project: project,
				Agent:   AgentVSCodeCopilot,
			})
		}
	}
	return files
}

// FindVSCodeCopilotSourceFile locates a VSCode Copilot
// session file by UUID (.jsonl preferred over .json).
func FindVSCodeCopilotSourceFile(
	vscodeUserDir, rawID string,
) string {
	if vscodeUserDir == "" || !IsValidSessionID(rawID) {
		return ""
	}

	// Search through workspaceStorage
	wsDir := filepath.Join(vscodeUserDir, "workspaceStorage")
	hashDirs, err := os.ReadDir(wsDir)
	if err == nil {
		for _, entry := range hashDirs {
			if !entry.IsDir() {
				continue
			}
			base := filepath.Join(
				wsDir, entry.Name(), "chatSessions",
			)
			// Prefer .jsonl
			for _, ext := range []string{".jsonl", ".json"} {
				candidate := filepath.Join(
					base, rawID+ext,
				)
				if _, err := os.Stat(candidate); err == nil {
					return candidate
				}
			}
		}
	}

	// Check global dirs
	for _, subdir := range []string{
		"globalStorage/emptyWindowChatSessions",
		"globalStorage/transferredChatSessions",
	} {
		base := filepath.Join(vscodeUserDir, subdir)
		for _, ext := range []string{".jsonl", ".json"} {
			candidate := filepath.Join(base, rawID+ext)
			if _, err := os.Stat(candidate); err == nil {
				return candidate
			}
		}
	}

	return ""
}

// DiscoverVisualStudioCopilotSessions finds Visual Studio Copilot
// trace files under the configured traces directory.
func DiscoverVisualStudioCopilotSessions(vsRoot string) []DiscoveredFile {
	if vsRoot == "" {
		return nil
	}
	entries, err := os.ReadDir(vsRoot)
	if err != nil {
		return nil
	}
	files := discoverVisualStudioCopilotSessionFiles(vsRoot, entries)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files
}

// discoverVisualStudioCopilotSessionFiles emits one work item per conversation
// found across the trace files in a directory. A single physical trace file
// can hold spans for several conversations, and one conversation can be split
// across rotating trace files, so each conversation is keyed independently and
// represented by the latest trace file that contains it. The work item path is
// a <traceFile>#<conversationID> virtual path so the parser can re-gather that
// conversation's spans from all sibling files.
func discoverVisualStudioCopilotSessionFiles(
	dir string, entries []os.DirEntry,
) []DiscoveredFile {
	type candidate struct {
		path  string
		mtime time.Time
	}
	bestByConversation := map[string]candidate{}
	var unreadable []DiscoveredFile
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".jsonl") ||
			!strings.Contains(name, "_VSGitHubCopilot_traces") {
			continue
		}
		path := filepath.Join(dir, name)
		mtime := time.Time{}
		if info, err := entry.Info(); err == nil {
			mtime = info.ModTime()
		}
		ids, err := VisualStudioCopilotFileConversationIDs(path)
		if err != nil {
			// Enqueue the physical file so the sync worker surfaces the
			// read failure instead of silently dropping every
			// conversation it might contain.
			unreadable = append(unreadable, DiscoveredFile{
				Path:    path,
				Project: "visualstudio",
				Agent:   AgentVSCopilot,
			})
			continue
		}
		for _, id := range ids {
			current, ok := bestByConversation[id]
			if !ok || mtime.After(current.mtime) ||
				(mtime.Equal(current.mtime) && path > current.path) {
				bestByConversation[id] = candidate{path: path, mtime: mtime}
			}
		}
	}
	files := make([]DiscoveredFile, 0, len(bestByConversation)+len(unreadable))
	for id, c := range bestByConversation {
		files = append(files, DiscoveredFile{
			Path:    VisualStudioCopilotVirtualPath(c.path, id),
			Project: "visualstudio",
			Agent:   AgentVSCopilot,
		})
	}
	files = append(files, unreadable...)
	return files
}

// FindVisualStudioCopilotSourceFile locates a Visual Studio Copilot
// trace file by conversation UUID.
func FindVisualStudioCopilotSourceFile(vsRoot, rawID string) string {
	if vsRoot == "" || !IsValidSessionID(rawID) {
		return ""
	}
	return findVisualStudioCopilotTraceSourceFile(vsRoot, rawID)
}

func findVisualStudioCopilotTraceSourceFile(
	dir, rawID string,
) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	needle := `"gen_ai.conversation.id"`
	valueNeedle := `"stringValue":"` + rawID + `"`
	var matches []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".jsonl") ||
			!strings.Contains(name, "_VSGitHubCopilot_traces") {
			continue
		}
		path := filepath.Join(dir, name)
		if visualStudioCopilotTraceContains(path, needle, valueNeedle) {
			matches = append(matches, path)
		}
	}
	if len(matches) == 0 {
		return ""
	}
	sort.Strings(matches)
	// Return a conversation-scoped virtual path. The stored file_path is a
	// <traceFile>#<conversationID> key, and returning the bare trace file would
	// let a single-session resync enumerate and rewrite every conversation in
	// that trace rather than only the requested one.
	return VisualStudioCopilotVirtualPath(matches[len(matches)-1], rawID)
}

func visualStudioCopilotTraceContains(
	path, keyNeedle, valueNeedle string,
) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 256*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, keyNeedle) &&
			strings.Contains(line, valueNeedle) {
			return true
		}
	}
	return false
}

// DiscoverOpenClawSessions finds all JSONL session files under the
// OpenClaw agents directory. The directory structure is:
// <agentsDir>/<agentId>/sessions/<sessionId>.jsonl
//
// When both active (.jsonl) and archived (.jsonl.deleted.*,
// .jsonl.full.bak, .jsonl.reset.*) files exist for the same
// logical session ID, only one file is returned per session:
// the active .jsonl file is preferred; if absent, the newest
// archived file (by filename, which embeds a timestamp, or by
// file mtime as a fallback) is chosen.
func DiscoverOpenClawSessions(agentsDir string) []DiscoveredFile {
	if agentsDir == "" {
		return nil
	}

	// Each agent has its own subdirectory.
	agentEntries, err := os.ReadDir(agentsDir)
	if err != nil {
		return nil
	}

	var files []DiscoveredFile
	for _, agentEntry := range agentEntries {
		if !isDirOrSymlink(agentEntry, agentsDir) {
			continue
		}
		if !IsValidSessionID(agentEntry.Name()) {
			continue
		}

		sessionsDir := filepath.Join(
			agentsDir, agentEntry.Name(), "sessions",
		)
		entries, err := os.ReadDir(sessionsDir)
		if err != nil {
			continue
		}

		// Deduplicate by logical session ID within each
		// agent's sessions directory.
		best := make(map[string]os.DirEntry) // sessionID -> best entry
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			name := entry.Name()
			if !IsOpenClawSessionFile(name) {
				continue
			}
			sid := OpenClawSessionID(name)
			prev, exists := best[sid]
			if !exists {
				best[sid] = entry
				continue
			}
			best[sid] = bestOpenClawEntry(prev, entry)
		}

		for _, entry := range best {
			files = append(files, DiscoveredFile{
				Path: filepath.Join(
					sessionsDir, entry.Name(),
				),
				Agent: AgentOpenClaw,
			})
		}
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].Path < files[j].Path
	})
	return files
}

// bestOpenClawEntry returns the preferred entry when two files
// share the same logical session ID. Active .jsonl files always
// win. Among archived files, the one with the newest embedded
// timestamp wins; when no timestamp is parseable, mtime is used.
func bestOpenClawEntry(a, b os.DirEntry) os.DirEntry {
	aActive := strings.HasSuffix(a.Name(), ".jsonl")
	bActive := strings.HasSuffix(b.Name(), ".jsonl")
	if aActive && !bActive {
		return a
	}
	if bActive && !aActive {
		return b
	}
	aTime := openClawArchiveTime(a)
	bTime := openClawArchiveTime(b)
	if !aTime.IsZero() && !bTime.IsZero() {
		if bTime.After(aTime) {
			return b
		}
		return a
	}
	if !aTime.IsZero() {
		return a
	}
	if !bTime.IsZero() {
		return b
	}
	ai, errA := a.Info()
	bi, errB := b.Info()
	if errA == nil && errB == nil &&
		bi.ModTime().After(ai.ModTime()) {
		return b
	}
	return a
}

// openClawArchiveTime extracts the timestamp embedded in an
// OpenClaw archive filename suffix (e.g. ".deleted.2026-02-19T08-59-24.951Z").
func openClawArchiveTime(e os.DirEntry) time.Time {
	name := e.Name()
	idx := strings.Index(name, ".jsonl.")
	if idx <= 0 {
		return time.Time{}
	}
	suffix := name[idx+len(".jsonl."):]
	// suffix is e.g. "deleted.2026-02-19T08-59-24.951Z" or "full.bak"
	_, tsStr, ok := strings.Cut(suffix, ".")
	if !ok {
		return time.Time{}
	}
	// Convert dash-separated time back to colons: 08-59-24 → 08:59:24
	if tIdx := strings.IndexByte(tsStr, 'T'); tIdx >= 0 {
		datePart := tsStr[:tIdx+1]
		timePart := tsStr[tIdx+1:]
		// Only replace first two dashes in time portion (hh-mm-ss)
		timePart = strings.Replace(timePart, "-", ":", 1)
		timePart = strings.Replace(timePart, "-", ":", 1)
		tsStr = datePart + timePart
	}
	t, err := time.Parse("2006-01-02T15:04:05.000Z", tsStr)
	if err != nil {
		t, err = time.Parse("2006-01-02T15:04:05Z", tsStr)
	}
	if err != nil {
		return time.Time{}
	}
	return t
}

// FindOpenClawSourceFile locates an OpenClaw session file by its
// raw ID (without the "openclaw:" prefix). The raw ID has the
// format "<agentId>:<sessionId>", which directly maps to the
// file at <agentsDir>/<agentId>/sessions/<sessionId>.jsonl.
//
// If the active .jsonl file does not exist (archive-only session),
// the sessions directory is scanned for any archived file whose
// logical session ID matches. When multiple archived files match,
// the best candidate (newest by filename timestamp) is returned.
func FindOpenClawSourceFile(agentsDir, rawID string) string {
	if agentsDir == "" {
		return ""
	}

	// Split "agentId:sessionId" into its two parts.
	agentID, sessionID, ok := strings.Cut(rawID, ":")
	if !ok || !IsValidSessionID(agentID) ||
		!IsValidSessionID(sessionID) {
		return ""
	}

	sessionsDir := filepath.Join(
		agentsDir, agentID, "sessions",
	)

	// Fast path: the active .jsonl file exists.
	active := filepath.Join(sessionsDir, sessionID+".jsonl")
	if _, err := os.Stat(active); err == nil {
		return active
	}

	// Slow path: scan for archived files matching this session.
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		return ""
	}

	var best os.DirEntry
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !IsOpenClawSessionFile(name) {
			continue
		}
		if OpenClawSessionID(name) != sessionID {
			continue
		}
		if best == nil {
			best = entry
			continue
		}
		best = bestOpenClawEntry(best, entry)
	}
	if best != nil {
		return filepath.Join(sessionsDir, best.Name())
	}
	return ""
}

// DiscoverQClawSessions finds all JSONL session files under the
// QClaw agents directory. The directory structure is:
// <agentsDir>/<agentId>/sessions/<sessionId>.jsonl
//
// When both active (.jsonl) and archived (.jsonl.deleted.*,
// .jsonl.full.bak, .jsonl.reset.*) files exist for the same
// logical session ID, only one file is returned per session:
// the active .jsonl file is preferred; if absent, the newest
// archived file (by filename, which embeds a timestamp, or by
// file mtime as a fallback) is chosen.
func DiscoverQClawSessions(agentsDir string) []DiscoveredFile {
	if agentsDir == "" {
		return nil
	}

	// Each agent has its own subdirectory.
	agentEntries, err := os.ReadDir(agentsDir)
	if err != nil {
		return nil
	}

	var files []DiscoveredFile
	for _, agentEntry := range agentEntries {
		if !isDirOrSymlink(agentEntry, agentsDir) {
			continue
		}
		if !IsValidSessionID(agentEntry.Name()) {
			continue
		}

		sessionsDir := filepath.Join(
			agentsDir, agentEntry.Name(), "sessions",
		)
		entries, err := os.ReadDir(sessionsDir)
		if err != nil {
			continue
		}

		// Deduplicate by logical session ID within each
		// agent's sessions directory.
		best := make(map[string]os.DirEntry) // sessionID -> best entry
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			name := entry.Name()
			if !IsQClawSessionFile(name) {
				continue
			}
			sid := QClawSessionID(name)
			prev, exists := best[sid]
			if !exists {
				best[sid] = entry
				continue
			}
			best[sid] = bestQClawEntry(prev, entry)
		}

		for _, entry := range best {
			files = append(files, DiscoveredFile{
				Path: filepath.Join(
					sessionsDir, entry.Name(),
				),
				Agent: AgentQClaw,
			})
		}
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].Path < files[j].Path
	})
	return files
}

// bestQClawEntry returns the preferred entry when two files
// share the same logical session ID. Active .jsonl files always
// win. Among archived files, the one with the newest embedded
// timestamp wins; when no timestamp is parseable, mtime is used.
func bestQClawEntry(a, b os.DirEntry) os.DirEntry {
	aActive := strings.HasSuffix(a.Name(), ".jsonl")
	bActive := strings.HasSuffix(b.Name(), ".jsonl")
	if aActive && !bActive {
		return a
	}
	if bActive && !aActive {
		return b
	}
	aTime := qClawArchiveTime(a)
	bTime := qClawArchiveTime(b)
	if !aTime.IsZero() && !bTime.IsZero() {
		if bTime.After(aTime) {
			return b
		}
		return a
	}
	if !aTime.IsZero() {
		return a
	}
	if !bTime.IsZero() {
		return b
	}
	ai, errA := a.Info()
	bi, errB := b.Info()
	if errA == nil && errB == nil &&
		bi.ModTime().After(ai.ModTime()) {
		return b
	}
	return a
}

// qClawArchiveTime extracts the timestamp embedded in an
// QClaw archive filename suffix (e.g. ".deleted.2026-02-19T08-59-24.951Z").
func qClawArchiveTime(e os.DirEntry) time.Time {
	name := e.Name()
	idx := strings.Index(name, ".jsonl.")
	if idx <= 0 {
		return time.Time{}
	}
	suffix := name[idx+len(".jsonl."):]
	// suffix is e.g. "deleted.2026-02-19T08-59-24.951Z" or "full.bak"
	_, tsStr, ok := strings.Cut(suffix, ".")
	if !ok {
		return time.Time{}
	}
	// Convert dash-separated time back to colons: 08-59-24 → 08:59:24
	if tIdx := strings.IndexByte(tsStr, 'T'); tIdx >= 0 {
		datePart := tsStr[:tIdx+1]
		timePart := tsStr[tIdx+1:]
		// Only replace first two dashes in time portion (hh-mm-ss)
		timePart = strings.Replace(timePart, "-", ":", 1)
		timePart = strings.Replace(timePart, "-", ":", 1)
		tsStr = datePart + timePart
	}
	t, err := time.Parse("2006-01-02T15:04:05.000Z", tsStr)
	if err != nil {
		t, err = time.Parse("2006-01-02T15:04:05Z", tsStr)
	}
	if err != nil {
		return time.Time{}
	}
	return t
}

// FindQClawSourceFile locates a QClaw session file by its
// raw ID (without the "qclaw:" prefix). The raw ID has the
// format "<agentId>:<sessionId>", which directly maps to the
// file at <agentsDir>/<agentId>/sessions/<sessionId>.jsonl.
//
// If the active .jsonl file does not exist (archive-only session),
// the sessions directory is scanned for any archived file whose
// logical session ID matches. When multiple archived files match,
// the best candidate (newest by filename timestamp) is returned.
func FindQClawSourceFile(agentsDir, rawID string) string {
	if agentsDir == "" {
		return ""
	}

	// Split "agentId:sessionId" into its two parts.
	agentID, sessionID, ok := strings.Cut(rawID, ":")
	if !ok || !IsValidSessionID(agentID) ||
		!IsValidSessionID(sessionID) {
		return ""
	}

	sessionsDir := filepath.Join(
		agentsDir, agentID, "sessions",
	)

	// Fast path: the active .jsonl file exists.
	active := filepath.Join(sessionsDir, sessionID+".jsonl")
	if _, err := os.Stat(active); err == nil {
		return active
	}

	// Slow path: scan for archived files matching this session.
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		return ""
	}

	var best os.DirEntry
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !IsQClawSessionFile(name) {
			continue
		}
		if QClawSessionID(name) != sessionID {
			continue
		}
		if best == nil {
			best = entry
			continue
		}
		best = bestQClawEntry(best, entry)
	}
	if best != nil {
		return filepath.Join(sessionsDir, best.Name())
	}
	return ""
}

// DiscoverIflowProjects finds all project directories under the
// iFlow projects dir and returns their JSONL session files.
// iFlow stores sessions in .iflow/projects/<project>/session-<uuid>.jsonl
func DiscoverIflowProjects(projectsDir string) []DiscoveredFile {
	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		return nil
	}

	var files []DiscoveredFile
	for _, entry := range entries {
		if !isDirOrSymlink(entry, projectsDir) {
			continue
		}

		projDir := filepath.Join(projectsDir, entry.Name())
		sessionFiles, err := os.ReadDir(projDir)
		if err != nil {
			continue
		}

		for _, sf := range sessionFiles {
			if sf.IsDir() {
				continue
			}
			name := sf.Name()
			if !strings.HasPrefix(name, "session-") || !strings.HasSuffix(name, ".jsonl") {
				continue
			}
			files = append(files, DiscoveredFile{
				Path:    filepath.Join(projDir, name),
				Project: entry.Name(),
				Agent:   AgentIflow,
			})
		}
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].Path < files[j].Path
	})
	return files
}

// extractIflowBaseSessionID extracts the base session ID from an iFlow
// session ID. Fork IDs are formatted as <baseUUID>-<childUUID>, so we
// remove the child UUID suffix to get the base session ID for file lookup.
// Both base and child UUIDs are full UUIDs with hyphens, so we count
// hyphens to determine where the base UUID ends (after 4 hyphens).
func extractIflowBaseSessionID(sessionID string) string {
	// iFlow fork IDs have the format: baseUUID-childUUID
	// where both are full UUIDs with hyphens.
	// A standard UUID has format: xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx (4 hyphens)
	// So the base UUID ends after the 4th hyphen, and the child UUID starts after that.

	hyphenCount := 0
	for i, r := range sessionID {
		if r == '-' {
			hyphenCount++
			// The 5th hyphen marks the boundary between base and child UUIDs
			if hyphenCount == 5 {
				// Return everything before this hyphen (the base UUID)
				return sessionID[:i]
			}
		}
	}

	// If we didn't find 5 hyphens, this is not a fork ID
	return sessionID
}

// FindIflowSourceFile finds the original JSONL file for an iFlow
// session ID by searching all project directories.
func FindIflowSourceFile(
	projectsDir, sessionID string,
) string {
	if !IsValidSessionID(sessionID) {
		return ""
	}

	// For fork IDs, extract the base session ID to find the source file
	baseID := extractIflowBaseSessionID(sessionID)

	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		return ""
	}

	target := "session-" + strings.TrimPrefix(baseID, "iflow:") + ".jsonl"
	for _, entry := range entries {
		if !isDirOrSymlink(entry, projectsDir) {
			continue
		}
		candidate := filepath.Join(
			projectsDir, entry.Name(), target,
		)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}

	return ""
}

// DiscoverVibeSessions finds all Vibe session files under the given root directory.
// Vibe stores sessions in: ~/.vibe/logs/session/session_YYYYMMDD_HHMMSS_uuid/
// Each session directory contains messages.jsonl
func DiscoverVibeSessions(root string) []DiscoveredFile {
	var results []DiscoveredFile

	entries, err := os.ReadDir(root)
	if err != nil {
		return results
	}

	for _, entry := range entries {
		if !isDirOrSymlink(entry, root) {
			continue
		}

		// Vibe session directories match pattern: session_YYYYMMDD_HHMMSS_uuid
		// The uuid part can contain hyphens
		if !strings.HasPrefix(entry.Name(), "session_") || !strings.Contains(entry.Name(), "_") {
			continue
		}

		sessionDir := filepath.Join(root, entry.Name())
		messagesPath := filepath.Join(sessionDir, "messages.jsonl")

		if info, err := os.Stat(messagesPath); err == nil && !info.IsDir() {
			results = append(results, DiscoveredFile{
				Path:    messagesPath,
				Agent:   AgentVibe,
				Project: entry.Name(),
			})
		}
	}

	return results
}

// FindVibeSourceFile locates a specific Vibe session file by ID. The ID is the
// session_id recorded in meta.json (a uuid), which usually differs from the
// session directory name. Sessions without meta.json fall back to the directory
// name, so a direct path is tried first before scanning meta.json files.
func FindVibeSourceFile(root, sessionID string) string {
	// Fast path: sessionID is the directory name (no-meta fallback).
	if messagesPath := filepath.Join(root, sessionID, "messages.jsonl"); isVibeMessagesFile(messagesPath) {
		return messagesPath
	}

	// Otherwise sessionID is a meta.json session_id; scan session
	// directories and match on their recorded session_id.
	entries, err := os.ReadDir(root)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if !isDirOrSymlink(entry, root) || !strings.HasPrefix(entry.Name(), "session_") {
			continue
		}
		messagesPath := filepath.Join(root, entry.Name(), "messages.jsonl")
		if !isVibeMessagesFile(messagesPath) {
			continue
		}
		metaPath := filepath.Join(root, entry.Name(), "meta.json")
		if meta, err := parseVibeMetadata(metaPath); err == nil && meta.SessionID == sessionID {
			return messagesPath
		}
	}
	return ""
}

// isVibeMessagesFile reports whether path is an existing regular file.
func isVibeMessagesFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info == nil {
		return false
	}
	return !info.IsDir()
}
