// ABOUTME: `session usage <id>` subcommand — prints per-session
// ABOUTME: token statistics and a cost estimate (JSON or human).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/service"
)

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
		// usage uses the direct token-use path (local SQLite +
		// on-demand sync), not the SessionService layer, so it cannot
		// honor --server. Reject it here with the same "--server not
		// yet implemented" error the service-backed session commands
		// return via resolveService, rather than silently querying
		// local data for a daemon-targeted request. PreRunE surfaces
		// the error through Execute (exit 1); Run keeps os.Exit for
		// the 0/2/3 usage codes.
		PreRunE: func(cmd *cobra.Command, args []string) error {
			if remote, _ := cmd.Flags().GetString("server"); remote != "" {
				return errors.New("--server not yet implemented")
			}
			return nil
		},
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
	cfg, err := config.LoadPFlags(cmd.Flags())
	if err != nil {
		return nil, tokenUseExitErr, fmt.Errorf("loading config: %w", err)
	}
	pgCfg, usePG, err := resolvePGReadConfig(cmd, cfg)
	if err != nil {
		return nil, tokenUseExitErr, err
	}
	if !usePG {
		return sessionUsageData(sessionID)
	}
	return pgSessionUsageData(cfg, pgCfg, sessionID)
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
	return nil
}
