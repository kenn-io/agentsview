// ABOUTME: session command group root — programmatic CLI
// ABOUTME: surface for the SessionService interface.
package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
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
	registerFormatFlags(cmd.PersistentFlags())
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
	tr, err := ensureTransport(&cfg, transportIntentRead, 0)
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
	return resolveWritableServiceWithIntent(cmd, false)
}

func resolveFreshWritableService(
	cmd *cobra.Command,
) (service.SessionService, func(), error) {
	return resolveWritableServiceWithIntent(cmd, true)
}

func resolveWritableServiceWithIntent(
	cmd *cobra.Command, fresh bool,
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
	var tr transport
	if fresh {
		tr, err = ensureTransport(&cfg, transportIntentArchiveWrite, 0)
	} else {
		tr, err = detectTransport(cfg.DataDir, cfg.AuthToken, 0)
	}
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
		reason := tr.DirectReason
		if reason == "" {
			reason = "local daemon owns the SQLite archive but is not responding"
		}
		return nil, nil, errors.New(
			reason + "; refusing to write directly. Retry once the daemon " +
				"is reachable and compatible, or stop it to write locally",
		)
	}
	return syncService(cfg, tr)
}

func resolvePGReadConfig(
	cmd *cobra.Command, cfg config.Config,
) (config.PGConfig, bool, error) {
	if !pgReadRequested(cmd) {
		return config.PGConfig{}, false, nil
	}
	pgCfg, err := cfg.ResolvePG()
	if err != nil {
		return config.PGConfig{}, false,
			fmt.Errorf("resolving pg config: %w", err)
	}
	if pgCfg.URL == "" {
		return config.PGConfig{}, false, errors.New(
			"pg url not configured; set AGENTSVIEW_PG_URL, use a legacy [pg].url, or configure default_pg with named [pg.NAME] targets",
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

// formatFlag restricts --format to "human" or "json", so a typo fails at
// parse time rather than silently degrading to human output. Type returns
// the allowed values, which --help renders as `--format human|json`.
type formatFlag string

func (f *formatFlag) String() string { return string(*f) }

func (f *formatFlag) Set(v string) error {
	switch v {
	case "human", "json":
		*f = formatFlag(v)
		return nil
	default:
		return errors.New("must be human or json")
	}
}

func (*formatFlag) Type() string { return "human|json" }

// registerFormatFlags installs the --format/--json pair shared by every
// machine-readable command. Read the result with outputFormat.
func registerFormatFlags(flags *pflag.FlagSet) {
	f := formatFlag("human")
	flags.Var(&f, "format", "Output format: human or json")
	flags.Bool("json", false, "Emit JSON output (alias for --format json)")
}

// rejectFormatFlags errors when --format or --json was set on a command
// that streams a fixed format: such commands inherit the pair from the
// session group but cannot honor it.
func rejectFormatFlags(cmd *cobra.Command, cmdName, streams string) error {
	if cmd.Flags().Changed("format") || cmd.Flags().Changed("json") {
		return fmt.Errorf(
			"%s: streams %s; --format/--json not supported",
			cmdName, streams,
		)
	}
	return nil
}

// outputFormat resolves the output format to "human" or "json". The
// --json alias wins when set; otherwise the --format value, default
// "human".
func outputFormat(cmd *cobra.Command) string {
	if jsonOutput, _ := cmd.Flags().GetBool("json"); jsonOutput {
		return "json"
	}
	if f := cmd.Flag("format"); f != nil {
		return f.Value.String()
	}
	return "human"
}
