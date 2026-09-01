package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/config"
	tuiterm "go.kenn.io/agentsview/internal/tui"
)

var runTUI = tuiterm.Run

func newTUICommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "tui",
		Short:        "Browse and manage agent sessions in the terminal",
		GroupID:      groupCore,
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runTUICommand(cmd)
		},
	}
	cmd.Flags().String("server", "", "Remote daemon URL")
	cmd.Flags().String("server-token-file", "",
		"File containing bearer token for explicit --server requests")
	return cmd
}

func runTUICommand(cmd *cobra.Command) error {
	cfg, err := config.LoadPFlags(cmd.Flags())
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	statePath := filepath.Join(cfg.DataDir, "tui-state.json")
	remote, _ := cmd.Flags().GetString("server")
	if remote != "" {
		token, err := explicitServerToken(cmd)
		if err != nil {
			return err
		}
		return runTUI(cmd.Context(), tuiterm.Options{
			BaseURL: strings.TrimRight(remote, "/"), Token: token,
			ReadOnly: true, ResolveReadOnly: true, StatePath: statePath,
		})
	}

	cfg.SkipInitialSync = !cfg.NoSync
	tr, err := ensureTransportContext(
		cmd.Context(), &cfg, transportIntentArchiveWrite, 0,
	)
	if err != nil {
		return err
	}
	if tr.Mode != transportHTTP || tr.URL == "" {
		return errors.New("agentsview tui requires a daemon; refusing direct archive access")
	}
	return runTUI(cmd.Context(), tuiterm.Options{
		BaseURL: tr.URL, Token: cfg.AuthToken, ReadOnly: tr.ReadOnly,
		StartupSync: tr.Started && !cfg.NoSync && !tr.ReadOnly,
		StatePath:   statePath,
	})
}
