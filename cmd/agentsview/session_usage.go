// ABOUTME: `session usage <id>` subcommand — prints per-session
// ABOUTME: token statistics and a cost estimate (JSON or human).
package main

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/apiclient"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/server"
	"go.kenn.io/agentsview/internal/service"
	"go.kenn.io/agentsview/internal/servicehttp"
)

var sessionUsageHTTPClient = &http.Client{Timeout: 30 * time.Second}

func newSessionUsageCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "usage <id>",
		Short: "Show token usage and cost estimate for a session",
		Long: "Show token usage and cost estimate for a session.\n\n" +
			"Totals include archived subagent usage. Source refresh can find " +
			"Claude subagent transcripts beside the named session. " +
			"--own-only reports the named transcript's usage.\n\n" +
			"Missing daemon IDs return a retry error while startup ingestion or an archive rebuild is pending; " +
			"otherwise they return exit 2. " +
			"--no-sync and HTTP --own-only read only the archive; missing IDs return exit 2. " +
			"If refresh is refused or fails, the command shows archived usage with a warning; " +
			"JSON sets refresh_incomplete. Direct mode keeps its targeted source lookup.",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		Run: func(cmd *cobra.Command, args []string) {
			runSessionUsage(cmd, args[0], outputFormat(cmd))
		},
	}
	cmd.Flags().Bool("no-sync", false,
		"Use archived usage without synchronizing source transcripts")
	cmd.Flags().Bool("own-only", false,
		"Report only this session's own usage, excluding subagents")
	return cmd
}

// runSessionUsage computes usage for one session and renders it,
// exiting with the shared usage exit code (0 = token data or cost,
// 2 = not found, 3 = neither). Uses Run + os.Exit (not RunE) so the
// 2/3 codes survive — cobra RunE errors collapse to exit 1.
func runSessionUsage(cmd *cobra.Command, sessionID, format string) {
	out, code, err := sessionUsageDataForCommand(cmd, sessionID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(tokenUseExitErr)
	}
	if out != nil {
		if format == "json" {
			enc := jsontext.NewEncoder(os.Stdout, jsontext.WithIndent("  "))
			if encErr := json.MarshalEncode(enc, out); encErr != nil {
				fmt.Fprintf(os.Stderr, "error: %v\n", encErr)
				os.Exit(tokenUseExitErr)
			}
		} else if rerr := renderSessionUsageHuman(
			os.Stdout, out,
		); rerr != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", rerr)
			os.Exit(tokenUseExitErr)
		}
	}
	os.Exit(code)
}

func sessionUsageDataForCommand(
	cmd *cobra.Command, sessionID string,
) (*sessionUsageOutput, int, error) {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	ownOnly, _ := cmd.Flags().GetBool("own-only")
	noSync, _ := cmd.Flags().GetBool("no-sync")
	query := sessionUsageQuery{
		SessionID: sessionID, OwnOnly: ownOnly, NoSync: noSync,
	}

	remote, _ := cmd.Flags().GetString("server")
	if remote != "" {
		if pgReadRequested(cmd) {
			return nil, tokenUseExitErr, errors.New("--server and --pg are mutually exclusive")
		}
		token, err := explicitServerToken(cmd)
		if err != nil {
			return nil, tokenUseExitErr, err
		}
		if !query.OwnOnly {
			if err := requireRemoteSubagentUsageSupport(
				ctx, remote, token, !query.NoSync,
			); err != nil {
				return nil, tokenUseExitErr, err
			}
		}
		return httpSessionUsageData(ctx, remote, token, query)
	}
	cfg, err := config.LoadPFlags(cmd.Flags())
	if err != nil {
		return nil, tokenUseExitErr, fmt.Errorf("loading config: %w", err)
	}
	if pgReadRequested(cmd) {
		pgCfg, _, err := resolvePGReadConfig(cmd, cfg)
		if err != nil {
			return nil, tokenUseExitErr, err
		}
		return pgSessionUsageData(cfg, pgCfg, query)
	}
	backend, cleanup, err := resolveArchiveQueryBackendWithConfig(
		ctx,
		cfg,
		archiveQueryPolicy{
			NoSync:               noSync,
			AutoStart:            true,
			SkipInitialSync:      true,
			ReadOnlyDaemon:       archiveQueryUseReadOnlyDaemon,
			DirectReadOnlyAction: "refresh session usage directly",
		},
	)
	if err != nil {
		return nil, tokenUseExitErr, err
	}
	defer closeArchiveQueryBackend(cleanup)
	return backend.SessionUsage(ctx, query)
}

