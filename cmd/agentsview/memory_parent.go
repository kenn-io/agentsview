// ABOUTME: Records which Claude Code or Codex session launched a new session
// ABOUTME: when the new session's SessionStart hook runs.
package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/mattn/go-sqlite3"
	"github.com/shirou/gopsutil/v4/process"
	"github.com/spf13/cobra"
	"go.kenn.io/kit/agenthook"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/gittrailer"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/pathutil"
	"go.kenn.io/agentsview/internal/service"
)

const (
	// launcherAncestryLimit bounds the walk from the hook to the launcher:
	// hook shell, child agent, its wrapper, the launcher's shell, launcher.
	launcherAncestryLimit  = 32
	launcherHookInputLimit = 1 << 20

	launchParentRetryDelay    = 100 * time.Millisecond
	launchParentRetryMaxDelay = 400 * time.Millisecond
)

// launchProcess is the part of a process the launcher walk reads.
type launchProcess struct {
	Name    string
	PPID    int32
	Cmdline func(context.Context) ([]string, error)
	Environ func(context.Context) ([]string, error)
	Cwd     func(context.Context) (string, error)
}

var (
	lookupLaunchProcess    = lookupGopsutilProcess
	resolveLaunchAnnotator = func(ctx context.Context, cmd *cobra.Command) (service.SessionAnnotator, func(), error) {
		return resolveSessionAnnotatorContext(ctx, cmd, true)
	}
)

func lookupGopsutilProcess(ctx context.Context, pid int32) (launchProcess, error) {
	p, err := process.NewProcessWithContext(ctx, pid)
	if err != nil {
		return launchProcess{}, err
	}
	name, err := p.NameWithContext(ctx)
	if err != nil {
		return launchProcess{}, err
	}
	ppid, err := p.PpidWithContext(ctx)
	if err != nil {
		return launchProcess{}, err
	}
	return launchProcess{Name: name, PPID: ppid, Cmdline: p.CmdlineSliceWithContext, Environ: p.EnvironWithContext, Cwd: p.CwdWithContext}, nil
}

// codexSQLiteOverride reads the last CLI override, using Codex's TOML-or-string parsing.
func codexSQLiteOverride(ctx context.Context, p launchProcess) (string, error) {
	if p.Cmdline == nil {
		return "", nil
	}
	args, err := p.Cmdline(ctx)
	if err != nil {
		return "", err
	}
	var home string
	for i := 1; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		var override string
		switch {
		case arg == "-c" || arg == "--config":
			if i+1 == len(args) {
				break
			}
			i++
			override = args[i]
		case strings.HasPrefix(arg, "--config="):
			override = strings.TrimPrefix(arg, "--config=")
		case strings.HasPrefix(arg, "-c") && len(arg) > 2:
			override = strings.TrimPrefix(strings.TrimPrefix(arg, "-c"), "=")
		}
		key, value, ok := strings.Cut(override, "=")
		if !ok || strings.TrimSpace(key) != "sqlite_home" {
			continue
		}
		var parsed struct{ Value any }
		if _, err := toml.Decode("value = "+value, &parsed); err != nil {
			home = value
		} else if path, ok := parsed.Value.(string); ok {
			home = path
		} else {
			return "", fmt.Errorf("sqlite_home override must be a string")
		}
	}
	return home, nil
}

// processAgent names the agent a process runs, or "" for any other process.
func processAgent(name string) string {
	switch strings.TrimSuffix(strings.ToLower(name), ".exe") {
	case "claude":
		return "claude"
	case "codex":
		return "codex"
	}
	return ""
}

// oneShotRun reports whether the agent's command line is a one-shot run
// (claude -p, claude --print, codex exec) rather than an interactive session.
func oneShotRun(ctx context.Context, p launchProcess, agent string) bool {
	if p.Cmdline == nil {
		return false
	}
	args, err := p.Cmdline(ctx)
	if err != nil {
		return false
	}
	for i := 1; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return false
		}
		if agent == "codex" && (arg == "exec" || arg == "e") || agent == "claude" && (arg == "-p" || arg == "--print") {
			return true
		}
		if !strings.HasPrefix(arg, "-") {
			if agent == "codex" {
				return false
			}
			continue
		}
		if !strings.Contains(arg, "=") {
			if agent == "codex" {
				switch arg {
				case "-C", "--cd", "-c", "--config", "-m", "--model", "-p", "--profile", "-s", "--sandbox", "-a", "--ask-for-approval", "-i", "--image", "--add-dir", "--enable", "--disable", "--remote", "--remote-auth-token-env", "--local-provider":
					i++
				}
			} else {
				switch arg {
				case "--model", "--add-dir", "--append-system-prompt", "--append-system-prompt-file", "--system-prompt", "--system-prompt-file", "--agent", "--agents", "--settings", "--setting-sources", "--session-id", "--permission-mode", "--output-format", "--input-format", "--json-schema", "--effort", "--fallback-model", "--name", "-n", "--max-budget-usd", "--mcp-config", "--plugin-dir", "--tools", "--allowedTools", "--allowed-tools", "--disallowedTools", "--disallowed-tools":
					i++
				}
			}
		}
	}
	return false
}

