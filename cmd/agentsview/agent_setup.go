package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/kit/agentmcp"
	"go.kenn.io/kit/agentsetup"

	"go.kenn.io/agentsview/internal/skills"
)

func newAgentCommand() *cobra.Command {
	group := &cobra.Command{Use: "agent", Short: "Set up coding agents", GroupID: groupMeta}
	var force bool
	var cmd *cobra.Command
	cmd = agentsetup.NewCommand(agentsetup.Options{SetupCommand: "agentsview agent setup", Items: func(agent string) ([]agentsetup.Item, error) {
		remote, err := skillRemoteFromFlags(cmd)
		if err != nil {
			return nil, err
		}
		if !remote.explicit && strings.TrimSpace(os.Getenv("AGENTSVIEW_MEMORY_SERVER")) == "" && strings.TrimSpace(os.Getenv("AGENTSVIEW_MEMORY_SERVER_TOKEN_FILE")) != "" {
			return nil, errors.New("AGENTSVIEW_MEMORY_SERVER_TOKEN_FILE requires AGENTSVIEW_MEMORY_SERVER")
		}
		if !remote.explicit && strings.TrimSpace(os.Getenv("AGENTSVIEW_MEMORY_SERVER")) != "" {
			remote.flag = skills.Remote{Server: strings.TrimSpace(os.Getenv("AGENTSVIEW_MEMORY_SERVER")), TokenFile: strings.TrimSpace(os.Getenv("AGENTSVIEW_MEMORY_SERVER_TOKEN_FILE"))}
			remote.explicit = true
			if err := remote.flag.Validate(); err != nil {
				return nil, err
			}
		}
		return setupItems(agent, remote, force)
	}})
	cmd.Flags().BoolVar(&force, "force", false, "Overwrite modified skills and conflicting MCP entries")
	cmd.Flags().String("server", "", "Remote daemon URL")
	cmd.Flags().String("server-token-file", "", "File containing the remote daemon token")
	group.AddCommand(cmd)
	return group
}

func setupItems(agent string, source skillRemoteSource, force bool) ([]agentsetup.Item, error) {
	var harness skills.Harness
	switch agent {
	case "claude":
		harness = skills.HarnessClaude
	case "codex":
		harness = skills.HarnessAgents
	default:
		return nil, errors.New("agent setup supports Claude Code and Codex")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, errors.New("resolve home directory")
	}
	body, err := readSkillFile(filepath.Join(skills.TargetDir(harness, home), skillFileName))
	if err != nil {
		return nil, errors.New("read installed skill")
	}
	remote := source.resolve(body)
	pkg, err := skills.RenderPackage(harness, version, remote)
	if err != nil {
		return nil, errors.New("render skill")
	}
	server := agentmcp.Server{Command: "agentsview", Args: []string{"mcp", "--profile", "memory"}}
	if remote.Server != "" {
		server.Args = append(server.Args, "--server", remote.Server)
	}
	if remote.TokenFile != "" {
		server.Args = append(server.Args, "--server-token-file", remote.TokenFile)
	}
	mcp := agentsetup.MCP(agent, "agentsview", server)
	mcp.Install = nil
	checkMCP := mcp.Check
	var cached *agentsetup.Status
	mcp.Check = func(ctx context.Context) (agentsetup.Status, error) {
		if cached != nil {
			return *cached, nil
		}
		status, err := checkMCP(ctx)
		if err != nil {
			return agentsetup.Status{State: agentsetup.StateFailed, Reason: "Unable to inspect MCP configuration", NextStep: "agentsview agent setup"}, nil
		}
		status.ConfigPath = ""
		switch status.State {
		case agentsetup.StateMissing:
			status.Reason, status.NextStep = "MCP server is missing", "agentsview agent setup"
		case agentsetup.StateOutdated:
			status.Reason, status.NextStep = "MCP entry differs from requested setup", "Remove it in the client or run agentsview agent setup --force"
		case agentsetup.StateFailed, agentsetup.StateUnknown:
			if strings.Contains(strings.ToLower(status.Reason), "connection refused") {
				status.Reason, status.NextStep = "MCP connection refused", "Check that the server is running, then retry"
			} else {
				status.Reason, status.NextStep = "MCP client check could not verify the server", "agentsview agent setup --check"
			}
		}
		return status, nil
	}
	checkSkills := func(context.Context) (agentsetup.Status, error) {
		state := agentsetup.StateOK
		for _, rendered := range pkg {
			existing, err := readSkillFile(filepath.Join(home, rendered.RelativePath))
			if err != nil {
				return agentsetup.Status{State: agentsetup.StateFailed, Reason: "Unable to read skill files"}, nil
			}
			switch skills.Classify(existing, rendered) {
			case skills.StateMissing:
				state = agentsetup.StateMissing
			case skills.StateStale:
				if state == agentsetup.StateOK {
					state = agentsetup.StateOutdated
				}
			case skills.StateModified, skills.StateForeign:
				return agentsetup.Status{State: agentsetup.StateModified, Reason: "Skill files were edited", NextStep: "agentsview agent setup --force"}, nil
			}
		}
		return agentsetup.Status{State: state}, nil
	}
	skill := agentsetup.Item{Name: "skill", Check: checkSkills}
	mcp.Install = func(ctx context.Context) error {
		status, err := checkSkills(ctx)
		if err != nil {
			return err
		}
		if status.State == agentsetup.StateFailed {
			return errors.New(status.Reason)
		}
		if status.State == agentsetup.StateModified && !force {
			return errors.New("Skill files were edited; use --force to overwrite")
		}
		plan, err := agentmcp.PlanInstall(agentmcp.Agent(agent), agentmcp.InstallOptions{Name: "agentsview", Server: server})
		if err != nil {
			return errors.New("Unable to inspect MCP configuration")
		}
		mcpStatus, err := mcp.Check(ctx)
		if err != nil {
			return err
		}
		if mcpStatus.State == agentsetup.StateOutdated && !force {
			cached = &mcpStatus
			return errors.New("MCP entry differs; remove it in the client or use --force")
		}
		if mcpStatus.State == agentsetup.StateUnknown && plan.Changed {
			return errors.New("Unable to inspect MCP configuration")
		}
		if err := runSkillsInstall(io.Discard, []skills.Harness{harness}, home, force, source); err != nil {
			return errors.New("Unable to install skill files")
		}
		if plan.Changed {
			if _, err := agentmcp.Install(agentmcp.Agent(agent), agentmcp.InstallOptions{Name: "agentsview", Server: server}); err != nil {
				return errors.New("Unable to install MCP configuration; rerun setup")
			}
		} else {
			cached = &mcpStatus
		}
		return nil
	}
	return []agentsetup.Item{skill, mcp}, nil
}
