package parser

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/tidwall/gjson"
)

// maxSkillFrontmatterSize bounds how much of a SKILL.md file is
// read when extracting the frontmatter name. Frontmatter sits at
// the top of the file and is tiny; the cap protects against a
// transcript pointing at a device or very large file.
const maxSkillFrontmatterSize = 64 << 10

var (
	skillNameByPath  sync.Map
	skillPathRE      = regexp.MustCompile(`(?:"([^"]*[/\\]SKILL\.md)"|'([^']*[/\\]SKILL\.md)'|(\S*[/\\]SKILL\.md)|(?:^|[\s"'])(SKILL\.md))`)
	shellSegmentRE   = regexp.MustCompile(`\s*(?:&&|\|\||;|&|\|)\s*`)
	outputRedirectRE = regexp.MustCompile(`^[0-9&]*>>?$`)
)

// searchValueFlags lists grep/rg flags (short and long) that consume
// the following token as their value, so it is neither the search
// pattern nor a file operand.
var searchValueFlags = map[string]bool{
	"-A": true, "-B": true, "-C": true, "-m": true, "-d": true,
	"-e": true, "-f": true, "-g": true, "-t": true, "-T": true,
	"--after-context": true, "--before-context": true, "--context": true,
	"--max-count": true, "--max-depth": true, "--regexp": true,
	"--file": true, "--glob": true, "--type": true, "--type-not": true,
}

// inferToolSkillName infers a skill name for an assistant tool
// call by trying the read-file heuristic first, then the shell
// read-command heuristic. Used by both the Cursor JSONL and
// plain-text transcript paths so they stay in sync.
func inferToolSkillName(toolName, inputJSON string) string {
	if name := inferCursorSkillName(toolName, inputJSON); name != "" {
		return name
	}
	return inferCodexSkillName(toolName, inputJSON)
}

func inferCursorSkillName(toolName, inputJSON string) string {
	if !isCursorSkillReadTool(toolName) {
		return ""
	}
	return inferSkillNameFromJSONPaths(inputJSON)
}

func inferCodexSkillName(toolName, inputJSON string) string {
	return inferCodexSkillNameWithBase(toolName, inputJSON, "")
}

// inferCodexSkillNameWithBase infers a Codex skill name, resolving
// relative SKILL.md paths against the tool call's own workdir/cwd
// hint when present, otherwise against fallbackBaseDir (typically
// the session working directory from session_meta).
func inferCodexSkillNameWithBase(toolName, inputJSON, fallbackBaseDir string) string {
	if !isCodexShellTool(toolName) {
		return ""
	}
	cmd := skillCommandFromInput(inputJSON)
	if !strings.Contains(cmd, "SKILL.md") {
		return ""
	}
	baseDir := skillBaseDirFromInput(inputJSON)
	if baseDir == "" {
		baseDir = fallbackBaseDir
	}
	for _, path := range skillPathsFromCommand(cmd) {
		if name := skillNameFromPath(path, baseDir); name != "" {
			return name
		}
	}
	return ""
}

// skillPathsFromCommand extracts candidate SKILL.md paths from a shell
// command. Each control-operator-delimited segment is handled by its
// own leading verb: only read commands (cat/sed/... and grep/rg) yield
// paths, so SKILL.md in an unrelated segment (e.g. `echo SKILL.md`) is
// ignored, and a grep/rg search pattern is not mistaken for a file.
func skillPathsFromCommand(cmd string) []string {
	var paths []string
	for _, seg := range shellSegmentRE.Split(cmd, -1) {
		paths = append(paths, skillPathsFromSegment(seg)...)
	}
	return paths
}

