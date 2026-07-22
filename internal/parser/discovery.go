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

const (
	copilotStateDir = "session-state"
	geminiChatsDir  = "chats"
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
	Path        string
	Project     string    // pre-extracted project name
	Agent       AgentType // which agent this file belongs to
	Machine     string    // source machine (set for s3:// sources; empty = host machine)
	SourceSize  int64     // source object size for s3:// sources
	SourceMtime int64     // source object mtime for s3:// sources, UnixNano
	// SourceFingerprint is a durable object fingerprint for s3:// sources.
	SourceFingerprint string
	ForceParse        bool       // caller requires a full source reparse
	ProviderSource    *SourceRef // provider-owned source identity, when known
	ProviderProcess   bool       // true when this caller may parse via ProviderSource
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
	icodemateFmt = openCodeFormat{
		agent: AgentIcodemate, dbName: "icodemate.db",
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
	// Keep a deterministic logical root even before the provider is installed.
	// Lifecycle-aware watcher backends can cover its creation from a bounded
	// ancestor and avoid permanent archive-scale polling of absent defaults.
	return []string{root}
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

func ResolveKiloWatchRoots(root string) []string {
	return resolveOpenCodeFormatWatchRoots(kiloFmt, root)
}

func KiloSQLiteVirtualPath(dbPath, sessionID string) string {
	return OpenCodeSQLiteVirtualPath(dbPath, sessionID)
}

// ResolveIcodemateSource detects whether an Icodemate root is using
// file-backed storage or legacy SQLite storage.
func ResolveIcodemateSource(root string) OpenCodeSource {
	return resolveOpenCodeFormatSource(icodemateFmt, root)
}

func ResolveIcodemateWatchRoots(root string) []string {
	return resolveOpenCodeFormatWatchRoots(icodemateFmt, root)
}

func IcodemateSQLiteVirtualPath(dbPath, sessionID string) string {
	return OpenCodeSQLiteVirtualPath(dbPath, sessionID)
}

func ParseIcodemateSQLiteVirtualPath(
	sourcePath string,
) (dbPath, sessionID string, ok bool) {
	return parseOpenCodeFormatVirtualPath(icodemateFmt.dbName, sourcePath)
}

// ResolveMiMoCodeSource detects whether a MiMoCode root is using
// file-backed storage (storage/session_diff) or SQLite storage.
func ResolveMiMoCodeSource(root string) OpenCodeSource {
	return resolveOpenCodeFormatSource(mimoFmt, root)
}

func ResolveMiMoCodeWatchRoots(root string) []string {
	return resolveOpenCodeFormatWatchRoots(mimoFmt, root)
}

func MiMoCodeSQLiteVirtualPath(dbPath, sessionID string) string {
	return OpenCodeSQLiteVirtualPath(dbPath, sessionID)
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

// ClaudeProjectSessionFiles finds all project directories under the
// Claude projects dir and returns their JSONL session files. It is the
// provider-owned enumeration body shared by the Claude provider source
// set (full-sync discovery) and the engine's duplicate-candidate
// expansion. The name carries no legacy entrypoint verb so the
// provider can call it without shimming a Discover* free function.
func ClaudeProjectSessionFiles(projectsDir string) []DiscoveredFile {
	if strings.HasPrefix(projectsDir, "s3://") {
		return discoverClaudeS3(projectsDir)
	}
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

// claudeFindSourceFile finds the original JSONL file for a Claude
// session ID by searching all project directories. It is the
// provider-owned lookup body used by the Claude provider source set's
// FindSource. The name carries no legacy entrypoint verb so the
// provider can call it without shimming a Find* free function.
func claudeFindSourceFile(
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
			if !isDirOrSymlink(entry, projectsDir) {
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

// IsPiSessionFile reads the first non-blank line of path and returns true
// when the JSON type field equals "session". OMP (Oh My Pi) v16.3+ prefixes
// session files with a fixed-width rewritable {"type":"title",...} slot
// line, so title lines before the session header are skipped, matching the
// header scan in parsePiLikeSession. The scanner buffer grows up to 64 MiB
// to match parser.maxLineSize. Leading blank lines are skipped to match
// lineReader behavior.
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
		typ := gjson.Get(line, "type").Str
		if typ == "title" {
			continue
		}
		return typ == "session"
	}
	return false
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

// discoverVSCodeSessionFiles collects .json and .jsonl
// session files from a directory, preferring .jsonl when
// both exist for the same UUID.
func discoverVSCodeSessionFiles(
	dir string, entries []os.DirEntry, project string, agent AgentType,
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
				Agent:   agent,
			})
		} else if uuid, ok := strings.CutSuffix(name, ".json"); ok {
			// Skip .json if a .jsonl exists for the same UUID
			if hasJSONL[uuid] {
				continue
			}
			files = append(files, DiscoveredFile{
				Path:    filepath.Join(dir, name),
				Project: project,
				Agent:   agent,
			})
		}
	}
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
	bestByConversation := map[string]visualStudioCopilotCandidate{}
	var unreadable []DiscoveredFile
	for _, entry := range entries {
		if entry.IsDir() ||
			!isVisualStudioCopilotTraceFileName(entry.Name()) {
			continue
		}
		path := filepath.Join(dir, entry.Name())
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
			current := bestByConversation[id]
			if visualStudioCopilotCandidateWins(path, mtime, current) {
				bestByConversation[id] = visualStudioCopilotCandidate{
					path: path, mtime: mtime,
				}
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

func findVisualStudioCopilotTraceSourceFile(
	dir, rawID string,
) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	needle := `"gen_ai.conversation.id"`
	valueNeedle := `"stringValue":"` + rawID + `"`
	var best visualStudioCopilotCandidate
	for _, entry := range entries {
		if entry.IsDir() ||
			!isVisualStudioCopilotTraceFileName(entry.Name()) {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		if !visualStudioCopilotTraceContains(path, needle, valueNeedle) {
			continue
		}
		mtime := time.Time{}
		if info, err := entry.Info(); err == nil {
			mtime = info.ModTime()
		}
		// Select the same canonical trace discovery would (newest mtime, then
		// greater path) so a single-session resync resolves the conversation to
		// the identical virtual path discovery stored, even when filename order
		// and mtime order disagree.
		if visualStudioCopilotCandidateWins(path, mtime, best) {
			best = visualStudioCopilotCandidate{path: path, mtime: mtime}
		}
	}
	if best.path == "" {
		return ""
	}
	// Return a conversation-scoped virtual path. The stored file_path is a
	// <traceFile>#<conversationID> key, and returning the bare trace file would
	// let a single-session resync enumerate and rewrite every conversation in
	// that trace rather than only the requested one.
	return VisualStudioCopilotVirtualPath(best.path, rawID)
}

// visualStudioCopilotCandidate is one trace file considered as the canonical
// home for a conversation: its path and mtime.
type visualStudioCopilotCandidate struct {
	path  string
	mtime time.Time
}

// isVisualStudioCopilotTraceFileName reports whether name is a Visual Studio
// Copilot trace file.
func isVisualStudioCopilotTraceFileName(name string) bool {
	return strings.HasSuffix(name, ".jsonl") &&
		strings.Contains(name, "_VSGitHubCopilot_traces")
}

// visualStudioCopilotCandidateWins reports whether the trace at (path, mtime)
// should replace the current best candidate. The canonical rule, shared by
// discovery and single-session lookup, is newest mtime first, then the
// lexicographically greater path as a deterministic tie-breaker. A zero-value
// best (empty path) means no candidate has been chosen yet.
func visualStudioCopilotCandidateWins(
	path string, mtime time.Time, best visualStudioCopilotCandidate,
) bool {
	if best.path == "" {
		return true
	}
	return mtime.After(best.mtime) ||
		(mtime.Equal(best.mtime) && path > best.path)
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
