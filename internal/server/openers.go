package server

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Opener represents an application that can open a project directory.
type Opener struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"` // "editor", "terminal", "files", "action"
	Bin  string `json:"bin"`
}

// openerCandidate is a binary we check for on PATH, or a macOS
// app bundle we check for on disk.
type openerCandidate struct {
	id      string
	name    string
	kind    string
	bins    []string // alternatives to try via LookPath
	appPath string   // macOS .app bundle path (checked via os.Stat)
}

// Linux + cross-platform candidates.
var openerCandidates = []openerCandidate{
	// File managers
	{"nautilus", "Files", "files", []string{"nautilus"}, ""},
	{"dolphin", "Dolphin", "files", []string{"dolphin"}, ""},
	{"thunar", "Thunar", "files", []string{"thunar"}, ""},
	{"nemo", "Nemo", "files", []string{"nemo"}, ""},
	{"pcmanfm", "PCManFM", "files", []string{"pcmanfm"}, ""},

	// Editors / IDEs (GUI only — CLI editors like vim need a TTY)
	{"cursor", "Cursor", "editor", []string{"cursor"}, ""},
	{"vscode", "VS Code", "editor", []string{"code"}, ""},
	{"zed", "Zed", "editor", []string{"zed"}, ""},
	{"sublime", "Sublime Text", "editor", []string{"subl"}, ""},
	{"emacs", "Emacs", "editor", []string{"emacs"}, ""},
	{"intellij", "IntelliJ IDEA", "editor", []string{"idea"}, ""},
	{"goland", "GoLand", "editor", []string{"goland"}, ""},
	{"webstorm", "WebStorm", "editor", []string{"webstorm"}, ""},

	// Terminals
	{"ghostty", "Ghostty", "terminal", []string{"ghostty"}, ""},
	{"kitty", "kitty", "terminal", []string{"kitty"}, ""},
	{"alacritty", "Alacritty", "terminal", []string{"alacritty"}, ""},
	{"wezterm", "WezTerm", "terminal", []string{"wezterm"}, ""},
	{"gnome-terminal", "GNOME Terminal", "terminal", []string{"gnome-terminal"}, ""},
	{"konsole", "Konsole", "terminal", []string{"konsole"}, ""},
	{"xfce4-terminal", "Xfce Terminal", "terminal", []string{"xfce4-terminal"}, ""},
	{"tilix", "Tilix", "terminal", []string{"tilix"}, ""},
	{"xterm", "xterm", "terminal", []string{"xterm"}, ""},
}

// macOS-specific candidates.
var darwinOpenerCandidates = []openerCandidate{
	// File manager is always Finder on macOS — use "open" command
	{"finder", "Finder", "files", []string{"open"}, ""},

	// Editors (GUI only — CLI editors like vim need a TTY)
	{"cursor", "Cursor", "editor", []string{"cursor"}, ""},
	{"vscode", "VS Code", "editor", []string{"code"}, ""},
	{"zed", "Zed", "editor", []string{"zed"}, ""},
	{"xcode", "Xcode", "editor", []string{"xed"}, ""},
	{"sublime", "Sublime Text", "editor", []string{"subl"}, ""},

	// Claude Desktop — detected via app bundle. Uses claude:// URL
	// scheme for session handoff rather than terminal launch.
	{"claude-desktop", "Claude Desktop", "action", nil, "/Applications/Claude.app"},

	// Terminals — Ghostty, iTerm2, kitty, and Terminal.app are macOS
	// GUI apps; detect via app bundle first, fall back to PATH binary
	// for non-default installs (e.g. Homebrew formula).
	{"ghostty", "Ghostty", "terminal", []string{
		"ghostty",
		"/Applications/Ghostty.app/Contents/MacOS/ghostty",
	}, ""},
	{"iterm2", "iTerm2", "terminal", nil, "/Applications/iTerm.app"},
	{"kitty", "kitty", "terminal", []string{"kitty"}, "/Applications/kitty.app"},
	{"alacritty", "Alacritty", "terminal", []string{"alacritty"}, ""},
	{"wezterm", "WezTerm", "terminal", []string{"wezterm"}, ""},
	{"terminal", "Terminal", "terminal", nil, "/System/Applications/Utilities/Terminal.app"},
}

// cachedOpeners stores detected openers with a TTL.
var (
	openerCache    []Opener
	openerCacheMu  sync.Mutex
	openerCacheAt  time.Time
	openerCacheTTL = 60 * time.Second
)