// launcherVariables lists the variables an agent sets for commands it runs,
// in gittrailer.Discover's order of preference.
var launcherVariables = map[string][]string{
	"claude": {"CLAUDE_CODE_SESSION_ID"},
	"codex":  {"CODEX_THREAD_ID", "CODEX_SESSION_ID"},
}

// launchAgents finds the agent running this hook and the agent that started
// it. The child's environment is what the launcher's shell passed to it.
func launchAgents(ctx context.Context, pid int32) (child launchProcess, childAgent string, launcher launchProcess, launcherAgent string, err error) {
	for range launcherAncestryLimit {
		if pid <= 1 {
			break
		}
		p, err := lookupLaunchProcess(ctx, pid)
		if err != nil {
			return child, childAgent, launcher, "", err
		}
		if agent := processAgent(p.Name); agent != "" {
			if childAgent == "" {
				child, childAgent = p, agent
			} else {
				return child, childAgent, p, agent, nil
			}
		}
		pid = p.PPID
	}
	return child, childAgent, launcher, "", nil
}

func processEnv(ctx context.Context, p launchProcess) (map[string]string, error) {
	values := map[string]string{}
	if p.Environ == nil {
		return values, nil
	}
	env, err := p.Environ(ctx)
	if err != nil {
		return values, err
	}
	for _, entry := range env {
		name, value, _ := strings.Cut(entry, "=")
		values[name] = value
	}
	return values, nil
}

// launcherSessionID reads the launcher's ID from the child's starting environment.
func launcherSessionID(env map[string]string, launcher string) (string, error) {
	values := map[string]string{}
	for _, name := range launcherVariables[launcher] {
		values[name] = env[name]
	}
	session, err := gittrailer.Discover(func(name string) string { return values[name] })
	if err != nil || session.Agent != launcher {
		return "", err
	}
	return session.ID, nil
}

func launcherArchiveID(ctx context.Context, home, agent, native string) (string, error) {
	if agent == "codex" {
		key, err := codexIndexedPage(ctx, home, native)
		if err != nil || key == "" {
			return "", err
		}
		return "codex:" + key, nil
	}
	return native, nil
}

// storedSessionID validates a native ID and adds the agent's archive prefix.
func storedSessionID(agent, id string) (string, error) {
	session, err := gittrailer.Discover(func(name string) string {
		switch name {
		case "AGENTSVIEW_SESSION_AGENT":
			return agent
		case "AGENTSVIEW_SESSION_ID":
			return id
		}
		return ""
	})
	if err != nil {
		return "", err
	}
	def, ok := parser.AgentByType(parser.AgentType(agent))
	if !ok {
		return "", fmt.Errorf("unknown agent %q", agent)
	}
	return def.IDPrefix + session.ID, nil
}

type sessionStartCapture struct {
	agenthook.NoopHandler
	input *agenthook.SessionStartInput
}

func (c *sessionStartCapture) SessionStart(
	_ context.Context, input agenthook.SessionStartInput,
) (agenthook.SessionStartOutput, error) {
	c.input = &input
	return agenthook.SessionStartOutput{}, nil
}