func requireRemoteSubagentUsageSupport(
	ctx context.Context, baseURL, token string, refresh bool,
) error {
	capabilities, err := servicehttp.ProbeHTTPServerCapabilities(
		ctx, baseURL, token,
	)
	if err != nil {
		return err
	}
	if capabilities.APIVersion < server.SubagentUsageAPIVersion {
		return fmt.Errorf(
			"server API version %d does not support combined subagent usage; "+
				"upgrade or restart the server, or pass --own-only",
			capabilities.APIVersion,
		)
	}
	if refresh && capabilities.APIVersion < server.UsageRefreshAPIVersion {
		return fmt.Errorf(
			"server API version %d does not support usage refresh without waiting for ingestion; "+
				"upgrade or restart the server, or pass --no-sync",
			capabilities.APIVersion,
		)
	}
	return nil
}

func readOnlySessionUsageDaemonError(url string) error {
	return fmt.Errorf(
		"daemon at %s is read-only; use --pg to query "+
			"a read-only mirror, or stop it to refresh local "+
			"session usage",
		url,
	)
}

func httpSessionUsageData(
	ctx context.Context,
	baseURL string,
	token string,
	query sessionUsageQuery,
) (*sessionUsageOutput, int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	sessionID := query.SessionID
	backend := servicehttp.NewHTTPBackend(baseURL, token, false, "")
	resolvedID, err := resolveServiceSessionID(ctx, backend, sessionID)
	if err != nil {
		if !errors.Is(err, errSessionNotFound) {
			return nil, tokenUseExitErr, err
		}
		if query.OwnOnly || query.NoSync {
			fmt.Fprintf(os.Stderr, "session not found: %s\n", sessionID)
			return nil, tokenUseExitNotFound, nil
		}
		startupPending, archiveStale, statusErr := httpSessionSyncStatus(
			ctx, baseURL, token,
		)
		if statusErr != nil {
			return nil, tokenUseExitErr, fmt.Errorf(
				"checking sync status: %w", statusErr,
			)
		}
		if startupPending || archiveStale {
			return nil, tokenUseExitErr, errors.New(
				"session usage is not yet available; retry after startup ingestion " +
					"or the archive rebuild finishes",
			)
		}
		// Startup or rebuild work can finish after the first lookup but before
		// the status request. Resolve once more before declaring the ID absent.
		resolvedID, err = resolveServiceSessionID(ctx, backend, sessionID)
		if err != nil {
			if !errors.Is(err, errSessionNotFound) {
				return nil, tokenUseExitErr, err
			}
			fmt.Fprintf(os.Stderr, "session not found: %s\n", sessionID)
			return nil, tokenUseExitNotFound, nil
		}
	}
	refreshIncomplete := false
	if !query.OwnOnly && !query.NoSync {
		_, syncErr := backend.Sync(ctx, service.SyncInput{
			ID: resolvedID, Subagents: true, UsageRefresh: true,
		})
		if syncErr != nil {
			if !errors.Is(syncErr, db.ErrReadOnly) {
				refreshIncomplete = true
				fmt.Fprintf(os.Stderr,
					"warning: usage refresh incomplete; showing usage currently stored in the archive: %v\n", syncErr)
			}
		}
	}
	// Request the full breakdown so the remote path matches the
	// shape returned by the direct store paths, and the same subagent
	// attribution scope so --server and local agree field for field.
	api, err := apiclient.NewHTTPClient(baseURL, token, sessionUsageHTTPClient)
	if err != nil {
		return nil, tokenUseExitErr, err
	}
	var subagents *bool
	if !query.OwnOnly {
		subagents = new(true)
	}
	response, err := api.GetAPIV1SessionsIDUsageWithResponse(ctx, &apiclient.GetAPIV1SessionsIDUsageRequestOptions{
		PathParams: &apiclient.GetAPIV1SessionsIDUsagePath{ID: url.PathEscape(resolvedID)},
		Query:      &apiclient.GetAPIV1SessionsIDUsageQuery{Breakdown: new(true), Subagents: subagents},
	})
	if response == nil {
		return nil, tokenUseExitErr, err
	}
	resp := response.HTTPResponse
	if resp.StatusCode == http.StatusNotFound {
		fmt.Fprintf(os.Stderr, "session not found: %s\n", sessionID)
		return nil, tokenUseExitNotFound, nil
	}
	if resp.StatusCode != http.StatusOK {
		body := response.Body
		return nil, tokenUseExitErr, fmt.Errorf(
			"usage: HTTP %d: %s", resp.StatusCode, body,
		)
	}
	if err != nil {
		return nil, tokenUseExitErr, err
	}
	if len(response.Body) == 0 {
		return nil, tokenUseExitErr, io.ErrUnexpectedEOF
	}
	wire := response.JSON200
	out := sessionUsageOutput{
		SessionID: wire.SessionID, Agent: wire.Agent, Project: wire.Project,
		TotalOutputTokens: int(wire.TotalOutputTokens), PeakContextTokens: int(wire.PeakContextTokens),
		HasTokenData: wire.HasTokenData, Cost: wire.Cost, HasCost: wire.HasCost, CostUSD: wire.CostUsd,
		Models: wire.Models, UnpricedModels: wire.UnpricedModels,
		BreakdownCount: int(wire.BreakdownCount), Breakdown: wire.Breakdown,
		ServerRunning: true, RefreshIncomplete: refreshIncomplete,
	}
	if wire.CostSource != nil {
		out.CostSource = export.CostSource(*wire.CostSource)
	}
	if wire.AiCredits != nil {
		out.AICredits = *wire.AiCredits
	}
	if wire.SubagentCount != nil {
		out.SubagentCount = int(*wire.SubagentCount)
	}
	return &out, usageExitCode(&out.SessionUsage), nil
}

