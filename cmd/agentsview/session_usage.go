// ABOUTME: `session usage <id>` subcommand — prints per-session
// ABOUTME: token statistics and a cost estimate (JSON or human).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/service"
)

var sessionUsageHTTPClient = &http.Client{Timeout: 30 * time.Second}

type rawSessionIDResolver interface {
	FindSessionIDsByRawSuffix(
		ctx context.Context, raw string, limit int,
	) ([]string, error)
}

func newSessionUsageCommand() *cobra.Command {
	return &cobra.Command{
		Use:          "usage <id>",
		Short:        "Show token usage and cost estimate for a session",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		Run: func(cmd *cobra.Command, args []string) {
			runSessionUsage(cmd, args[0], outputFormat(cmd))
		},
	}
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
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			if encErr := enc.Encode(out); encErr != nil {
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

	remote, _ := cmd.Flags().GetString("server")
	if remote != "" {
		if pgReadRequested(cmd) {
			return nil, tokenUseExitErr, fmt.Errorf(
				"--server and --pg are mutually exclusive",
			)
		}
		token, err := explicitServerToken(cmd)
		if err != nil {
			return nil, tokenUseExitErr, err
		}
		return httpSessionUsageData(ctx, remote, token, sessionID)
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
		return pgSessionUsageData(cfg, pgCfg, sessionID)
	}
	backend, cleanup, err := resolveArchiveQueryBackendWithConfig(
		ctx,
		cfg,
		archiveQueryPolicy{
			AutoStart:            true,
			ReadOnlyDaemon:       archiveQueryRejectReadOnlyDaemon,
			DirectReadOnlyAction: "refresh session usage directly",
		},
	)
	if err != nil {
		return nil, tokenUseExitErr, err
	}
	defer closeArchiveQueryBackend(cleanup)
	return backend.SessionUsage(ctx, sessionID)
}

func readOnlySessionUsageDaemonError(url string) error {
	return fmt.Errorf(
		"daemon at %s is read-only; use --pg to query "+
			"PostgreSQL usage, or stop it to refresh local "+
			"session usage",
		url,
	)
}

func httpSessionUsageData(
	ctx context.Context,
	baseURL string,
	token string,
	sessionID string,
) (*sessionUsageOutput, int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	resolvedID, err := resolveServiceSessionID(
		ctx, service.NewHTTPBackend(baseURL, token, false), sessionID,
	)
	if err != nil {
		if strings.HasPrefix(err.Error(), "session not found:") {
			fmt.Fprintf(os.Stderr, "session not found: %s\n", sessionID)
			return nil, tokenUseExitNotFound, nil
		}
		return nil, tokenUseExitErr, err
	}
	endpoint := strings.TrimSuffix(baseURL, "/") +
		"/api/v1/sessions/" + url.PathEscape(resolvedID) + "/usage"
	req, err := http.NewRequestWithContext(
		ctx, http.MethodGet, endpoint, nil,
	)
	if err != nil {
		return nil, tokenUseExitErr, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := sessionUsageHTTPClient.Do(req)
	if err != nil {
		return nil, tokenUseExitErr, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		fmt.Fprintf(os.Stderr, "session not found: %s\n", sessionID)
		return nil, tokenUseExitNotFound, nil
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, tokenUseExitErr, fmt.Errorf(
			"usage: HTTP %d: %s", resp.StatusCode, body,
		)
	}
	var out sessionUsageOutput
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, tokenUseExitErr, err
	}
	out.ServerRunning = true
	return &out, usageExitCode(&out.SessionUsage), nil
}

func pgSessionUsageData(
	cfg config.Config, pgCfg config.PGConfig, sessionID string,
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

	if len(cfg.CustomModelPricing) > 0 {
		if priced, ok := store.(customPricingStore); ok {
			priced.SetCustomPricing(cfg.CustomModelPricing)
		}
	}

	ctx := context.Background()
	resolvedID, err := resolveStoreSessionID(ctx, store, sessionID)
	if err != nil {
		if !strings.HasPrefix(err.Error(), "session not found:") {
			return nil, tokenUseExitErr,
				fmt.Errorf("resolving pg session id: %w", err)
		}
		fmt.Fprintf(os.Stderr, "session not found: %s\n", sessionID)
		return nil, tokenUseExitNotFound, nil
	}

	u, err := store.GetSessionUsage(ctx, resolvedID)
	if err != nil {
		return nil, tokenUseExitErr,
			fmt.Errorf("querying pg session usage: %w", err)
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
	if resolver, ok := store.(rawSessionIDResolver); ok {
		matches, err := resolver.FindSessionIDsByRawSuffix(
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
	}
	return resolveServiceSessionID(
		ctx, service.NewReadOnlyBackend(store), sessionID,
	)
}

// renderSessionUsageHuman writes a compact key/value summary. The
// cost line shows "~$X.XX (models)" when a complete estimate exists,
// otherwise "n/a" (noting any unpriced models). The tilde marks the
// figure as a model-pricing estimate.
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
	if out.HasCost {
		models := strings.Join(out.Models, ", ")
		fmt.Fprintf(w, "%s ~$%.2f (%s)\n", label("Cost"),
			out.CostUSD, sanitizeTerminal(models))
	} else if len(out.UnpricedModels) > 0 {
		fmt.Fprintf(w, "%s n/a (unpriced: %s)\n", label("Cost"),
			sanitizeTerminal(strings.Join(out.UnpricedModels, ", ")))
	} else {
		fmt.Fprintf(w, "%s n/a\n", label("Cost"))
	}
	if out.AICredits > 0 {
		fmt.Fprintf(w, "%s %.0f\n", label("AI Credits"), out.AICredits)
	}
	return nil
}
