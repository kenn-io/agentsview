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
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/process"
	"github.com/spf13/cobra"
	"go.kenn.io/kit/agenthook"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/gittrailer"
	"go.kenn.io/agentsview/internal/parser"
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
	Name      string
	PPID      int32
	Environ   func(context.Context) ([]string, error)
	OpenFiles func(context.Context) ([]process.OpenFilesStat, error)
}

var (
	lookupLaunchProcess    = lookupGopsutilProcess
	resolveLaunchAnnotator = resolveSessionAnnotatorContext
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
	return launchProcess{Name: name, PPID: ppid, Environ: p.EnvironWithContext, OpenFiles: p.OpenFilesWithContext}, nil
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

// launcherVariables lists the variables an agent sets for commands it runs,
// in gittrailer.Discover's order of preference.
var launcherVariables = map[string][]string{
	"claude": {"CLAUDE_CODE_SESSION_ID"},
	"codex":  {"CODEX_THREAD_ID", "CODEX_SESSION_ID"},
}

// launchAgents finds the agent running this hook and the agent that started
// it. The child's environment is what the launcher's shell passed to it.
func launchAgents(ctx context.Context, pid int32) (child, parent launchProcess, childAgent, launcher string, err error) {
	for range launcherAncestryLimit {
		if pid <= 1 {
			break
		}
		p, err := lookupLaunchProcess(ctx, pid)
		if err != nil {
			return child, parent, childAgent, "", err
		}
		if agent := processAgent(p.Name); agent != "" {
			if childAgent == "" {
				child, childAgent = p, agent
			} else {
				return child, p, childAgent, agent, nil
			}
		}
		pid = p.PPID
	}
	return child, parent, childAgent, "", nil
}

// launcherSessionID reads the launcher's session ID from the child's starting
// environment, before the child supplies its own ID to the hook.
func launcherSessionID(ctx context.Context, child launchProcess, launcher string) (string, error) {
	var env []string
	if child.Environ != nil {
		env, _ = child.Environ(ctx)
	}
	values := map[string]string{}
	for _, entry := range env {
		name, value, _ := strings.Cut(entry, "=")
		for _, wanted := range launcherVariables[launcher] {
			if name == wanted {
				values[name] = value
			}
		}
	}
	session, err := gittrailer.Discover(func(name string) string { return values[name] })
	if err != nil || session.Agent != launcher {
		return "", err
	}
	return session.ID, nil
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

// readHookInput returns stdin, or nil when stdin is a terminal. A harness
// closes stdin after writing the payload; the deadline covers one that
// doesn't, and the abandoned read ends with the process.
func readHookInput(ctx context.Context, r io.Reader) ([]byte, error) {
	if f, ok := r.(*os.File); ok {
		if info, err := f.Stat(); err == nil && info.Mode()&os.ModeCharDevice != 0 {
			return nil, nil
		}
	}
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
	child, parent, childAgent, launcher, err := launchAgents(ctx, int32(os.Getppid()))
	if childAgent == "" {
		return err
	}
	ambiguous := false
	if launcher == "" {
		if child.Environ == nil {
			return err
		}
		env, envErr := child.Environ(ctx)
		if envErr != nil {
			return envErr
		}
		vars := map[string]string{}
		for _, entry := range env {
			key, value, _ := strings.Cut(entry, "=")
			vars[key] = value
		}
		claude := vars["CLAUDE_CODE_SESSION_ID"] != ""
		codex := vars["CODEX_THREAD_ID"] != "" || vars["CODEX_SESSION_ID"] != ""
		if !claude && !codex {
			return nil
		}
		if claude && codex {
			ambiguous = true
		} else if claude {
			launcher = "claude"
		} else {
			launcher = "codex"
		}
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
	if childAgent == "codex" {
		key := parser.CodexSessionUUIDFromPath(in.TranscriptPath)
		if key != "" && parser.CodexThreadIDFromSessionKey(key) == in.SessionID {
			childID = "codex:" + key
		}
	}
	parentID := ""
	if ambiguous {
		parentID, err = detachedLauncherParent(ctx, cmd, child, parent)
	} else {
		var nativeID string
		nativeID, err = launcherSessionID(ctx, child, launcher)
		if err == nil && nativeID != "" {
			parentID, err = storedSessionID(launcher, nativeID)
			if err == nil && launcher == "codex" {
				if key := codexLauncherPage(ctx, child, parent, nativeID); key != "" {
					parentID = "codex:" + key
				}
			}
		}
	}
	if err != nil || parentID == "" {
		return err
	}

	return recordLaunchParent(ctx, cmd, childID, parentID)
}

// detachedLauncherParent selects the candidate whose archived parent is the
// other candidate. Without that relationship the environment is ambiguous.
func detachedLauncherParent(ctx context.Context, cmd *cobra.Command, child, parent launchProcess) (string, error) {
	ids := make([]string, 0, 2)
	for _, agent := range []string{"claude", "codex"} {
		native, err := launcherSessionID(ctx, child, agent)
		if err != nil || native == "" {
			return "", err
		}
		id, err := storedSessionID(agent, native)
		if err != nil {
			return "", err
		}
		if agent == "codex" {
			if key := codexLauncherPage(ctx, child, parent, native); key != "" {
				id = "codex:" + key
			}
		}
		ids = append(ids, id)
	}
	annotator, cleanup, err := resolveLaunchAnnotator(ctx, cmd)
	if err != nil {
		return "", err
	}
	defer cleanup()
	reader, ok := annotator.(interface {
		Get(context.Context, string) (*service.SessionDetail, error)
	})
	if !ok {
		return "", nil
	}
	var found string
	for i, id := range ids {
		session, err := reader.Get(ctx, id)
		if err != nil {
			return "", err
		}
		if session != nil && session.ParentSessionID != nil && *session.ParentSessionID == ids[1-i] {
			if found != "" {
				return "", nil
			}
			found = id
		}
	}
	return found, nil
}

// recordLaunchParent retries a busy archive or an unreachable daemon until
// the hook's deadline, since no later event writes the link. It gives up at
// once on a link the archive rejects or a read-only backend.
func recordLaunchParent(ctx context.Context, cmd *cobra.Command, childID, parentID string) error {
	delay := launchParentRetryDelay
	for {
		err := writeLaunchParent(ctx, cmd, childID, parentID)
		if err == nil || errors.Is(err, db.ErrSessionExternalParentInvalid) || errors.Is(err, db.ErrReadOnly) {
			return err
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
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

// codexLauncherPage reads the current rollout from Codex's thread index,
// or from the launcher's open files when the index is unavailable.
func codexLauncherPage(ctx context.Context, child, parent launchProcess, thread string) string {
	if key := codexIndexedPage(ctx, child, thread); key != "" {
		return key
	}

	if parent.OpenFiles != nil {
		files, _ := parent.OpenFiles(ctx)
		found := ""
		for _, file := range files {
			key := parser.CodexSessionUUIDFromPath(file.Path)
			if key == "" || parser.CodexThreadIDFromSessionKey(key) != thread {
				continue
			}
			if found != "" && found != key {
				found = ""
				break
			}
			found = key
		}
		if found != "" {
			return found
		}
	}
	return ""
}

func codexIndexedPage(ctx context.Context, child launchProcess, thread string) string {
	var home string
	if child.Environ != nil {
		env, _ := child.Environ(ctx)
		for _, entry := range env {
			if key, value, ok := strings.Cut(entry, "="); ok && key == "CODEX_HOME" {
				home = value
				break
			}
		}
	}
	if home == "" {
		if userHome, err := os.UserHomeDir(); err == nil {
			home = filepath.Join(userHome, ".codex")
		}
	}
	if home == "" {
		return ""
	}
	paths, _ := filepath.Glob(filepath.Join(home, "state_*.sqlite"))
	latest := -1
	var state string
	for _, path := range paths {
		name := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(path), "state_"), ".sqlite")
		version, err := strconv.Atoi(name)
		if err == nil && version > latest {
			latest, state = version, path
		}
	}
	if state == "" {
		return ""
	}
	path := filepath.ToSlash(state)
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	uri := (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro&_busy_timeout=100"}).String()
	conn, err := sql.Open("sqlite3", uri)
	if err != nil {
		return ""
	}
	defer conn.Close()
	var rollout string
	if err := conn.QueryRowContext(ctx, "SELECT rollout_path FROM threads WHERE id = ?", thread).Scan(&rollout); err != nil {
		return ""
	}
	key := parser.CodexSessionUUIDFromPath(rollout)
	if parser.CodexThreadIDFromSessionKey(key) != thread {
		return ""
	}
	return key
}
