// ABOUTME: session command group root — programmatic CLI
// ABOUTME: surface for the SessionService interface.
package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/service"
)

func newSessionCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "session",
		Short:        "Programmatic access to session data",
		GroupID:      groupData,
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	cmd.PersistentFlags().String(
		"format", "human",
		"Output format: human or json",
	)
	cmd.PersistentFlags().Bool(
		"json", false,
		"Emit JSON output (alias for --format json)",
	)
	cmd.PersistentFlags().String(
		"server", "",
		"Remote daemon URL",
	)
	cmd.PersistentFlags().String(
		"server-token-file", "",
		"File containing bearer token for explicit --server requests",
	)
	cmd.PersistentFlags().Bool(
		"pg", false,
		"Read session data from configured PostgreSQL",
	)

	cmd.AddCommand(newSessionGetCommand())
	cmd.AddCommand(newSessionUsageCommand())
	cmd.AddCommand(newSessionListCommand())
	cmd.AddCommand(newSessionMessagesCommand())
	cmd.AddCommand(newSessionToolCallsCommand())
	cmd.AddCommand(newSessionExportCommand())
	cmd.AddCommand(newSessionSyncCommand())
	cmd.AddCommand(newSessionWatchCommand())
	cmd.AddCommand(newSessionSearchCommand())
	return cmd
}

// resolveService constructs the SessionService matching the
// current transport: HTTP when a daemon is discoverable, direct
// SQLite otherwise. Callers MUST defer the returned cleanup.
func resolveService(
	cmd *cobra.Command,
) (service.SessionService, func(), error) {
	remote, _ := cmd.Flags().GetString("server")
	if remote != "" {
		if pgReadRequested(cmd) {
			return nil, nil, errors.New(
				"--server and --pg are mutually exclusive",
			)
		}
		token, err := explicitServerToken(cmd)
		if err != nil {
			return nil, nil, err
		}
		return service.NewHTTPBackend(remote, token, false),
			func() {}, nil
	}
	cfg, err := config.LoadPFlags(cmd.Flags())
	if err != nil {
		return nil, nil, fmt.Errorf(
			"loading config: %w", err,
		)
	}
	pgCfg, usePG, err := resolvePGReadConfig(cmd, cfg)
	if err != nil {
		return nil, nil, err
	}
	if usePG {
		return newPGReadService(cfg, pgCfg)
	}
	tr, err := detectTransport(cfg.DataDir, cfg.AuthToken, 0)
	if err != nil {
		return nil, nil, err
	}
	return newService(cfg, tr)
}

// resolveWritableService constructs a write-capable SessionService:
// HTTP when a writable daemon is reachable, otherwise a direct
// backend wired with a real sync.Engine. It refuses read-only
// daemons (pg serve) and unreachable writable daemons. Callers MUST defer the returned
// cleanup. Read-only commands should use resolveService instead.
func resolveWritableService(
	cmd *cobra.Command,
) (service.SessionService, func(), error) {
	if remote, _ := cmd.Flags().GetString("server"); remote != "" {
		if pgReadRequested(cmd) {
			return nil, nil, errors.New(
				"--server and --pg are mutually exclusive",
			)
		}
		token, err := explicitServerToken(cmd)
		if err != nil {
			return nil, nil, err
		}
		return service.NewHTTPBackend(remote, token, false),
			func() {}, nil
	}
	if pgReadRequested(cmd) {
		return nil, nil, errors.New(
			"--pg is read-only and cannot be used with write commands",
		)
	}
	cfg, err := config.LoadPFlags(cmd.Flags())
	if err != nil {
		return nil, nil, fmt.Errorf("loading config: %w", err)
	}
	tr, err := detectTransport(cfg.DataDir, cfg.AuthToken, 0)
	if err != nil {
		return nil, nil, err
	}
	if tr.Mode == transportHTTP && tr.ReadOnly {
		return nil, nil, fmt.Errorf(
			"daemon at %s is read-only (pg serve); cannot write: stop "+
				"'pg serve' and use the local DB, or start a local daemon",
			tr.URL,
		)
	}
	if tr.Mode == transportDirect && tr.DirectReadOnly {
		return nil, nil, errors.New(
			"local daemon owns the SQLite archive but is not responding; " +
				"refusing to write directly. Retry once the daemon is " +
				"reachable, or stop it to write locally",
		)
	}
	return syncService(cfg, tr)
}

func resolvePGReadConfig(
	cmd *cobra.Command, cfg config.Config,
) (config.PGConfig, bool, error) {
	requested := pgReadRequested(cmd)
	if !requested && cfg.PG.URL == "" {
		return config.PGConfig{}, false, nil
	}
	pgCfg, err := cfg.ResolvePG()
	if err != nil {
		return config.PGConfig{}, false,
			fmt.Errorf("resolving pg config: %w", err)
	}
	if pgCfg.URL == "" {
		return config.PGConfig{}, false, errors.New(
			"pg url not configured; set AGENTSVIEW_PG_URL or [pg].url",
		)
	}
	return pgCfg, true, nil
}

func pgReadRequested(cmd *cobra.Command) bool {
	if cmd == nil {
		return false
	}
	v, err := cmd.Flags().GetBool("pg")
	return err == nil && v
}

func explicitServerToken(cmd *cobra.Command) (string, error) {
	if cmd == nil {
		return "", nil
	}
	path, err := cmd.Flags().GetString("server-token-file")
	if err == nil && strings.TrimSpace(path) != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("reading --server-token-file: %w", err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	return strings.TrimSpace(os.Getenv("AGENTSVIEW_SERVER_TOKEN")), nil
}

// outputFormat returns the requested --format flag value
// ("human" or "json"). Defaults to "human".
func outputFormat(cmd *cobra.Command) string {
	jsonOutput, _ := cmd.Flags().GetBool("json")
	if jsonOutput {
		return "json"
	}
	v, _ := cmd.Flags().GetString("format")
	if v == "" {
		return "human"
	}
	return v
}