func httpSessionSyncStatus(
	ctx context.Context, baseURL, token string,
) (startupPending, archiveStale bool, err error) {
	api, err := apiclient.NewHTTPClient(baseURL, token, sessionUsageHTTPClient)
	if err != nil {
		return false, false, err
	}
	response, err := api.GetAPIV1SyncStatusWithResponse(ctx)
	if err != nil {
		return false, false, err
	}
	if response == nil {
		return false, false, io.ErrUnexpectedEOF
	}
	if response.JSON200 == nil {
		return false, false, fmt.Errorf(
			"sync status: HTTP %d: %s", response.StatusCode, response.Body,
		)
	}
	return response.JSON200.StartupPending, response.JSON200.ArchiveStale, nil
}

func pgSessionUsageData(
	cfg config.Config, pgCfg config.PGConfig, query sessionUsageQuery,
) (*sessionUsageOutput, int, error) {
	store, cleanup, err := openPGReadStore(cfg, pgCfg)
	if err != nil {
		if cleanup != nil {
			cleanup()
		}
		return nil, tokenUseExitErr, fmt.Errorf("opening pg store: %w", err)
	}
	if cleanup != nil {
		defer cleanup()
	}
	return storeSessionUsageData("pg", cfg, store, query)
}

func storeSessionUsageData(
	storeName string,
	cfg config.Config,
	store db.Store,
	query sessionUsageQuery,
) (*sessionUsageOutput, int, error) {
	if len(cfg.CustomModelPricing) > 0 {
		if priced, ok := store.(customPricingStore); ok {
			priced.SetCustomPricing(cfg.CustomModelPricing)
		}
	}

	ctx := context.Background()
	sessionID := query.SessionID
	resolvedID, err := resolveStoreSessionID(ctx, store, sessionID)
	if err != nil {
		if !errors.Is(err, errSessionNotFound) {
			return nil, tokenUseExitErr,
				fmt.Errorf("resolving %s session id: %w", storeName, err)
		}
		fmt.Fprintf(os.Stderr, "session not found: %s\n", sessionID)
		return nil, tokenUseExitNotFound, nil
	}

	var u *db.SessionUsage
	if query.OwnOnly {
		u, err = store.GetSessionUsage(ctx, resolvedID, true)
	} else {
		u, err = service.SessionUsageWithSubagents(
			ctx, store, resolvedID, true)
	}
	if err != nil {
		return nil, tokenUseExitErr,
			fmt.Errorf("querying %s session usage: %w", storeName, err)
	}
	if u == nil {
		fmt.Fprintf(os.Stderr, "session not found: %s\n", sessionID)
		return nil, tokenUseExitNotFound, nil
	}
	if u.Agent == "" {
		if def, ok := parser.AgentByPrefix(u.SessionID); ok {
			u.Agent = string(def.Type)
		}
	}
	return &sessionUsageOutput{
		SessionUsage:  *u,
		ServerRunning: false,
	}, usageExitCode(u), nil
}

