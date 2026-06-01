// ABOUTME: CLI subcommand that returns token usage data for a
// ABOUTME: session, syncing on-demand if no server is running.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/pricing"
	"go.kenn.io/agentsview/internal/server"
	"go.kenn.io/agentsview/internal/sync"
)

// Exit codes for the token-use subcommand.
const (
	tokenUseExitOK            = 0
	tokenUseExitErr           = 1
	tokenUseExitNotFound      = 2
	tokenUseExitNoTokenData   = 3
	tokenUseResolveMatchLimit = 2
)

// resolveRawSessionID translates a user-supplied session ID into
// the canonical form stored in sessions.id. Callers may pass
// either a canonical ID ("codex:<uuid>") or a bare raw ID as
// emitted by the underlying agent — including raw IDs that
// themselves contain colons (Kimi: "<project-hash>:<session-uuid>",
// OpenClaw: "<agentId>:<sessionId>", legacy Kiro IDE).
//
// Resolution order (short-circuit only on host-prefixed IDs, which
// are unambiguously remote; any other input — even one that begins
// with a registered prefix — flows through DB and disk probes
// because the first colon-delimited component can legitimately be
// part of a raw ID):
//
//  1. Host-prefixed input -> returned unchanged.
//  2. DB lookup: exact row (if any) sorts ahead of suffix matches
//     in SQL; suffix matches come back in most-recent order. If
//     multiple suffix matches exist without an exact row, the
//     most recent wins and an ambiguity warning is emitted.
//  3. Canonical disk probe: when input begins with a registered
//     agent prefix, strip the prefix and call that agent's
//     FindSourceFunc so a truly canonical-but-unsynced ID on disk
//     still resolves.
//  4. Raw disk probe: call every file-based agent's FindSourceFunc
//     with the raw input; the first hit yields "<prefix><input>".
//  5. No match anywhere: returned unchanged with known=false.
//
// known reports whether resolution found evidence for the ID.
// When false, the caller should skip on-demand sync because it
// cannot produce meaningful output.
func resolveRawSessionID(
	ctx context.Context,
	database *db.DB,
	agentDirs map[parser.AgentType][]string,
	input string,
) (resolved string, known bool) {
	if host, _ := parser.StripHostPrefix(input); host != "" {
		return input, true
	}

	matches, err := database.FindSessionIDsByRawSuffix(
		ctx, input, tokenUseResolveMatchLimit,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"warning: session id lookup failed: %v\n", err)
	}
	if len(matches) > 0 {
		if matches[0] == input {
			return input, true
		}
		if len(matches) > 1 {
			fmt.Fprintf(os.Stderr,
				"warning: ambiguous session id %q matches "+
					"multiple sessions, using most recent (%s)\n",
				input, matches[0],
			)
		}
		return matches[0], true
	}

	// Canonical disk probe: if the input starts with a known
	// agent prefix, trust that interpretation first and strip
	// before calling FindSourceFunc (which rejects IDs with
	// colons via IsValidSessionID).
	for _, def := range parser.Registry {
		if def.IDPrefix == "" || !def.FileBased ||
			def.FindSourceFunc == nil {
			continue
		}
		if !strings.HasPrefix(input, def.IDPrefix) {
			continue
		}
		bareID := strings.TrimPrefix(input, def.IDPrefix)
		for _, dir := range agentDirs[def.Type] {
			if def.FindSourceFunc(dir, bareID) != "" {
				return input, true
			}
		}
	}

	// Raw disk probe: treat input as a raw agent ID. Agents
	// whose raw IDs cannot contain ':' (most of them) reject
	// the input via IsValidSessionID; agents that accept
	// colon-bearing raw IDs (Kimi, OpenClaw, Kiro IDE) may
	// match.
	for _, def := range parser.Registry {
		if !def.FileBased || def.FindSourceFunc == nil {
			continue
		}
		for _, dir := range agentDirs[def.Type] {
			if def.FindSourceFunc(dir, input) != "" {
				return def.IDPrefix + input, true
			}
		}
	}

	return input, false
}