// readHookInput returns stdin. A harness closes stdin after writing the
// payload; the deadline covers one that doesn't, and the abandoned read ends
// with the process.
func readHookInput(ctx context.Context, r io.Reader) ([]byte, error) {
	type result struct {
		data []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		data, err := io.ReadAll(io.LimitReader(r, launcherHookInputLimit))
		done <- result{data, err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res := <-done:
		return res.data, res.err
	}
}

// captureLaunchParent links a newly started session to the Claude Code or
// Codex session whose command started it. It records nothing for a session
// with no agent launcher, and only on startup, so a resume or clear cannot
// restore a link the user removed.
func captureLaunchParent(ctx context.Context, cmd *cobra.Command) error {
	child, childAgent, launcherProcess, launcher, err := launchAgents(ctx, int32(os.Getppid()))
	if childAgent == "" || !oneShotRun(ctx, child, childAgent) {
		return err
	}
	env, envErr := processEnv(ctx, child)
	if envErr != nil {
		if launcher != "" {
			return nil
		}
		return envErr
	}
	if launcher == "" {
		claude := env["CLAUDE_CODE_SESSION_ID"] != ""
		codex := env["CODEX_THREAD_ID"] != "" || env["CODEX_SESSION_ID"] != ""
		if claude == codex {
			return nil
		}
		if claude {
			launcher = "claude"
		} else {
			launcher = "codex"
		}
	}
	native, err := launcherSessionID(env, launcher)
	if err != nil || native == "" {
		return err
	}
	launcherEnv := env
	if launcherProcess.Environ != nil {
		if values, err := processEnv(ctx, launcherProcess); err == nil {
			launcherEnv = values
		}
	}
	home := launcherEnv["CODEX_HOME"]
	if home == "" {
		if userHome, err := os.UserHomeDir(); err == nil {
			home = filepath.Join(userHome, ".codex")
		}
	}
	sqliteHome := launcherEnv["CODEX_SQLITE_HOME"]
	if launcher == "codex" && home != "" {
		var config struct {
			SQLiteHome string `toml:"sqlite_home"`
		}
		if _, err := toml.DecodeFile(filepath.Join(home, "config.toml"), &config); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if config.SQLiteHome != "" {
			sqliteHome, err = pathutil.ExpandHome(config.SQLiteHome)
			if err != nil {
				return err
			}
			if !filepath.IsAbs(sqliteHome) {
				sqliteHome = filepath.Join(home, sqliteHome)
			}
		}
		override, err := codexSQLiteOverride(ctx, launcherProcess)
		if err != nil {
			return err
		}
		if override != "" {
			sqliteHome = override
		}
	}
	if sqliteHome != "" {
		sqliteHome, err = pathutil.ExpandHome(sqliteHome)
		if err != nil {
			return err
		}
		if !filepath.IsAbs(sqliteHome) {
			process := launcherProcess
			if process.Cwd == nil {
				process = child
			}
			if process.Cwd == nil {
				return nil
			}
			cwd, err := process.Cwd(ctx)
			if err != nil {
				return err
			}
			sqliteHome = filepath.Join(cwd, sqliteHome)
		}
		home = sqliteHome
	}

	data, err := readHookInput(ctx, cmd.InOrStdin())
	if err != nil || len(data) == 0 {
		return err
	}
	capture := &sessionStartCapture{}
	if err := agenthook.Handle(
		ctx, agenthook.Agent(childAgent), bytes.NewReader(data), io.Discard, capture,
	); err != nil {
		return err
	}
	in := capture.input
	if in == nil || in.Source != agenthook.SessionSourceStartup || in.AgentID != "" {
		return nil
	}
	childID, err := storedSessionID(childAgent, in.SessionID)
	if err != nil {
		return err
	}
	return recordLaunchParent(ctx, cmd, childID, home, launcher, native)
}

// recordLaunchParent retries a busy archive or an unreachable daemon until
// the hook's deadline, including a Codex index not yet written,
// since no later event writes the link. It gives up at once on a link the
// archive rejects or a read-only backend.
func recordLaunchParent(ctx context.Context, cmd *cobra.Command, childID, home, launcher, native string) error {
	delay := launchParentRetryDelay
	var parentID string
	for {
		var err error
		if parentID == "" {
			parentID, err = launcherArchiveID(ctx, home, launcher, native)
			missing := errors.Is(err, os.ErrNotExist) || errors.Is(err, sql.ErrNoRows)
			sqliteErr, isSQLite := errors.AsType[sqlite3.Error](err)
			if err != nil && !missing && (!isSQLite || (sqliteErr.Code != sqlite3.ErrBusy && sqliteErr.Code != sqlite3.ErrLocked)) {
				return err
			}
		}
		if err == nil && parentID != "" {
			err = writeLaunchParent(ctx, cmd, childID, parentID)
		}
		if err == nil || errors.Is(err, db.ErrSessionExternalParentInvalid) || errors.Is(err, db.ErrReadOnly) {
			return err
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		case <-timer.C:
		}
		delay = min(2*delay, launchParentRetryMaxDelay)
	}
}

func writeLaunchParent(ctx context.Context, cmd *cobra.Command, childID, parentID string) error {
	annotator, cleanup, err := resolveLaunchAnnotator(ctx, cmd)
	if err != nil {
		return err
	}
	defer cleanup()
	if _, err := annotator.SetSessionParent(ctx, childID, parentID); err != nil {
		return fmt.Errorf("recording launcher: %w", err)
	}
	return nil
}

// codexIndexedPage reads the current rollout from Codex's thread index.
func codexIndexedPage(ctx context.Context, home, thread string) (string, error) {
	if home == "" {
		if userHome, err := os.UserHomeDir(); err == nil {
			home = filepath.Join(userHome, ".codex")
		}
	}
	if home == "" {
		return "", nil
	}
	return parser.CodexIndexedSessionKey(ctx, home, thread)
}