func skillPathsFromSegment(seg string) []string {
	tokens := tokenizeCommand(seg)
	if len(tokens) == 0 {
		return nil
	}
	args := stripRedirects(tokens[1:])
	switch commandVerb(tokens[0]) {
	case "grep", "rg":
		return skillPathsFromSearchArgs(args)
	case "sed":
		// sed is a read verb, but -i / --in-place edits its operands in
		// place, so a SKILL.md argument is written, not read.
		if sedWritesInPlace(args) {
			return nil
		}
		return skillFilePaths(args)
	case "cat", "head", "tail", "less", "more":
		return skillFilePaths(args)
	default:
		return nil
	}
}

// sedWritesInPlace reports whether a sed invocation edits its file
// operands in place via -i (optionally with a backup suffix, e.g.
// -i.bak) or --in-place, which makes a SKILL.md operand a write target.
func sedWritesInPlace(args []string) bool {
	for _, arg := range args {
		if strings.HasPrefix(arg, "-i") || strings.HasPrefix(arg, "--in-place") {
			return true
		}
	}
	return false
}

// stripRedirects drops output-redirection operators (>, >>, 2>, &>,
// ...) together with their target token, so a redirect destination
// such as the SKILL.md in `cat foo > SKILL.md` is written, not read,
// and is never collected as a file operand.
func stripRedirects(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		if outputRedirectRE.MatchString(args[i]) {
			i++ // also skip the redirect target token
			continue
		}
		out = append(out, args[i])
	}
	return out
}

// tokenizeCommand splits a command segment into tokens on unquoted
// whitespace, honoring single and double quotes so a quoted argument
// stays one token (e.g. the multi-word pattern in `grep "a b" f`).
// Unlike a full shell lexer it treats backslash literally, preserving
// Windows paths such as C:\skills\foo\SKILL.md.
func tokenizeCommand(seg string) []string {
	var tokens []string
	var cur strings.Builder
	inToken := false
	var quote rune
	flush := func() {
		if inToken {
			tokens = append(tokens, cur.String())
			cur.Reset()
			inToken = false
		}
	}
	for _, r := range seg {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
			inToken = true
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			flush()
		default:
			cur.WriteRune(r)
			inToken = true
		}
	}
	flush()
	return tokens
}