// usageExitCode classifies a SessionUsage into an exit code: 2 when
// the session is not in the DB, 0 when token data OR cost is present,
// 3 when the session exists but has neither. Cost-only sessions
// (e.g. Hermes) return 0 so callers do not discard useful cost.
func usageExitCode(u *db.SessionUsage) int {
	if u == nil {
		return tokenUseExitNotFound
	}
	if u.HasTokenData || u.HasCost {
		return tokenUseExitOK
	}
	return tokenUseExitNoTokenData
}

// sessionUsageOutput is the JSON shape emitted by `session usage`
// and the deprecated `token-use`. It is a strict superset of the
// historical token-use output (same fields, plus cost). The shape
// is experimental and may change.
type sessionUsageOutput struct {
	db.SessionUsage
	ServerRunning bool `json:"server_running"`
}

// startupWaitTimeout is how long CLI subcommands wait for a
// starting server to become ready before falling back to
// on-demand sync or direct DB access.
const startupWaitTimeout = 30 * time.Second

func runTokenUse(args []string) {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr,
			"usage: agentsview token-use <session-id>")
		os.Exit(tokenUseExitErr)
	}
	fmt.Fprintln(os.Stderr,
		"note: 'token-use' is deprecated; use 'session usage <id>' instead")

	out, code, err := sessionUsageData(args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(tokenUseExitErr)
	}
	if out != nil {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if encErr := enc.Encode(out); encErr != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", encErr)
			os.Exit(tokenUseExitErr)
		}
	}
	os.Exit(code)
}