func detectOpeners() []Opener {
	openerCacheMu.Lock()
	defer openerCacheMu.Unlock()

	if time.Since(openerCacheAt) < openerCacheTTL && openerCache != nil {
		return openerCache
	}

	candidates := openerCandidates
	if runtime.GOOS == "darwin" {
		candidates = darwinOpenerCandidates
	}

	var result []Opener
	for _, c := range candidates {
		// Try app bundle path first (macOS GUI apps).
		if c.appPath != "" {
			if _, err := os.Stat(c.appPath); err == nil {
				result = append(result, Opener{
					ID:   c.id,
					Name: c.name,
					Kind: c.kind,
					Bin:  c.appPath,
				})
				continue
			}
		}
		// Fall back to PATH-based binary detection.
		for _, bin := range c.bins {
			path, err := exec.LookPath(bin)
			if err != nil {
				continue
			}
			result = append(result, Opener{
				ID:   c.id,
				Name: c.name,
				Kind: c.kind,
				Bin:  path,
			})
			break // found one binary for this candidate
		}
	}

	openerCache = result
	openerCacheAt = time.Now()
	return result
}

type openRequest struct {
	OpenerID string `json:"opener_id"`
}

func launchOpener(ctx context.Context, o Opener, dir string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// The opened application belongs to the user after this request returns.
	ctx = context.WithoutCancel(ctx)
	var cmd *exec.Cmd

	switch o.Kind {
	case "files":
		if runtime.GOOS == "darwin" {
			cmd = exec.CommandContext(ctx, "open", dir)
		} else {
			cmd = exec.CommandContext(ctx, o.Bin, dir)
		}
	case "editor":
		cmd = exec.CommandContext(ctx, o.Bin, dir)
	case "terminal":
		cmd = launchTerminalInDir(ctx, o, dir)
	default:
		return fmt.Errorf("unsupported opener kind: %s", o.Kind)
	}

	if cmd == nil {
		return fmt.Errorf("could not build command for opener %s", o.Name)
	}

	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.Stdin = nil

	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

// isAppBundle returns true if the path looks like a macOS .app bundle.
func isAppBundle(bin string) bool {
	return strings.HasSuffix(bin, ".app")
}

// macExecCommand builds an exec.Cmd for macOS. If bin is an .app
// bundle it wraps with `open -na`; otherwise executes the binary
// directly. This keeps detection and launch consistent regardless
// of whether the opener was found via app bundle or PATH.
func macExecCommand(ctx context.Context, bin string, args ...string) *exec.Cmd {
	if isAppBundle(bin) {
		openArgs := []string{"-na", bin, "--args"}
		openArgs = append(openArgs, args...)
		return exec.CommandContext(ctx, "open", openArgs...)
	}
	return exec.CommandContext(ctx, bin, args...)
}

func launchTerminalInDir(ctx context.Context, o Opener, dir string) *exec.Cmd {
	if runtime.GOOS == "darwin" {
		switch o.ID {
		case "iterm2":
			shellCmd := fmt.Sprintf(
				"cd %s && exec bash", shellQuote(dir),
			)
			script := fmt.Sprintf(
				`tell application "iTerm"
					create window with default profile command "%s"
				end tell`,
				escapeForAppleScript(shellCmd),
			)
			return exec.CommandContext(ctx, "osascript", "-e", script)
		case "terminal":
			shellCmd := "cd " + shellQuote(dir)
			script := fmt.Sprintf(
				`tell application "Terminal"
					activate
					do script "%s"
				end tell`,
				escapeForAppleScript(shellCmd),
			)
			return exec.CommandContext(ctx, "osascript", "-e", script)
		case "ghostty":
			return macExecCommand(ctx, o.Bin,
				"--working-directory="+dir)
		case "kitty":
			return macExecCommand(ctx, o.Bin, "-d", dir)
		case "alacritty":
			return macExecCommand(ctx, o.Bin,
				"--working-directory", dir)
		case "wezterm":
			return macExecCommand(ctx, o.Bin,
				"start", "--cwd", dir)
		}
	}

	// Linux: launch via CLI binary directly.
	switch o.ID {
	case "kitty":
		return exec.CommandContext(ctx, o.Bin, "--directory", dir)
	case "alacritty":
		return exec.CommandContext(ctx, o.Bin, "--working-directory", dir)
	case "wezterm":
		return exec.CommandContext(ctx, o.Bin, "start", "--cwd", dir)
	case "gnome-terminal":
		return exec.CommandContext(ctx, o.Bin, "--working-directory="+dir)
	case "konsole":
		return exec.CommandContext(ctx, o.Bin, "--workdir", dir)
	case "xfce4-terminal":
		return exec.CommandContext(ctx, o.Bin,
			"--default-working-directory="+dir)
	case "tilix":
		return exec.CommandContext(ctx, o.Bin, "--working-directory="+dir)
	case "ghostty":
		return exec.CommandContext(ctx, o.Bin, "--working-directory="+dir)
	default:
		cmd := exec.CommandContext(ctx, o.Bin)
		cmd.Dir = dir
		return cmd
	}
}

// escapeForAppleScript escapes a string for embedding inside an
// AppleScript double-quoted string literal. Does NOT add outer quotes.
func escapeForAppleScript(s string) string {
	return strings.NewReplacer(
		"\n", " ",
		"\r", " ",
		`\`, `\\`,
		`"`, `\"`,
	).Replace(s)
}
