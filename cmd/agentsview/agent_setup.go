package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"go.kenn.io/kit/agentmcp"
	"go.kenn.io/kit/agentsetup"

	"go.kenn.io/agentsview/internal/skills"
)

func newAgentCommand() *cobra.Command {
	group := &cobra.Command{Use: "agent", Short: "Set up coding agents", GroupID: groupMeta}
	var force bool
	cmd := agentsetup.NewCommand(agentsetup.Options{SetupCommand: "agentsview agent setup", Items: func(agent string) ([]agentsetup.Item, error) {
		return setupItems(agent, force)
	}})
	cmd.Flags().BoolVar(&force, "force", false, "Overwrite modified skills")
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
	body, err := readSkillFile(filepath.Join(skills.TargetDir(harness, home), skillFileName))
	if err != nil {
		return nil, errors.New("read installed skill")
	}
	remote := (skillRemoteSource{}).resolve(body)
	pkg, err := skills.RenderPackage(harness, version, remote)
	if err != nil {
		return nil, errors.New("render skill")
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
	skill := agentsetup.Item{Name: "skill", Check: checkSkills, Install: func(ctx context.Context) error {
		status, err := checkSkills(ctx)
		if err != nil {
			return err
		}
		if status.State == agentsetup.StateFailed {
			return errors.New(status.Reason)
		}
		if status.State == agentsetup.StateOK && !force {
			return nil
		}
		return runSkillsInstall(io.Discard, []skills.Harness{harness}, home, force, skillRemoteSource{})
	}}
	return []agentsetup.Item{skill, setupMCP(agent)}, nil
}

func setupMCP(agent string) agentsetup.Item {
	return agentsetup.MCP(agent, "agentsview", agentmcp.Server{Command: "agentsview", Args: []string{"mcp", "--profile", "memory"}})
}
