// ABOUTME: `session usage <id>` subcommand — prints per-session
// ABOUTME: token statistics and a cost estimate (JSON or human).
package main

import (
	"cmp"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strconv"
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
	cfg, err := config.LoadPFlagsFor(cmd.Flags(), config.ScopeLocal)
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
	// The shared HTTP backend requests the full breakdown and the same
	// subagent scope as the direct store paths, so --server and local
	// agree field for field. The request keeps the CLI's usage timeout.
	usageCtx := ctx
	if timeout := sessionUsageHTTPClient.Timeout; timeout > 0 {
		var cancel context.CancelFunc
		usageCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	usage, err := backend.SessionUsage(usageCtx, service.SessionUsageRequest{
		ID: resolvedID, OwnOnly: query.OwnOnly,
	})
	if err != nil {
		return nil, tokenUseExitErr, fmt.Errorf("usage: %w", err)
	}
	if usage == nil {
		fmt.Fprintf(os.Stderr, "session not found: %s\n", sessionID)
		return nil, tokenUseExitNotFound, nil
	}
	out := &sessionUsageOutput{
		SessionUsageReport: *usage,
		ServerRunning:      true, RefreshIncomplete: refreshIncomplete,
	}
	return out, usageExitCode(out), nil
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

	u, err := service.LoadSessionUsage(ctx, store, service.SessionUsageRequest{
		ID: resolvedID, OwnOnly: query.OwnOnly,
	})
	if err != nil {
		return nil, tokenUseExitErr,
			fmt.Errorf("querying %s session usage: %w", storeName, err)
	}
	if u == nil {
		fmt.Fprintf(os.Stderr, "session not found: %s\n", sessionID)
		return nil, tokenUseExitNotFound, nil
	}
	out := &sessionUsageOutput{SessionUsageReport: *u}
	return out, usageExitCode(out), nil
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
	// A Codex cloud session that live capture never saw has only plan
	// usage; zeros would read as a session that used nothing. Plan figures
	// are shares of the plan's limits, never output or context tokens.
	planOnly := out.Agent == string(parser.AgentCodexCloud) && !out.HasTokenData && !out.HasCost
	if planOnly {
		fmt.Fprintf(w, "%s n/a\n", label("Output"))
		fmt.Fprintf(w, "%s n/a\n", label("Peak ctx"))
	} else {
		fmt.Fprintf(w, "%s %d\n", label("Output"), out.TotalOutputTokens)
		fmt.Fprintf(w, "%s %d\n", label("Peak ctx"), out.PeakContextTokens)
	}
	if out.SubagentCount > 0 {
		fmt.Fprintf(w, "%s %d (included)\n", label("Subagents"),
			out.SubagentCount)
	}
	switch {
	case planOnly && hasPlanFigures(out.Plan):
		fmt.Fprintf(w, "%s billed as plan usage\n", label("Cost"))
	case planOnly:
		fmt.Fprintf(w, "%s n/a\n", label("Cost"))
	case out.HasCost:
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
	case len(out.UnpricedModels) > 0:
		fmt.Fprintf(w, "%s n/a (unpriced: %s)\n", label("Cost"),
			sanitizeTerminal(strings.Join(out.UnpricedModels, ", ")))
	default:
		fmt.Fprintf(w, "%s n/a\n", label("Cost"))
	}
	if out.AICredits > 0 {
		fmt.Fprintf(w, "%s %.0f\n", label("AI Credits"), out.AICredits)
	}
	if out.RefreshIncomplete {
		fmt.Fprintln(w, "Usage refresh: incomplete; showing archived totals")
	}
	if line := planUsageLine(out.Agent, out.Plan); line != "" {
		fmt.Fprintf(w, "%s %s\n", label("Plan usage"), sanitizeTerminal(line))
	}
	if line := captureCoverageLine(out.Coverage, out.Plan); line != "" {
		fmt.Fprintf(w, "%s %s\n", label("Captured"), sanitizeTerminal(line))
	}
	return nil
}

// planUsageLine describes a session's plan usage in one line, "" for a
// session that is not a codex-cloud chat and has no plan usage.
func planUsageLine(agent string, plan *db.PlanUsageView) string {
	// A codex-cloud chat with nothing recorded yet has an empty row, so it
	// counts as plan usage; it reads the same as having no row.
	if plan == nil || (!hasPlanFigures(plan) && plan.OutcomeKind == "" &&
		plan.OutcomeAt == "" && plan.DataAsOf == "" && !plan.Final) {
		if agent == string(parser.AgentCodexCloud) {
			return "not checked yet; a sync checks it when Codex cloud import is on"
		}
		return ""
	}
	if plan.OutcomeKind == "counted_under" {
		return "counted under session " + plan.CountedUnder
	}
	if !hasPlanFigures(plan) {
		if msg := planOutcomeMessage(plan); msg != "" {
			return msg
		}
		if plan.Final {
			return "not reported by OpenAI"
		}
		return "not reported yet" + asOfLocal(cmp.Or(plan.OutcomeAt, plan.DataAsOf))
	}
	var parts []string
	if plan.WeeklyPercent != nil {
		parts = append(parts, planFigure(*plan.WeeklyPercent)+"% of weekly limit")
	}
	if plan.FiveHourPercent != nil {
		parts = append(parts, planFigure(*plan.FiveHourPercent)+"% of 5-hour limit")
	}
	if plan.Credits != nil {
		parts = append(parts, planCredits(*plan.Credits)+" credits")
	}
	line := strings.Join(parts, ", ") + asOfLocal(plan.DataAsOf)
	if plan.DataStatus == "partial" {
		if plan.Final {
			line = "partial: " + line
		} else {
			line = "still updating: " + line
		}
	}
	// The stored outcome is newer than the figures, so the figures may
	// be stale; say what the later check found.
	if plan.OutcomeKind != "" {
		line += "; later, " + laterPlanCheck(plan.OutcomeKind)
		if at := localTime(plan.OutcomeAt); at != "" {
			line += " (" + at + ")"
		}
		line += ", so these figures may be out of date"
	}
	return line
}

// laterPlanCheck says what a plan usage check after the stored figures
// found.
func laterPlanCheck(kind string) string {
	switch kind {
	case "could_not_check":
		return "AgentsView could not check plan usage"
	case "account_unavailable":
		return "usage became unavailable for this account"
	case "too_many_descendants":
		return "the chat had too many sub-agent chats for OpenAI's usage report"
	case "settled_no_row":
		return "OpenAI stopped reporting this chat"
	default:
		return "OpenAI's usage report left this chat out"
	}
}

// planOutcomeMessage describes a plan usage outcome that ends a root's
// checks without a row, or "" for the other outcomes.
func planOutcomeMessage(plan *db.PlanUsageView) string {
	switch plan.OutcomeKind {
	case "account_unavailable":
		return "usage isn't available for this account"
	case "too_many_descendants":
		return "too many sub-agent chats for OpenAI's usage report"
	case "could_not_check":
		return "AgentsView could not check plan usage"
	case "settled_no_row":
		return "not reported by OpenAI"
	default:
		return ""
	}
}

// hasPlanFigures reports whether a plan usage row carries any figure.
func hasPlanFigures(plan *db.PlanUsageView) bool {
	return plan != nil && (plan.WeeklyPercent != nil || plan.FiveHourPercent != nil || plan.Credits != nil)
}

// planFigure rounds a plan usage percentage or credit amount to two
// decimal places. A nonzero amount that rounds to zero prints as "<0.01",
// so a small share never reads as none.
func planFigure(v float64) string {
	rounded := math.Round(v*100) / 100
	if rounded == 0 && v > 0 {
		return "<0.01"
	}
	return strconv.FormatFloat(rounded, 'f', -1, 64)
}

// planCredits formats OpenAI's decimal credits string, such as "0E-10",
// like a percentage. A string that is not a number prints as sent.
func planCredits(credits string) string {
	v, err := strconv.ParseFloat(credits, 64)
	if err != nil {
		return credits
	}
	return planFigure(v)
}

// captureCoverageLine describes live token capture for a session, "" when
// no live capture saw it.
func captureCoverageLine(
	coverage *db.CaptureCoverageView, plan *db.PlanUsageView,
) string {
	if coverage == nil {
		return ""
	}
	line := "tokens captured live" + asOfLocal(coverage.LastCaptureAt)
	if coverage.UncapturedTurns == 0 {
		return line
	}
	turns := "turns"
	if coverage.UncapturedTurns == 1 {
		turns = "turn"
	}
	line += fmt.Sprintf("; %d %s not captured", coverage.UncapturedTurns, turns)
	if coverage.UncapturedTurns == 1 {
		line += ", so some of its tokens may be missing from the totals"
	} else {
		line += ", so some of their tokens may be missing from the totals"
	}
	// A complete report with no later failure counts every turn that ran
	// before its data time. Whether a missed turn ran before it is not
	// known here, so the line names the time instead of claiming it.
	if plan == nil || plan.DataStatus != "available" || plan.OutcomeKind != "" || !hasPlanFigures(plan) {
		return line
	}
	asOf := localTime(plan.DataAsOf)
	if asOf == "" {
		return line
	}
	if coverage.UncapturedTurns == 1 {
		return line + "; plan usage counts it if it ran before " + asOf
	}
	return line + "; plan usage counts the ones that ran before " + asOf
}

// asOfLocal renders an RFC 3339 time as ", as of <local time>", or "" when
// the time is absent or unreadable.
func asOfLocal(ts string) string {
	if t := localTime(ts); t != "" {
		return ", as of " + t
	}
	return ""
}

// localTime renders an RFC 3339 time in the local zone, or "" when the
// time is absent or unreadable.
func localTime(ts string) string {
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return ""
	}
	return t.Local().Format("2006-01-02 15:04 MST")
}
