package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"bufio"

	"github.com/google/shlex"
	"github.com/tidwall/gjson"
	"github.com/wesm/agentsview/internal/config"
	"github.com/wesm/agentsview/internal/db"
)

// resumeRequest is the JSON body for POST /api/v1/sessions/{id}/resume.
type resumeRequest struct {
	SkipPermissions bool   `json:"skip_permissions"`
	ForkSession     bool   `json:"fork_session"`
	CommandOnly     bool   `json:"command_only"`
	OpenerID        string `json:"opener_id"`
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
	"gemini":   "gemini --resume %s",
	"opencode": "opencode --session %s",
	"amp":      "amp --resume %s",
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

func (s *Server) handleResumeSession(
	w http.ResponseWriter, r *http.Request,
) {
	id := r.PathValue("id")

	// Look up the session with full file metadata so
	// resolveSessionDir can read the session file for cwd.
	session, err := s.db.GetSessionFull(r.Context(), id)
	if err != nil {
		if handleContextError(w, err) {
			return
		}
		log.Printf("resume: session lookup failed: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if session == nil || session.DeletedAt != nil {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}

	// Check if this agent supports resumption.
	tmpl, ok := resumeAgents[string(session.Agent)]
	if !ok {
		writeError(
			w, http.StatusBadRequest,
			fmt.Sprintf("agent %q does not support resume", session.Agent),
		)
		return
	}

	// Parse optional flags.
	var req resumeRequest
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
	}

	// Strip agent prefix from compound ID only when it matches the
	// expected agent (e.g. "codex:abc" → "abc"). Raw IDs that
	// happen to contain ":" are left untouched.
	prefix := string(session.Agent) + ":"
	rawID := strings.TrimPrefix(id, prefix)

	// Build the CLI command.
	cmd := fmt.Sprintf(tmpl, shellQuote(rawID))
	if string(session.Agent) == "claude" {
		if req.SkipPermissions {
			cmd += " --dangerously-skip-permissions"
		}
		if req.ForkSession {
			cmd += " --fork-session"
		}
	}

	// Resolve the project directory from the session file or
	// project field for use in cd prefix and response metadata.
	sessionDir := resolveSessionDir(session)

	// Claude Code scopes sessions by the working directory the
	// session was started from. Prepend cd <cwd> so the resume
	// works from any terminal location. Only Claude uses this
	// pattern — other agents handle directory scoping internally.
	if string(session.Agent) == "claude" && sessionDir != "" {
		cmd = fmt.Sprintf("cd %s && %s", shellQuote(sessionDir), cmd)
	}

	// If the caller only wants the command string (e.g. for
	// clipboard copy), skip terminal detection and launch.
	if req.CommandOnly {
		writeJSON(w, http.StatusOK, resumeResponse{
			Launched: false,
			Command:  cmd,
			Cwd:      sessionDir,
		})
		return
	}

	// Block actual launches in read-only mode. command_only
	// requests above are safe and remain available.
	if s.db.ReadOnly() {
		writeError(w, http.StatusNotImplemented,
			"session launch not available in remote mode")
		return
	}

	// If the caller specified a terminal opener, use it directly.
	if req.OpenerID != "" {
		openers := detectOpeners()
		var opener *Opener
		for i := range openers {
			if openers[i].ID == req.OpenerID {
				opener = &openers[i]
				break
			}
		}
		if opener == nil {
			writeError(w, http.StatusBadRequest,
				fmt.Sprintf("opener %q not found", req.OpenerID))
			return
		}
		proc := launchResumeInOpener(*opener, cmd, sessionDir)
		if proc == nil {
			writeJSON(w, http.StatusOK, resumeResponse{
				Launched: false,
				Command:  cmd,
				Cwd:      sessionDir,
				Error:    "unsupported_opener",
			})
			return
		}
		if err := proc.Start(); err != nil {
			log.Printf("resume: opener start failed: %v", err)
			writeJSON(w, http.StatusOK, resumeResponse{
				Launched: false,
				Command:  cmd,
				Cwd:      sessionDir,
				Error:    "terminal_launch_failed",
			})
			return
		}
		go func() { _ = proc.Wait() }()
		writeJSON(w, http.StatusOK, resumeResponse{
			Launched: true,
			Terminal: opener.Name,
			Command:  cmd,
			Cwd:      sessionDir,
		})
		return
	}

	// Check terminal config.
	s.mu.RLock()
	termCfg := s.cfg.Terminal
	s.mu.RUnlock()

	if termCfg.Mode == "clipboard" {
		// User explicitly chose clipboard-only mode.
		writeJSON(w, http.StatusOK, resumeResponse{
			Launched: false,
			Command:  cmd,
		})
		return
	}

	// Detect and launch a terminal.
	termBin, termArgs, termName, termErr := detectTerminal(cmd, sessionDir, termCfg)
	if termErr != nil {
		// Can't launch — return the command for clipboard fallback.
		log.Printf("resume: terminal detection failed: %v", termErr)
		writeJSON(w, http.StatusOK, resumeResponse{
			Launched: false,
			Command:  cmd,
			Cwd:      sessionDir,
			Error:    "no_terminal_found",
		})
		return
	}

	// Fire and forget — we don't need the terminal process to
	// complete before responding.
	proc := exec.Command(termBin, termArgs...)
	proc.Stdout = nil
	proc.Stderr = nil
	proc.Stdin = nil
	if sessionDir != "" {
		proc.Dir = sessionDir
	}

	if err := proc.Start(); err != nil {
		log.Printf("resume: terminal start failed: %v", err)
		writeJSON(w, http.StatusOK, resumeResponse{
			Launched: false,
			Command:  cmd,
			Cwd:      sessionDir,
			Error:    "terminal_launch_failed",
		})
		return
	}

	// Detach — don't wait for the terminal process.
	go func() { _ = proc.Wait() }()

	writeJSON(w, http.StatusOK, resumeResponse{
		Launched: true,
		Terminal: termName,
		Command:  cmd,
		Cwd:      sessionDir,
	})
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
	script := cmd
	if cwd != "" {
		if info, err := os.Stat(cwd); err == nil && info.IsDir() {
			script = fmt.Sprintf("cd %s && %s", shellQuote(cwd), cmd)
		}
	}

	// Try iTerm2 first.
	if _, err := exec.LookPath("osascript"); err == nil {
		// Sanitize for AppleScript: escape backslashes, then quotes,
		// and reject newlines to prevent multi-line injection.
		safe := strings.NewReplacer(
			"\n", " ",
			"\r", " ",
			`\`, `\\`,
			`"`, `\"`,
		).Replace(script)

		// Check if iTerm is installed.
		iterm := "/Applications/iTerm.app"
		if _, err := os.Stat(iterm); err == nil {
			appleScript := fmt.Sprintf(
				`tell application "iTerm"
					create window with default profile command "%s"
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

func (s *Server) handleGetTerminalConfig(
	w http.ResponseWriter, _ *http.Request,
) {
	s.mu.RLock()
	tc := s.cfg.Terminal
	s.mu.RUnlock()
	if tc.Mode == "" {
		tc.Mode = "auto"
	}
	writeJSON(w, http.StatusOK, tc)
}

func (s *Server) handleSetTerminalConfig(
	w http.ResponseWriter, r *http.Request,
) {
	var tc config.TerminalConfig
	if err := json.NewDecoder(r.Body).Decode(&tc); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	switch tc.Mode {
	case "auto", "custom", "clipboard":
		// ok
	default:
		writeError(w, http.StatusBadRequest,
			`mode must be "auto", "custom", or "clipboard"`)
		return
	}

	if tc.Mode == "custom" && tc.CustomBin == "" {
		writeError(w, http.StatusBadRequest,
			`custom_bin is required when mode is "custom"`)
		return
	}

	// Only validate custom_args when mode is "custom" — stale
	// args from a previous config shouldn't block saving other modes.
	if tc.Mode == "custom" {
		if tc.CustomArgs != "" &&
			!strings.Contains(tc.CustomArgs, "{cmd}") {
			writeError(w, http.StatusBadRequest,
				`custom_args must contain the {cmd} placeholder so the `+
					`resume command is passed to the terminal`)
			return
		}
		if tc.CustomArgs != "" {
			if _, splitErr := shlex.Split(tc.CustomArgs); splitErr != nil {
				writeError(w, http.StatusBadRequest,
					fmt.Sprintf("custom_args has invalid shell syntax: %v", splitErr))
				return
			}
		}
	}

	s.mu.Lock()
	err := s.cfg.SaveTerminalConfig(tc)
	s.mu.Unlock()
	if err != nil {
		log.Printf("save terminal config: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, tc)
}

// readSessionCwd reads the first few lines of a session JSONL file
// and extracts the "cwd" field. Claude Code stores the working
// directory in early conversation entries; some agents (e.g. Codex)
// store it under payload.cwd. Returns "" if not found.
func readSessionCwd(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	reader := bufio.NewReader(f)
	for range 20 {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			s := string(line)
			if cwd := gjson.Get(s, "cwd").Str; cwd != "" {
				return cwd
			}
			if cwd := gjson.Get(s, "payload.cwd").Str; cwd != "" {
				return cwd
			}
			// Copilot stores cwd under data.context.cwd on the
			// session.start event.
			if cwd := gjson.Get(s, "data.context.cwd").Str; cwd != "" {
				return cwd
			}
		}
		if err != nil {
			break
		}
	}
	return ""
}

// resolveSessionDir determines the project directory for a session.
// It tries the session file's embedded cwd first, then falls back to
// the session's project field. Both candidates must be absolute paths
// pointing to existing directories.
func resolveSessionDir(session *db.Session) string {
	if session.FilePath != nil {
		if cwd := readSessionCwd(*session.FilePath); isDir(cwd) {
			return cwd
		}
	}
	if isDir(session.Project) {
		return session.Project
	}
	return ""
}

func isDir(path string) bool {
	if !filepath.IsAbs(path) {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
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
// opener kind is not "terminal" or the terminal is not supported.
func launchResumeInOpener(
	o Opener, cmd string, cwd string,
) *exec.Cmd {
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
	// that cd's and then runs the resume command.
	shellCmd := cmd
	if cwd != "" {
		shellCmd = fmt.Sprintf(
			"cd %s && %s", shellQuote(cwd), cmd,
		)
	}
	safe := escapeForAppleScript(shellCmd)

	switch o.ID {
	case "iterm2":
		script := fmt.Sprintf(
			`tell application "iTerm"
				create window with default profile command "%s"
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