// commandVerb returns the lowercased base name of a command token,
// dropping any leading path (e.g. "/usr/bin/grep" -> "grep").
func commandVerb(token string) string {
	token = strings.ToLower(token)
	if i := strings.LastIndexAny(token, `/\`); i >= 0 {
		token = token[i+1:]
	}
	return token
}

// skillFilePaths returns the operand tokens that name a SKILL.md file.
// Each token is a whole shell operand, so a path is taken verbatim
// rather than re-scanned, preserving spaces inside quoted paths.
func skillFilePaths(args []string) []string {
	var paths []string
	for _, arg := range args {
		if isSkillMarkdownPath(strings.TrimSpace(arg)) {
			paths = append(paths, arg)
		}
	}
	return paths
}

// skillPathsFromSearchArgs extracts SKILL.md file operands from grep/rg
// arguments. The search pattern (the value of -e/-f, or otherwise the
// first non-flag operand) is skipped so it is not read as a file;
// operands after it are files.
func skillPathsFromSearchArgs(args []string) []string {
	var files []string
	patternSeen := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg != "-" && strings.HasPrefix(arg, "-") {
			flag, _, hasValue := strings.Cut(arg, "=")
			if flag == "-e" || flag == "-f" ||
				flag == "--regexp" || flag == "--file" {
				patternSeen = true
			}
			if !hasValue && searchValueFlags[flag] {
				i++
			}
			continue
		}
		if !patternSeen {
			patternSeen = true
			continue
		}
		files = append(files, arg)
	}
	return skillFilePaths(files)
}

func inferSkillNameFromJSONPaths(inputJSON string) string {
	trimmed := strings.TrimSpace(inputJSON)
	if trimmed == "" {
		return ""
	}
	baseDir := skillBaseDirFromInput(trimmed)
	if !gjson.Valid(trimmed) {
		for _, path := range skillPathsFromText(trimmed) {
			if name := skillNameFromPath(path, baseDir); name != "" {
				return name
			}
		}
		return ""
	}

	var v any
	if err := json.Unmarshal([]byte(trimmed), &v); err != nil {
		return ""
	}
	var found string
	var walk func(any)
	walk = func(x any) {
		if found != "" {
			return
		}
		switch t := x.(type) {
		case string:
			// A JSON string value is already one unescaped path, so
			// try it whole first — the free-text regex below splits
			// on whitespace and would truncate paths with spaces.
			if name := skillNameFromPath(t, baseDir); name != "" {
				found = name
				return
			}
			for _, path := range skillPathsFromText(t) {
				if name := skillNameFromPath(path, baseDir); name != "" {
					found = name
					return
				}
			}
		case []any:
			for _, item := range t {
				walk(item)
				if found != "" {
					return
				}
			}
		case map[string]any:
			for _, item := range t {
				walk(item)
				if found != "" {
					return
				}
			}
		}
	}
	walk(v)
	return found
}

func isCursorSkillReadTool(toolName string) bool {
	switch strings.ToLower(strings.TrimSpace(toolName)) {
	case "read", "readfile", "read_file":
		return true
	default:
		return false
	}
}

func isCodexShellTool(toolName string) bool {
	switch strings.ToLower(strings.TrimSpace(toolName)) {
	case "exec_command", "shell_command", "shell", "bash":
		return true
	default:
		return false
	}
}

func skillCommandFromInput(inputJSON string) string {
	trimmed := strings.TrimSpace(inputJSON)
	if trimmed == "" {
		return ""
	}
	if gjson.Valid(trimmed) {
		g := gjson.Parse(trimmed)
		for _, key := range []string{"cmd", "command", "script"} {
			if s := strings.TrimSpace(g.Get(key).Str); s != "" {
				return s
			}
		}
	}
	return trimmed
}

func skillPathsFromText(text string) []string {
	matches := skillPathRE.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return nil
	}
	paths := make([]string, 0, len(matches))
	for _, m := range matches {
		if !skillPathMatchHasBoundary(text, m[1]) {
			continue
		}
		for i := 2; i < len(m); i += 2 {
			if m[i] >= 0 && m[i+1] >= 0 {
				paths = append(paths, text[m[i]:m[i+1]])
				break
			}
		}
	}
	return paths
}

func skillPathMatchHasBoundary(text string, end int) bool {
	if end >= len(text) {
		return true
	}
	switch text[end] {
	case ' ', '\t', '\n', '\r', '"', '\'', ';', '&', '|', ')', '}', ']':
		return true
	default:
		return false
	}
}

// skillNameFromPath derives a skill name from a captured
// SKILL.md path. baseDir is the tool/session working directory
// (when known) used to resolve relative paths before reading
// frontmatter. A leading "~" is expanded to the user home dir.
// When the path stays relative because no base directory is
// available, the frontmatter read is skipped so an unrelated
// SKILL.md under the agentsview process cwd cannot be read by
// accident; the parent-directory name is used as a fallback.
// Paths carrying shell glob metacharacters are rejected: they come
// from discovery commands (e.g. "**/SKILL.md") rather than a
// concrete file, and would otherwise yield a bogus name like "**".
func skillNameFromPath(path, baseDir string) string {
	path = strings.TrimSpace(path)
	if path == "" || skillPathIsGlob(path) || !isSkillMarkdownPath(path) {
		return ""
	}
	resolved, readable := resolveSkillPath(path, baseDir)
	clean := filepath.Clean(resolved)
	if cached, ok := skillNameByPath.Load(clean); ok {
		return cached.(string)
	}

	var name string
	if readable {
		name = skillNameFromFrontmatter(clean)
	}
	if name == "" && !skillPathIsBare(path) {
		// A path with a directory component names the skill via its
		// folder even when the file is unreadable (e.g.
		// "skills/foo/SKILL.md" -> "foo"). A bare "SKILL.md" carries
		// no such signal, so it is only inferred from frontmatter of
		// a resolvable file; this keeps a read command that merely
		// references SKILL.md (e.g. `grep SKILL.md notes.txt`) from
		// being miscounted as the working directory's name.
		name = skillNameFromParentDir(clean)
	}
	skillNameByPath.Store(clean, name)
	return name
}

// resolveSkillPath expands a leading "~" and joins relative
// paths against baseDir. It returns the path to use and whether
// that path is absolute (and therefore safe to read frontmatter
// from). A relative path with no usable base is returned as-is
// with readable=false.
func resolveSkillPath(path, baseDir string) (string, bool) {
	path = expandSkillHome(path)
	if filepath.IsAbs(path) {
		return path, true
	}
	if baseDir = expandSkillHome(strings.TrimSpace(baseDir)); filepath.IsAbs(baseDir) {
		return filepath.Join(baseDir, path), true
	}
	return path, false
}

// expandSkillHome replaces a leading "~" or "~/" with the
// current user's home directory. Other inputs are returned
// unchanged.
func expandSkillHome(path string) string {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	if path == "~" {
		return home
	}
	return filepath.Join(home, path[len("~/"):])
}

// skillBaseDirFromInput extracts a working-directory hint from a
// tool call's input JSON (e.g. Codex exec_command "workdir").
func skillBaseDirFromInput(inputJSON string) string {
	trimmed := strings.TrimSpace(inputJSON)
	if trimmed == "" || !gjson.Valid(trimmed) {
		return ""
	}
	g := gjson.Parse(trimmed)
	for _, key := range []string{"workdir", "cwd", "working_directory"} {
		if s := strings.TrimSpace(g.Get(key).Str); s != "" {
			return s
		}
	}
	return ""
}

func isSkillMarkdownPath(path string) bool {
	normalized := strings.ReplaceAll(path, "\\", "/")
	return strings.HasSuffix(normalized, "/SKILL.md") ||
		normalized == "SKILL.md"
}

// skillPathIsGlob reports whether a captured path holds shell glob
// metacharacters, marking it as a discovery pattern rather than a
// concrete file to read.
func skillPathIsGlob(path string) bool {
	return strings.ContainsAny(path, "*?[")
}

// skillPathIsBare reports whether a captured path is just "SKILL.md"
// with no directory component, so the surrounding folder name is not
// available as a skill-name fallback.
func skillPathIsBare(path string) bool {
	return !strings.ContainsAny(path, `/\`)
}

func skillNameFromFrontmatter(path string) string {
	// Lstat first so a symlink, FIFO, device, or directory captured
	// from a transcript is rejected before open — opening a FIFO
	// read-only would otherwise block waiting for a writer.
	if li, err := os.Lstat(path); err != nil || !li.Mode().IsRegular() {
		return ""
	}

	// Open with O_NOFOLLOW and re-check via Fstat to close the
	// window between Lstat and open, then read only a bounded
	// prefix so a crafted path cannot exhaust memory during sync.
	f, err := openNoFollow(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return ""
	}

	b, err := io.ReadAll(io.LimitReader(f, maxSkillFrontmatterSize))
	if err != nil {
		return ""
	}
	text := strings.TrimPrefix(string(b), "\ufeff")
	if !strings.HasPrefix(text, "---") {
		return ""
	}
	lines := strings.Split(text, "\n")
	for i := 1; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if line == "---" || line == "..." {
			return ""
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(key) != "name" {
			continue
		}
		value = strings.TrimSpace(value)
		value = strings.Trim(value, `"'`)
		return value
	}
	return ""
}

func skillNameFromParentDir(path string) string {
	dir := filepath.Base(filepath.Dir(path))
	if dir == "." || dir == string(filepath.Separator) {
		return ""
	}
	return dir
}
