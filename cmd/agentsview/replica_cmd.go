package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/storage"
)

// newReplicaCommand builds the `<name>` command group for one replica with
// the shared push, status, and serve verbs. Backend-specific verbs are added
// by the caller.
func newReplicaCommand(backend storage.Replica, extra ...*cobra.Command) *cobra.Command {
	cmd := &cobra.Command{
		Use:          backend.Name(),
		Short:        backend.DisplayName() + " sync and serve commands",
		GroupID:      groupData,
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newReplicaPushCommand(backend))
	cmd.AddCommand(newReplicaStatusCommand(backend))
	cmd.AddCommand(newReplicaServeCommand(backend))
	cmd.AddCommand(extra...)
	return cmd
}

func newReplicaPushCommand(backend storage.Replica) *cobra.Command {
	name := backend.Name()
	display := backend.DisplayName()
	var cfg ReplicaPushConfig
	cmd := &cobra.Command{
		Use:          "push [target]",
		Short:        "Push local data to " + display,
		SilenceUsage: true,
		Args:         cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			targetName := ""
			if len(args) == 1 {
				targetName = args[0]
			}
			if cfg.AllTargets && cfg.Watch {
				return fmt.Errorf(
					"%s push --watch: %w", name,
					errors.New("--all cannot be combined with --watch"),
				)
			}
			if cfg.Watch {
				if err := runReplicaPushWatch(backend, cfg, targetName); err != nil {
					return fmt.Errorf("%s push --watch: %w", name, err)
				}
				return nil
			}
			if cmd.Flags().Changed("debounce") || cmd.Flags().Changed("interval") {
				fmt.Fprintln(os.Stderr,
					"warning: --debounce and --interval have no effect without --watch")
			}
			if err := runReplicaPush(backend, cfg, targetName); err != nil {
				return fmt.Errorf("%s push: %w", name, err)
			}
			return nil
		},
	}
	tag := replicaFlagTag(backend)
	cmd.Flags().BoolVar(&cfg.AllTargets, "all", false, "Push every configured "+tag+" target sequentially")
	cmd.Flags().BoolVar(&cfg.Full, "full", false, "Force full local resync and "+tag+" push")
	cmd.Flags().StringVar(&cfg.ProjectsFlag, "projects", "", "Comma-separated list of projects to push (inclusive)")
	cmd.Flags().StringVar(&cfg.ExcludeProjects, "exclude-projects", "", "Comma-separated list of projects to exclude from push")
	cmd.Flags().BoolVar(&cfg.AllProjects, "all-projects", false, "Ignore configured project filters for this run")
	cmd.Flags().BoolVar(&cfg.Watch, "watch", false, "Run continuously, pushing on change plus a periodic floor")
	cmd.Flags().DurationVar(&cfg.Debounce, "debounce", defaultWatchDebounce, "Coalesce window after a change before pushing (--watch only)")
	cmd.Flags().DurationVar(&cfg.Interval, "interval", defaultWatchInterval, "Periodic floor push interval (--watch only)")
	cmd.Flags().BoolVar(&cfg.NoVectors, "no-vectors", false, "Skip pushing semantic-search vectors")
	return cmd
}

func newReplicaStatusCommand(backend storage.Replica) *cobra.Command {
	name := backend.Name()
	tag := replicaFlagTag(backend)
	var cfg ReplicaStatusConfig
	cmd := &cobra.Command{
		Use:          "status [target]",
		Short:        "Show " + tag + " sync status",
		SilenceUsage: true,
		Args:         cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			targetName := ""
			if len(args) == 1 {
				targetName = args[0]
			}
			if err := runReplicaStatus(cmd.Context(), backend, targetName, cfg); err != nil {
				return fmt.Errorf("%s status: %w", name, err)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&cfg.AllTargets, "all", false, "Show status for every configured "+tag+" target")
	cmd.Flags().StringVar(&cfg.ProjectsFlag, "projects", "", "Comma-separated list of projects whose push status to show")
	cmd.Flags().StringVar(&cfg.ExcludeProjects, "exclude-projects", "", "Comma-separated list of excluded projects whose push status to show")
	cmd.Flags().BoolVar(&cfg.AllProjects, "all-projects", false, "Ignore configured project filters for this status")
	return cmd
}

func newReplicaServeCommand(backend storage.Replica) *cobra.Command {
	cmd := &cobra.Command{
		Use:          "serve",
		Short:        "Serve from " + backend.DisplayName() + " (read-only)",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			appCfg, basePath, err := loadReplicaServeConfig(cmd)
			if err != nil {
				fatal("%v", err)
			}
			runReplicaServe(backend, appCfg, basePath)
		},
	}
	cmd.Flags().String(
		"base-path",
		"",
		"URL prefix for reverse-proxy subpath (e.g. /agentsview)",
	)
	config.RegisterServePFlags(cmd.Flags())
	return cmd
}

// replicaFlagTag is the short upper-case backend tag used in flag help and
// status output, e.g. "PG".
func replicaFlagTag(backend storage.Replica) string {
	return strings.ToUpper(backend.Name())
}
