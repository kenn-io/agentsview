package server

import (
	"bufio"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf16"

	"github.com/google/shlex"
	"github.com/tidwall/gjson"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

// resumeRequest is the JSON body for POST /api/v1/sessions/{id}/resume.
type resumeRequest struct {
	SkipPermissions bool   `json:"skip_permissions,omitempty"`
	ForkSession     bool   `json:"fork_session,omitempty"`
	FromOrdinal     *int   `json:"from_ordinal,omitempty"`
	CommandOnly     bool   `json:"command_only,omitempty"`
	OpenerID        string `json:"opener_id,omitempty"`
}

// resumeResponse is the JSON response for a resume request.
type resumeResponse struct {
	Launched bool   `json:"launched"`
	Terminal string `json:"terminal,omitempty"`
	Command  string `json:"command"`
	Cwd      string `json:"cwd,omitempty"`
	Error    string `json:"error,omitempty"`
}

// resumeAgents maps agent type strings to their resume command templates.
// The %s placeholder is replaced with the (quoted) session ID.
var resumeAgents = map[string]string{
	"claude":   "claude --resume %s",
	"codex":    "codex resume %s",
	"copilot":  "copilot --resume=%s",
	"cursor":   "cursor agent --resume %s",
	"gemini":   "gemini --resume %s",
	"opencode": "opencode --session %s",
	"amp":      "amp --resume %s",
	"kiro":     "kiro-cli chat --resume-id %s",
}

// terminalCandidates lists terminal emulators to try on Linux, in
// preference order. Each entry is {binary, args-before-command...}.
// The resume command is appended after the last arg.
var terminalCandidates = []struct {
	bin  string
	args []string
}{
	{"kitty", []string{"--"}},
	{"alacritty", []string{"-e"}},
	{"wezterm", []string{"start", "--"}},
	{"gnome-terminal", []string{"--", "bash", "-c"}},
	{"konsole", []string{"-e"}},
	{"xfce4-terminal", []string{"-e"}},
	{"tilix", []string{"-e"}},
	{"xterm", []string{"-e"}},
	{"x-terminal-emulator", []string{"-e"}},
}

// shellQuote applies POSIX single-quote escaping.
func shellQuote(s string) string {
	// Simple IDs: alphanumeric + hyphens need no quoting,
	// but a leading '-' must always be quoted to prevent
	// the value being interpreted as a CLI flag.
	safe := len(s) == 0 || s[0] != '-'
	if safe {
		for _, c := range s {
			if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') &&
				(c < '0' || c > '9') && c != '-' && c != '_' {
				safe = false
				break
			}
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

func commandWithCwd(cmd, cwd string) string {
	if !isDir(cwd) {
		return cmd
	}
	return fmt.Sprintf("cd %s && %s", shellQuote(cwd), cmd)
}

func commandWithCleanup(cmd, cleanupPath string) string {
	if cleanupPath == "" {
		return cmd
	}
	return cmd + "; rm -f -- " + shellQuote(cleanupPath)
}

func claudeMessagePointLaunchCommand(
	promptPath string, skipPermissions bool,
) string {
	cmd := "claude"
	if skipPermissions {
		cmd += " --dangerously-skip-permissions"
	}
	cmd += " < " + shellQuote(promptPath)
	return commandWithCleanup(cmd, promptPath)
}

func claudeMessagePointResponseCommand(
	promptPath string, skipPermissions bool, cwd string, goos string,
) string {
	if goos == "windows" {
		return claudeMessagePointWindowsCommand(
			promptPath, skipPermissions, cwd,
		)
	}
	return commandWithCwd(
		claudeMessagePointLaunchCommand(promptPath, skipPermissions),
		cwd,
	)
}

func claudeMessagePointWindowsCommand(
	promptPath string, skipPermissions bool, cwd string,
) string {
	script := claudeMessagePointWindowsScript(
		promptPath, skipPermissions, cwd,
	)
	return "powershell.exe -NoProfile -EncodedCommand " +
		powerShellEncodedCommand(script)
}

func claudeMessagePointWindowsScript(
	promptPath string, skipPermissions bool, cwd string,
) string {
	var b strings.Builder
	b.WriteString("try { ")
	if cwd != "" {
		b.WriteString("Set-Location -LiteralPath ")
		b.WriteString(powerShellSingleQuote(cwd))
		b.WriteString("; ")
	}
	b.WriteString("Get-Content -Raw -Encoding UTF8 -LiteralPath ")
	b.WriteString(powerShellSingleQuote(promptPath))
	b.WriteString(" | & 'claude'")
	if skipPermissions {
		b.WriteString(" --dangerously-skip-permissions")
	}
	b.WriteString(" } finally { Remove-Item -LiteralPath ")
	b.WriteString(powerShellSingleQuote(promptPath))
	b.WriteString(" -Force -ErrorAction SilentlyContinue }")
	return b.String()
}

func powerShellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func powerShellEncodedCommand(script string) string {
	codeUnits := utf16.Encode([]rune(script))
	raw := make([]byte, len(codeUnits)*2)
	for i, unit := range codeUnits {
		binary.LittleEndian.PutUint16(raw[i*2:], unit)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func claudeMessagePointPromptPath(sessionID string, ordinal int) (string, error) {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(cacheDir, "agentsview", "claude-message-points")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	prefix := sanitizeFilename(
		fmt.Sprintf("%s-ordinal-%d-", sessionID, ordinal),
	)
	f, err := os.CreateTemp(dir, prefix+"*.txt")
	if err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

func renderClaudeMessagePointPrompt(
	session *db.Session, msgs []db.Message, ordinal int,
) string {
	var b strings.Builder
	b.WriteString("You are continuing a Claude Code conversation forked from AgentsView.\n")
	b.WriteString("Treat the transcript below as the full context through the selected message ordinal.\n")
	b.WriteString("Continue the conversation from the next turn.\n\n")
	b.WriteString("Session: ")
	b.WriteString(session.ID)
	b.WriteString("\n")
	if session.Project != "" {
		b.WriteString("Project: ")
		b.WriteString(session.Project)
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "Fork point ordinal: %d\n\n", ordinal)
	b.WriteString("Transcript:\n")
	for _, msg := range msgs {
		fmt.Fprintf(&b, "\n## Message %d\n", msg.Ordinal)
		b.WriteString("Role: ")
		b.WriteString(msg.Role)
		b.WriteString("\n")
		if msg.Timestamp != "" {
			b.WriteString("Timestamp: ")
			b.WriteString(msg.Timestamp)
			b.WriteString("\n")
		}
		if msg.Model != "" {
			b.WriteString("Model: ")
			b.WriteString(msg.Model)
			b.WriteString("\n")
		}
		if body := renderClaudeMessagePointMessage(msg); body != "" {
			b.WriteString("\n")
			b.WriteString(body)
			b.WriteString("\n")
		}
	}
	return b.String()
}

func renderClaudeMessagePointMessage(msg db.Message) string {
	var parts []string
	if msg.Content != "" {
		parts = append(parts, msg.Content)
	}
	for _, tc := range msg.ToolCalls {
		header := tc.ToolName
		if header == "" {
			header = "Tool"
		}
		parts = append(parts, "["+header+"]")
		if tc.InputJSON != "" {
			parts = append(parts, tc.InputJSON)
		}
		if tc.ResultContent != "" {
			parts = append(parts, tc.ResultContent)
		}
	}
	return strings.Join(parts, "\n\n")
}

func writeClaudeMessagePointPrompt(
	session *db.Session, msgs []db.Message, ordinal int,
) (string, error) {
	path, err := claudeMessagePointPromptPath(session.ID, ordinal)
	if err != nil {
		return "", err
	}
	prompt := renderClaudeMessagePointPrompt(session, msgs, ordinal)
	if err := os.WriteFile(path, []byte(prompt), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// resumeLaunchCwd returns the cwd a terminal launcher should apply for
// a resume command. Cursor resumes still need the terminal shell to
// start in the last working directory even when --workspace points the
// CLI at the session's workspace root.
func resumeLaunchCwd(agent, openerID, goos, cwd string) string {
	return cwd
}

// detectTerminal finds a suitable terminal emulator and builds the
// full argument list to launch the given command. Returns the
// executable path, args, a user-facing display name, and any error.
func detectTerminal(
	cmd string, cwd string, tc config.TerminalConfig,
) (bin string, args []string, name string, err error) {
	// Custom terminal mode — use the user-configured binary + args.
	if tc.Mode == "custom" && tc.CustomBin != "" {
		path, lookErr := exec.LookPath(tc.CustomBin)
		if lookErr != nil {
			return "", nil, "", fmt.Errorf(
				"custom terminal %q not found: %w",
				tc.CustomBin, lookErr,
			)
		}
		displayName := filepath.Base(tc.CustomBin)
		if tc.CustomArgs != "" {
			// Shell-aware split so that quoted args like
			// --title "My Terminal" are kept together.
			parts, splitErr := shlex.Split(tc.CustomArgs)
			if splitErr != nil {
				return "", nil, "", fmt.Errorf(
					"parsing custom_args: %w", splitErr,
				)
			}
			a := make([]string, 0, len(parts))
			for _, p := range parts {
				a = append(a, strings.ReplaceAll(p, "{cmd}", cmd))
			}
			return path, a, displayName, nil
		}
		// No args template — default pattern.
		return path, []string{"-e", "bash", "-c", cmd + "; exec bash"}, displayName, nil
	}

	switch runtime.GOOS {
	case "darwin":
		return detectTerminalDarwin(cmd, cwd)
	case "linux":
		return detectTerminalLinux(cmd)
	default:
		return "", nil, "", fmt.Errorf(
			"unsupported OS %q for terminal launch", runtime.GOOS,
		)
	}
}

func detectTerminalDarwin(
	cmd string, cwd string,
) (string, []string, string, error) {
	// Check for iTerm2 first, then fall back to Terminal.app.
	// Use osascript to tell the app to open a new window and run
	// the command.
	script := commandWithCwd(cmd, cwd)

	// Try iTerm2 first.
	if _, err := exec.LookPath("osascript"); err == nil {
		safe := escapeForAppleScript(script)

		// Check if iTerm is installed.
		iterm := "/Applications/iTerm.app"
		if _, err := os.Stat(iterm); err == nil {
			appleScript := fmt.Sprintf(
				`tell application "System Events"
					set isRunning to (exists (processes whose name is "iTerm2"))
				end tell
				tell application "iTerm"
					activate
					if isRunning and (count of windows) > 0 then
						tell current window
							create tab with default profile
						end tell
					else
						create window with default profile
					end if
					tell current window
						tell current session
							write text "%s"
						end tell
					end tell
				end tell`,
				safe,
			)
			return "osascript", []string{"-e", appleScript}, "iTerm2", nil
		}
		// Fall back to Terminal.app.
		appleScript := fmt.Sprintf(
			`tell application "Terminal"
				activate
				do script "%s"
			end tell`,
			safe,
		)
		return "osascript", []string{"-e", appleScript}, "Terminal", nil
	}
	return "", nil, "", fmt.Errorf("osascript not found on macOS")
}

// readSessionCwd reads the first few lines of a session JSONL file
// and extracts the initial "cwd" field. Claude Code stores the working
// directory in early conversation entries; some agents (e.g. Codex)
// store it under payload.cwd. Returns "" if not found.
func readSessionCwd(path string) string {
	// Kiro CLI stores cwd in a companion .json metadata file
	// alongside the .jsonl session file.
	if before, ok := strings.CutSuffix(path, ".jsonl"); ok {
		metaPath := before + ".json"
		if data, err := os.ReadFile(metaPath); err == nil {
			if cwd := gjson.GetBytes(data, "cwd").Str; cwd != "" {
				return cwd
			}
		}
	}

	var cwd string
	scanJSONLLines(path, 20, func(line []byte) bool {
		for _, jsonPath := range []string{
			"cwd",
			"payload.cwd",
			// Copilot stores cwd under data.context.cwd on the
			// session.start event.
			"data.context.cwd",
		} {
			if value := gjson.GetBytes(line, jsonPath).Str; value != "" {
				cwd = value
				return false
			}
		}
		return true
	})
	return cwd
}

// readCursorLastWorkingDir scans a Cursor transcript for the most
// recent tool invocation that recorded a working_directory. Returns
// the latest existing absolute directory, or "" if not found.
func readCursorLastWorkingDir(path string) string {
	last := ""
	scanJSONLLines(path, 0, func(line []byte) bool {
		content := gjson.GetBytes(line, "message.content")
		if content.IsArray() {
			content.ForEach(func(_, item gjson.Result) bool {
				if item.Get("type").Str != "tool_use" {
					return true
				}
				for _, jsonPath := range []string{
					"input.working_directory",
					"parameters.working_directory",
				} {
					wd := normalizeCursorDir(item.Get(jsonPath).Str)
					if wd != "" {
						last = wd
					}
				}
				return true
			})
		}
		return true
	})
	return last
}

func scanJSONLLines(
	path string, maxLines int, visit func([]byte) bool,
) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	reader := bufio.NewReader(f)
	for lineNum := 0; maxLines <= 0 || lineNum < maxLines; lineNum++ {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 && !visit(line) {
			return
		}
		if err != nil {
			return
		}
	}
}

func cursorLastWorkingDir(session *db.Session) string {
	if session.Agent != "cursor" || session.FilePath == nil {
		return ""
	}
	return readCursorLastWorkingDir(*session.FilePath)
}

func resolveCursorResumePaths(
	session *db.Session, lastCwd string,
) (launchDir, workspaceDir string) {
	workspaceDir = resolveCursorWorkspaceDirWithHint(
		session,
		func() string { return lastCwd },
	)
	if workspaceDir == "" {
		workspaceDir = lastCwd
	}
	if lastCwd != "" {
		return lastCwd, workspaceDir
	}
	return workspaceDir, workspaceDir
}

func resolveResumePaths(session *db.Session) (launchDir, workspaceDir string) {
	if session.Agent != "cursor" {
		return resolveSessionDir(session), ""
	}
	return resolveCursorResumePaths(
		session, cursorLastWorkingDir(session),
	)
}

func resolveCursorWorkspaceDirFromTranscriptPath(
	session *db.Session,
) (string, bool) {
	if session.FilePath == nil {
		return "", false
	}
	dir, ambiguous := resolveCursorProjectDirFromSessionFile(
		*session.FilePath,
	)
	if canonical := normalizeCursorDir(dir); canonical != "" {
		return canonical, ambiguous
	}
	return "", false
}

func resolveCursorWorkspaceDirFromTranscriptPathHint(
	session *db.Session, hint string,
) string {
	if session.FilePath == nil {
		return ""
	}
	dir := resolveCursorProjectDirFromSessionFileHint(
		*session.FilePath, hint,
	)
	return normalizeCursorDir(dir)
}

func resolveCursorWorkspaceDirWithHint(
	session *db.Session, hintFn func() string,
) string {
	projectDir := normalizeCursorDir(session.Project)
	if dir, ambiguous := resolveCursorWorkspaceDirFromTranscriptPath(
		session,
	); dir != "" {
		if ambiguous {
			hint := projectDir
			if hintFn != nil {
				if value := hintFn(); value != "" {
					hint = value
				}
			}
			if hint != "" {
				if hinted := resolveCursorWorkspaceDirFromTranscriptPathHint(
					session, hint,
				); hinted != "" {
					return hinted
				}
			}
			// Ambiguous with no useful hint — don't guess.
			return projectDir
		}
		return dir
	}
	return projectDir
}

// resolveResumeDir determines the terminal launch directory for a
// session resume. Cursor sessions prefer the latest recorded
// working_directory so resumed chats reopen in the same shell cwd
// they last used instead of a generic workspace root.
func resolveResumeDir(session *db.Session) string {
	launchDir, _ := resolveResumePaths(session)
	return launchDir
}

func isVirtualSessionPath(path string) bool {
	if _, _, ok := parser.ParseVirtualSourcePathForBase(path, "data.sqlite3"); ok {
		return true
	}
	if _, _, ok := parser.ParseVirtualSourcePathForBase(path, "opencode.db"); ok {
		return true
	}
	return false
}

// resolveSessionDir determines the project directory for a session.
// It tries the session file's embedded cwd first, then the cached cwd,
// then Cursor's transcript-derived workspace path, then falls back to
// the session's project field. Virtual DB-backed file paths are storage
// locators only, so they skip source-file cwd reads and use cached cwd.
// All returned candidates must be absolute paths pointing to existing
// directories.
func resolveSessionDir(session *db.Session) string {
	if session.FilePath != nil && !isVirtualSessionPath(*session.FilePath) {
		if cwd := readSessionCwd(*session.FilePath); isDir(cwd) {
			return cwd
		}
	}
	if isDir(session.Cwd) {
		return session.Cwd
	}
	if session.Agent == "cursor" {
		if dir := resolveCursorWorkspaceDir(session); dir != "" {
			return dir
		}
	}
	if isDir(session.Project) {
		return session.Project
	}
	return ""
}

// resolveCursorWorkspaceDir returns the real workspace root for a
// Cursor session, preferring the transcript path and falling back to
// an absolute project field when available. It only scans transcript
// contents when the transcript path maps to multiple plausible
// workspace roots.
func resolveCursorWorkspaceDir(session *db.Session) string {
	return resolveCursorWorkspaceDirWithHint(
		session,
		func() string { return cursorLastWorkingDir(session) },
	)
}

func normalizeCursorDir(path string) string {
	if !isDir(path) {
		return ""
	}
	clean := filepath.Clean(path)
	resolved, err := filepath.EvalSymlinks(clean)
	if err != nil || !isDir(resolved) {
		return clean
	}
	resolved = filepath.Clean(resolved)
	if runtime.GOOS == "darwin" &&
		strings.HasPrefix(resolved, "/private/") {
		publicPath := filepath.Clean(
			strings.TrimPrefix(resolved, "/private"),
		)
		if isDir(publicPath) {
			return publicPath
		}
	}
	return resolved
}

func isDir(path string) bool {
	if !filepath.IsAbs(path) {
		return false
	}
	info, err := os.Stat(path)
	if err != nil || info == nil {
		return false
	}
	return info.IsDir()
}

func detectTerminalLinux(cmd string) (string, []string, string, error) {
	// Check $TERMINAL env var first. The value may contain
	// arguments (e.g. "kitty --single-instance"), so split it
	// with a shell lexer and use the first token for LookPath.
	if envTerm := os.Getenv("TERMINAL"); envTerm != "" {
		parts, splitErr := shlex.Split(envTerm)
		if splitErr == nil && len(parts) > 0 {
			if path, err := exec.LookPath(parts[0]); err == nil {
				base := filepath.Base(parts[0])
				args := buildTerminalArgs(base, cmd)
				// Prepend extra tokens from $TERMINAL before
				// the template args (e.g. --single-instance).
				if len(parts) > 1 {
					args = append(parts[1:], args...)
				}
				return path, args, base, nil
			}
		}
	}

	// Try each candidate in preference order.
	for _, c := range terminalCandidates {
		path, err := exec.LookPath(c.bin)
		if err != nil {
			continue
		}
		return path, buildTerminalArgs(c.bin, cmd), c.bin, nil
	}

	return "", nil, "", fmt.Errorf(
		"no terminal emulator found; install kitty, alacritty, " +
			"gnome-terminal, or set $TERMINAL",
	)
}

// buildTerminalArgs returns the argument list for launching a command
// in a named terminal. The bin parameter is the terminal basename
// (e.g. "kitty", "gnome-terminal"). Used by both $TERMINAL and the
// auto-detection loop.
func buildTerminalArgs(bin, cmd string) []string {
	switch bin {
	case "gnome-terminal":
		return []string{"--", "bash", "-c", cmd + "; exec bash"}
	case "kitty":
		return []string{"--", "bash", "-c", cmd + "; exec bash"}
	case "alacritty":
		return []string{"-e", "bash", "-c", cmd + "; exec bash"}
	case "wezterm":
		return []string{"start", "--", "bash", "-c", cmd + "; exec bash"}
	case "konsole":
		return []string{"-e", "bash", "-c", cmd + "; exec bash"}
	case "xfce4-terminal":
		return []string{"-e", "bash -c '" + strings.ReplaceAll(cmd, "'", `'"'"'`) + "; exec bash'"}
	case "tilix":
		return []string{"-e", "bash -c '" + strings.ReplaceAll(cmd, "'", `'"'"'`) + "; exec bash'"}
	case "xterm":
		return []string{"-e", "bash", "-c", cmd + "; exec bash"}
	default:
		return []string{"-e", "bash", "-c", cmd + "; exec bash"}
	}
}

// launchResumeInOpener builds an exec.Cmd that runs a shell command
// inside the terminal identified by the opener. Returns nil if the
// opener kind is not "terminal" (or "action" for special openers like
// Claude Desktop) or the terminal is not supported.
func launchResumeInOpener(
	o Opener, cmd string, cwd string,
) *exec.Cmd {
	if o.ID == "claude-desktop" {
		return nil // handled separately via launchClaudeDesktop
	}
	if o.Kind != "terminal" {
		return nil
	}

	if runtime.GOOS == "darwin" {
		return launchResumeDarwin(o, cmd, cwd)
	}

	// Linux: launch via CLI binary with per-terminal arg patterns.
	// Wrap the resume command so the shell stays open after it exits.
	args := buildTerminalArgs(o.ID, cmd+"; exec bash")
	proc := exec.Command(o.Bin, args...)
	if cwd != "" {
		proc.Dir = cwd
	}
	proc.Stdout = nil
	proc.Stderr = nil
	proc.Stdin = nil
	return proc
}

// launchResumeDarwin launches a resume command in a macOS terminal
// app. Uses AppleScript for iTerm2/Terminal.app and `open -na` with
// appropriate flags for others.
func launchResumeDarwin(
	o Opener, cmd string, cwd string,
) *exec.Cmd {
	// For AppleScript-based terminals, build a single shell command
	// that enters the requested directory and then runs the resume
	// command. The caller passes the raw resume command without a
	// leading `cd` so terminal-specific launchers only add it once.
	shellCmd := commandWithCwd(cmd, cwd)
	safe := escapeForAppleScript(shellCmd)

	switch o.ID {
	case "iterm2":
		script := fmt.Sprintf(
			`tell application "System Events"
				set isRunning to (exists (processes whose name is "iTerm2"))
			end tell
			tell application "iTerm"
				activate
				if isRunning and (count of windows) > 0 then
					tell current window
						create tab with default profile
					end tell
				else
					create window with default profile
				end if
				tell current window
					tell current session
						write text "%s"
					end tell
				end tell
			end tell`, safe,
		)
		return exec.Command("osascript", "-e", script)
	case "terminal":
		script := fmt.Sprintf(
			`tell application "Terminal"
				activate
				do script "%s"
			end tell`, safe,
		)
		return exec.Command("osascript", "-e", script)
	case "ghostty":
		var args []string
		if cwd != "" {
			args = append(args, "--working-directory="+cwd)
		}
		args = append(args, "-e", "bash", "-c",
			cmd+"; exec bash")
		return macExecCommand(o.Bin, args...)
	case "kitty":
		var args []string
		if cwd != "" {
			args = append(args, "-d", cwd)
		}
		args = append(args, "bash", "-c", cmd+"; exec bash")
		return macExecCommand(o.Bin, args...)
	case "alacritty":
		var args []string
		if cwd != "" {
			args = append(args, "--working-directory", cwd)
		}
		args = append(args, "-e", "bash", "-c",
			cmd+"; exec bash")
		return macExecCommand(o.Bin, args...)
	case "wezterm":
		args := []string{"start"}
		if cwd != "" {
			args = append(args, "--cwd", cwd)
		}
		args = append(args, "--", "bash", "-c",
			cmd+"; exec bash")
		return macExecCommand(o.Bin, args...)
	default:
		return nil
	}
}

// launchClaudeDesktop builds an exec.Cmd that opens a Claude Code
// session in Claude Desktop via the claude:// URL scheme. The URL
// format is claude://resume?session={id}&cwd={path}.
func launchClaudeDesktop(sessionID string, cwd string) *exec.Cmd {
	u := "claude://resume?session=" + url.QueryEscape(sessionID)
	if cwd != "" {
		u += "&cwd=" + url.QueryEscape(cwd)
	}
	return exec.Command("open", u)
}
