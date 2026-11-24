// ABOUTME: Records which Claude Code or Codex session launched a new session
// ABOUTME: when the new session's SessionStart hook runs.
package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/shirou/gopsutil/v4/process"
	"github.com/spf13/cobra"
	"go.kenn.io/kit/agenthook"

	"go.kenn.io/agentsview/internal/gittrailer"
	"go.kenn.io/agentsview/internal/parser"
)

const (
	// launcherAncestryLimit bounds the walk from the hook to the launcher:
	// hook shell, child agent, its wrapper, the launcher's shell, launcher.
	launcherAncestryLimit  = 32
	launcherHookInputLimit = 1 << 20
)

// launchProcess is the part of a process the launcher walk reads.
type launchProcess struct {
	Name    string
	PPID    int32
	Environ func(context.Context) ([]string, error)
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
	return launchProcess{Name: name, PPID: ppid, Environ: p.EnvironWithContext}, nil
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
func launchAgents(ctx context.Context, pid int32) (child launchProcess, childAgent, launcher string, err error) {
	for range launcherAncestryLimit {
		if pid <= 1 {
			break
		}
		p, err := lookupLaunchProcess(ctx, pid)
		if err != nil {
			return launchProcess{}, "", "", err
		}
		if agent := processAgent(p.Name); agent != "" {
			if childAgent == "" {
				child, childAgent = p, agent
			} else {
				return child, childAgent, agent, nil
			}
		}
		pid = p.PPID
	}
	return launchProcess{}, "", "", nil
}

// launcherSessionID reads the launcher's session ID from the child's starting
// environment. Without it, the hook's own environment works only when the
// agents differ, since an agent overwrites its own variable for its hooks.
func launcherSessionID(ctx context.Context, child launchProcess, childAgent, launcher string) (string, error) {
	var env []string
	if child.Environ != nil {
		env, _ = child.Environ(ctx)
	}
	values := map[string]string{}
	if env == nil && childAgent != launcher {
		for _, name := range launcherVariables[launcher] {
			values[name] = os.Getenv(name)
		}
	}
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
	child, childAgent, launcher, err := launchAgents(ctx, int32(os.Getppid()))
	if err != nil {
		return fmt.Errorf("reading process ancestry: %w", err)
	}
	if launcher == "" {
		return nil
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
	parentID, err := launcherSessionID(ctx, child, childAgent, launcher)
	if err != nil || parentID == "" {
		return err
	}
	childID, err := storedSessionID(childAgent, in.SessionID)
	if err != nil {
		return err
	}
	if parentID, err = storedSessionID(launcher, parentID); err != nil {
		return err
	}
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
