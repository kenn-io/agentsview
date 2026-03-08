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
	return err == nil && fi.IsDir()
}

// DiscoveredFile holds a discovered session file.
type DiscoveredFile struct {
	Path    string
	Project string    // pre-extracted project name
	Agent   AgentType // which agent this file belongs to
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

		// Scan session directories for subagent files
		for _, sf := range sessionFiles {
			if !sf.IsDir() {
				continue
			}
			subagentsDir := filepath.Join(
				projDir, sf.Name(), "subagents",
			)
			subFiles, err := os.ReadDir(subagentsDir)
			if err != nil {
				continue
			}
			for _, sub := range subFiles {
				if sub.IsDir() {
					continue
				}
				name := sub.Name()
				if !strings.HasPrefix(name, "agent-") ||
					!strings.HasSuffix(name, ".jsonl") {
					continue
				}
				files = append(files, DiscoveredFile{
					Path: filepath.Join(
						subagentsDir, name,
					),
					Project: entry.Name(),
					Agent:   AgentClaude,
				})
			}
		}
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].Path < files[j].Path
	})
	return files
}

// DiscoverCodexSessions finds all JSONL files under the Codex
// sessions dir (year/month/day structure).
func DiscoverCodexSessions(sessionsDir string) []DiscoveredFile {
	var files []DiscoveredFile

	walkCodexDayDirs(sessionsDir, func(dayPath string) bool {
		entries, err := os.ReadDir(dayPath)
		if err != nil {
			return true
		}
		for _, sf := range entries {
			if sf.IsDir() {
				continue
			}
			if !strings.HasSuffix(sf.Name(), ".jsonl") {
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
	// <project>/<session>/subagents/agent-<id>.jsonl
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
				candidate := filepath.Join(
					projDir, sd.Name(),
					"subagents", target,
				)
				if _, err := os.Stat(candidate); err == nil {
					return candidate
				}
			}
		}
	}

	return ""
}

// FindCodexSourceFile finds a Codex session file by UUID.
// Searches the year/month/day directory structure for files matching
// rollout-{timestamp}-{uuid}.jsonl.
func FindCodexSourceFile(sessionsDir, sessionID string) string {
	if !IsValidSessionID(sessionID) {
		return ""
	}

	var result string
	walkCodexDayDirs(sessionsDir, func(dayPath string) bool {
		if result != "" {
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
			if !strings.HasPrefix(name, "rollout-") ||
				!strings.HasSuffix(name, ".jsonl") {
				continue
			}
			if extractUUIDFromRollout(name) == sessionID {
				result = filepath.Join(dayPath, name)
				return false
			}
		}
		return true
	})
	return result
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

// DiscoverGeminiSessions finds all session JSON files under
// the Gemini directory (~/.gemini/tmp/*/chats/session-*.json).
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
			if !strings.HasPrefix(name, "session-") ||
				!strings.HasSuffix(name, ".json") {
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
			if !strings.HasPrefix(name, "session-") ||
				!strings.HasSuffix(name, ".json") {
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
		seen := make(map[string]string) // stem -> path
		for _, sf := range transcripts {
			if sf.IsDir() {
				continue
			}
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
			stem := strings.TrimSuffix(
				name, filepath.Ext(name),
			)
			if prev, ok := seen[stem]; ok {
				// .jsonl wins over .txt
				if strings.HasSuffix(prev, ".txt") &&
					strings.HasSuffix(name, ".jsonl") {
					seen[stem] = fullPath
				}
				continue
			}
			seen[stem] = fullPath
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
			candidate := filepath.Join(
				projectsDir, entry.Name(),
				"agent-transcripts", target,
			)
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

// isPiSessionFile reads the first non-blank line of path and returns true
// when the JSON type field equals "session". The scanner buffer grows up to
// 64 MiB to match parser.maxLineSize. Leading blank lines are skipped to
// match lineReader behavior.
func isPiSessionFile(path string) bool {
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
			if !isPiSessionFile(path) {
				continue
			}
			files = append(files, DiscoveredFile{
				Path:  path,
				Agent: AgentPi,
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