func resolveStoreSessionID(
	ctx context.Context, store db.Store, sessionID string,
) (string, error) {
	matches, err := store.FindSessionIDsByRawSuffix(
		ctx, sessionID, tokenUseResolveMatchLimit,
	)
	if err != nil {
		return "", err
	}
	if len(matches) > 0 {
		if matches[0] == sessionID {
			return sessionID, nil
		}
		if len(matches) > 1 {
			fmt.Fprintf(os.Stderr,
				"warning: ambiguous session id %q matches "+
					"multiple sessions, using most recent (%s)\n",
				sessionID, matches[0],
			)
		}
		return matches[0], nil
	}
	return resolveServiceSessionID(
		ctx, service.NewReadOnlyBackend(store), sessionID,
	)
}

// renderSessionUsageHuman writes a compact key/value summary. The
// cost line shows "~$X.XX (models)" when a complete estimate exists,
// otherwise "n/a" (noting any unpriced models). The tilde marks the
// figure as a model-pricing estimate. A "Subagents" line appears only
// when the figures cover subagent transcripts, so the reader can tell
// why the total exceeds what the session's own file records.
func renderSessionUsageHuman(w io.Writer, out *sessionUsageOutput) error {
	label := func(name string) string {
		return fmt.Sprintf("%-14s", name+":")
	}
	fmt.Fprintf(w, "%s %s\n", label("Session"),
		sanitizeTerminal(out.SessionID))
	fmt.Fprintf(w, "%s %s\n", label("Agent"),
		sanitizeTerminal(out.Agent))
	fmt.Fprintf(w, "%s %d\n", label("Output"), out.TotalOutputTokens)
	fmt.Fprintf(w, "%s %d\n", label("Peak ctx"), out.PeakContextTokens)
	if out.SubagentCount > 0 {
		fmt.Fprintf(w, "%s %d (included)\n", label("Subagents"),
			out.SubagentCount)
	}
	if out.HasCost {
		models := strings.Join(out.Models, ", ")
		prefix := "~"
		if out.CostSource == export.CostSourceReported {
			prefix = ""
		}
		suffix := ""
		if models != "" {
			suffix = " (" + sanitizeTerminal(models) + ")"
		}
		fmt.Fprintf(w, "%s %s%s%s\n", label("Cost"), prefix,
			money.FormatUSD(out.Cost, money.DisplayCents), suffix)
	} else if len(out.UnpricedModels) > 0 {
		fmt.Fprintf(w, "%s n/a (unpriced: %s)\n", label("Cost"),
			sanitizeTerminal(strings.Join(out.UnpricedModels, ", ")))
	} else {
		fmt.Fprintf(w, "%s n/a\n", label("Cost"))
	}
	if out.AICredits > 0 {
		fmt.Fprintf(w, "%s %.0f\n", label("AI Credits"), out.AICredits)
	}
	if out.RefreshIncomplete {
		fmt.Fprintln(w, "Usage refresh: incomplete; showing archived totals")
	}
	return nil
}
