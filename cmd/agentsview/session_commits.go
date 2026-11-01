package main

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/service"
	"go.kenn.io/agentsview/internal/servicehttp"
)

func newSessionCommitsCommand() *cobra.Command {
	return &cobra.Command{Use: "commits <id>", Short: "Show the stored commits linked to a session", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		svc, cleanup, err := resolveCommitLookupService(cmd)
		if err != nil {
			return err
		}
		defer cleanup()
		rows, err := svc.SessionCommits(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		if outputFormat(cmd) == "json" {
			return json.MarshalEncode(jsontext.NewEncoder(cmd.OutOrStdout(), jsontext.WithIndent("  ")), service.SessionCommitsResponse{Commits: rows})
		}
		for _, r := range rows {
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", sanitizeTerminal(r.CommitSHA), r.Source, sanitizeTerminal(r.Subject)); err != nil {
				return err
			}
		}
		return nil
	}}
}

// Provenance reads use discovery only: no config migration, daemon lifecycle or
// source refresh. Keep this independent from the usual auto-starting resolver.
func resolveCommitLookupService(cmd *cobra.Command) (service.SessionService, func(), error) {
	if remote, _ := cmd.Flags().GetString("server"); remote != "" {
		if pgReadRequested(cmd) {
			return nil, nil, errors.New("--server and --pg are mutually exclusive")
		}
		token, err := explicitServerToken(cmd)
		if err != nil {
			return nil, nil, err
		}
		return servicehttp.NewHTTPBackend(remote, token, true, ""), func() {}, nil
	}
	dataDir, _ := cmd.Flags().GetString("data-dir")
	var cfg config.Config
	var err error
	if dataDir != "" {
		cfg, err = config.LoadReadOnlyAt(dataDir)
	} else {
		cfg, err = config.LoadReadOnlyFor(config.ScopeLocal)
	}
	if err != nil {
		return nil, nil, err
	}
	pgCfg, usePG, err := resolvePGReadConfig(cmd, cfg)
	if err != nil {
		return nil, nil, err
	}
	if usePG {
		return newPGReadService(cfg, pgCfg)
	}
	return newCommitLookupService(cmd.Context(), cfg)
}

func newCommitLookupService(ctx context.Context, cfg config.Config) (service.SessionService, func(), error) {
	tr, err := detectClientTransportContext(ctx, &cfg, 0)
	if err != nil {
		return nil, nil, err
	}
	if tr.Mode == transportHTTP {
		return servicehttp.NewHTTPBackend(tr.URL, cfg.AuthToken, true, tr.BrowserURL), func() {}, nil
	}
	if err := directIncompatibleDaemonError(tr); err != nil {
		return nil, nil, err
	}
	d, err := openReadOnlyDB(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	return service.NewReadOnlyBackend(d), func() { _ = d.Close() }, nil
}
