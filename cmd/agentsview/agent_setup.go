package main

import (
	"context"
	"errors"
	"io"
	"os"

	"github.com/spf13/cobra"
	"go.kenn.io/kit/agentmcp"
	"go.kenn.io/kit/agentsetup"

	"go.kenn.io/agentsview/internal/skills"
)

func newAgentCommand() *cobra.Command {
	group := &cobra.Command{Use: "agent", Short: "Set up coding agents", Long: "Connect Claude Code and Codex to conversation memory by installing skills and registering the agentsview MCP server.", GroupID: groupMeta}
	var force bool
	cmd := agentsetup.NewCommand(agentsetup.Options{
		SetupCommand: "agentsview agent setup",
		Items: func(agent string) ([]agentsetup.Item, error) {
			return setupItems(agent, force)
		},
	})
	format := formatFlag("human")
	cmd.Flags().Var(&format, "format", "Output format: human or json")
	cmd.PreRunE = func(cmd *cobra.Command, _ []string) error {
		if format == "json" {
			return cmd.Flags().Set("json", "true")
		}
		return nil
	}
	cmd.Flags().BoolVar(&force, "force", false,
		"Overwrite edited skills and a different MCP entry")
	group.AddCommand(cmd)
	return group
}

func setupItems(agent string, force bool) ([]agentsetup.Item, error) {
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
	remote, err := skillRemoteFromEnv()
	if err != nil {
		return nil, err
	}
	skillRemote := skillRemoteSource{env: remote}
	checkSkills := func(context.Context) (agentsetup.Status, error) {
		rows, err := listSkillRows(
			home, false, []skills.Harness{harness}, skillRemote)
		if err != nil {
			//nolint:nilerr // The failed status is included in the setup report.
			return agentsetup.Status{
				State:  agentsetup.StateFailed,
				Reason: "Unable to read skill files",
			}, nil
		}
		state := agentsetup.StateOK
		for _, row := range rows {
			switch row.State {
			case "missing":
				state = agentsetup.StateMissing
			case "stale":
				if state == agentsetup.StateOK {
					state = agentsetup.StateOutdated
				}
			case "modified", "foreign":
				return agentsetup.Status{
					State:    agentsetup.StateModified,
					Reason:   "Skill files were edited",
					NextStep: "agentsview agent setup --force",
				}, nil
			}
		}
		return agentsetup.Status{State: state}, nil
	}
	skill := agentsetup.Item{
		Name:  "skill",
		Check: checkSkills,
		Install: func(context.Context) error {
			return runSkillsInstall(
				io.Discard, []skills.Harness{harness}, home, force, skillRemote)
		},
	}
	mcp := setupMCP(agent)
	installMCP := mcp.Install
	mcp.Install = func(ctx context.Context) error {
		if !force {
			status, err := mcp.Check(ctx)
			if err != nil {
				return err
			}
			plan, err := agentmcp.PlanInstall(agentmcp.Agent(agent), agentmcp.InstallOptions{Name: "agentsview", Server: memoryMCPServer()})
			if err != nil {
				return err
			}
			if status.State != agentsetup.StateMissing && plan.Changed {
				return errors.New(
					"existing agentsview MCP entry differs; rerun with --force")
			}
		}
		return installMCP(ctx)
	}
	return []agentsetup.Item{skill, mcp}, nil
}

func setupMCP(agent string) agentsetup.Item {
	return agentsetup.MCP(agent, "agentsview", memoryMCPServer())
}

func memoryMCPServer() agentmcp.Server {
	return agentmcp.Server{
		Command: "agentsview",
		Args:    []string{"mcp", "--profile", "memory"},
	}
}