func sessionUsageData(sessionID string) (*sessionUsageOutput, int, error) {
	appCfg, err := config.LoadMinimal()
	if err != nil {
		return nil, tokenUseExitErr, fmt.Errorf("loading config: %w", err)
	}

	if err := os.MkdirAll(appCfg.DataDir, 0o755); err != nil {
		return nil, tokenUseExitErr,
			fmt.Errorf("creating data dir: %w", err)
	}

	serverActive := server.IsLocalServerActive(appCfg.DataDir)

	// If a server is actively starting up (startup lock
	// present), wait for it to finish so we read fresh data
	// rather than returning stale results or "not found".
	// We only wait when the startup lock is the reason
	// IsLocalServerActive returned true — if a state file has a
	// live PID but the TCP probe is transiently failing,
	// the server is running and we should just read the DB.
	if serverActive &&
		server.FindRunningServer(appCfg.DataDir) == nil {
		if server.IsStartupLocked(appCfg.DataDir) {
			fmt.Fprintf(os.Stderr,
				"server is starting up, waiting...\n")
			if !server.WaitForStartup(
				appCfg.DataDir, startupWaitTimeout,
			) {
				if server.IsStartupLocked(appCfg.DataDir) {
					// Lock still live after timeout:
					// the server is active (still
					// syncing, or state file write
					// failed). Don't compete — read
					// the DB as-is.
					fmt.Fprintf(os.Stderr,
						"server still starting after "+
							"%s, reading DB as-is\n",
						startupWaitTimeout,
					)
				} else {
					// Lock cleared but no running
					// server. Re-check in case of
					// transient TCP failure.
					serverActive = server.IsLocalServerActive(
						appCfg.DataDir,
					)
				}
			}
		} else if !server.IsLocalServerActive(appCfg.DataDir) {
			// The server that was alive at the first check
			// has since exited. Fall back to on-demand sync.
			serverActive = false
		}
	}

	applyClassifierConfig(appCfg)
	database, err := db.Open(appCfg.DBPath)
	if err != nil {
		return nil, tokenUseExitErr,
			fmt.Errorf("opening database: %w", err)
	}
	defer database.Close()

	if appCfg.CursorSecret != "" {
		secret, decErr := base64.StdEncoding.DecodeString(
			appCfg.CursorSecret,
		)
		if decErr != nil {
			return nil, tokenUseExitErr, fmt.Errorf(
				"invalid cursor secret: %w", decErr,
			)
		}
		database.SetCursorSecret(secret)
	}

	// Pricing setup for the direct path: db.Open (unlike openDB)
	// neither applies custom rates nor seeds model_pricing. Custom
	// rates are in-memory only (safe always). Fallback seeding is a
	// DB write, so do it only when no writable local daemon owns the
	// DB (same condition as the on-demand sync below); a running
	// server already seeds pricing at startup.
	applyCustomPricing(database, appCfg)
	if !serverActive {
		if perr := insertMissingPricing(
			database, pricing.FallbackPricing(),
		); perr != nil {
			fmt.Fprintf(os.Stderr,
				"warning: pricing seed failed: %v\n", perr)
		}
	}

	ctx := context.Background()
	resolvedID, known := resolveRawSessionID(
		ctx, database, appCfg.AgentDirs, sessionID,
	)

	// If no server is managing the DB, do an on-demand sync
	// for this session so the data is fresh. Re-check right
	// before syncing to close the TOCTOU window where a
	// server could have started since our initial probe.
	// If the re-check detects a starting server, wait for
	// it rather than reading potentially stale data.
	if !serverActive {
		serverActive = server.IsLocalServerActive(appCfg.DataDir)
		if serverActive &&
			server.FindRunningServer(appCfg.DataDir) == nil &&
			server.IsStartupLocked(appCfg.DataDir) {
			fmt.Fprintf(os.Stderr,
				"server is starting up, waiting...\n")
			if server.WaitForStartup(
				appCfg.DataDir, startupWaitTimeout,
			) {
				// Server is ready; read DB below.
			} else if !server.IsStartupLocked(
				appCfg.DataDir,
			) {
				// Lock cleared, no running server
				// via TCP. Re-check: a live state
				// file (transient probe failure)
				// still means the server is active.
				serverActive = server.IsLocalServerActive(
					appCfg.DataDir,
				)
			}
			// Lock still live after timeout: server is
			// active but slow. Read DB as-is.
		}
	}
	// Skip sync entirely when we have no evidence of the
	// session (known=false) — SyncSingleSession would just
	// log a misleading "source file not found" warning.
	if !serverActive && known {
		engine := sync.NewEngine(database, sync.EngineConfig{
			AgentDirs:               appCfg.AgentDirs,
			Machine:                 "local",
			BlockedResultCategories: appCfg.ResultContentBlockedCategories,
		})
		if syncErr := engine.SyncSingleSessionContext(
			ctx, resolvedID,
		); syncErr != nil {
			// Not fatal: session may already be in the DB
			// from a previous sync, or may not exist at all.
			fmt.Fprintf(os.Stderr,
				"warning: sync failed: %v\n", syncErr)
		}
	}

	u, err := database.GetSessionUsage(ctx, resolvedID)
	if err != nil {
		return nil, tokenUseExitErr,
			fmt.Errorf("querying session usage: %w", err)
	}
	if u == nil {
		fmt.Fprintf(os.Stderr, "session not found: %s\n", sessionID)
		return nil, tokenUseExitNotFound, nil
	}
	// If the session uses models the local pricing catalog
	// doesn't know about, try a one-off LiteLLM refresh and
	// re-query — newly released models often hit this until
	// the user next runs `agentsview serve`. Skip when a
	// server is active (it owns pricing writes) or the
	// cooldown hasn't elapsed.
	if len(u.UnpricedModels) > 0 && !serverActive {
		refreshed, refErr := refreshPricingIfStale(
			database, pricing.FetchLiteLLMPricing,
			pricingRefreshCooldown, time.Now(),
		)
		if refErr != nil {
			fmt.Fprintf(os.Stderr,
				"warning: pricing refresh failed: %v\n", refErr)
		} else if refreshed {
			if u2, e := database.GetSessionUsage(
				ctx, resolvedID,
			); e == nil && u2 != nil {
				u = u2
			}
		}
	}
	if u.Agent == "" {
		if def, ok := parser.AgentByPrefix(u.SessionID); ok {
			u.Agent = string(def.Type)
		}
	}
	return &sessionUsageOutput{
		SessionUsage:  *u,
		ServerRunning: serverActive,
	}, usageExitCode(u), nil
}
