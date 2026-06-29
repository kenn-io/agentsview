// ABOUTME: CLI subcommand that returns token usage data for a
// ABOUTME: session, syncing on-demand if no server is running.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
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
//     agent prefix, strip the prefix and ask that agent's disk source
//     lookup so a truly canonical-but-unsynced ID on disk still resolves.
//  4. Raw disk probe: ask every file-based agent's disk source lookup
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
	// before resolving the source (which rejects IDs with
	// colons via IsValidSessionID).
	for _, def := range parser.Registry {
		if def.IDPrefix == "" || !def.FileBased ||
			!agentHasDiskSourceLookup(def) {
			continue
		}
		if !strings.HasPrefix(input, def.IDPrefix) {
			continue
		}
		bareID := strings.TrimPrefix(input, def.IDPrefix)
		for _, dir := range agentDirs[def.Type] {
			if findAgentSourceFile(def, dir, bareID) != "" {
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
		if !def.FileBased || !agentHasDiskSourceLookup(def) {
			continue
		}
		for _, dir := range agentDirs[def.Type] {
			if findAgentSourceFile(def, dir, input) != "" {
				return def.IDPrefix + input, true
			}
		}
	}

	return input, false
}

// agentHasDiskSourceLookup reports whether a session source can be located on
// disk by raw ID for the agent, via its provider-authoritative provider's
// FindSource.
func agentHasDiskSourceLookup(def parser.AgentDef) bool {
	if parser.ProviderMigrationModes()[def.Type] !=
		parser.ProviderMigrationProviderAuthoritative {
		return false
	}
	_, ok := parser.ProviderFactoryByType(def.Type)
	return ok
}

// findAgentSourceFile resolves a raw agent session ID to an on-disk source path
// under dir via the provider's FindSource (RawSessionID lookup). Returns ""
// when no source resolves or the agent has no on-disk lookup.
func findAgentSourceFile(def parser.AgentDef, dir, rawID string) string {
	factory, ok := parser.ProviderFactoryByType(def.Type)
	if !ok {
		return ""
	}
	provider := factory.NewProvider(parser.ProviderConfig{Roots: []string{dir}})
	source, found, err := provider.FindSource(
		context.Background(),
		parser.FindSourceRequest{RawSessionID: rawID},
	)
	if err != nil || !found {
		return ""
	}
	if path, ok := providerSourcePath(source); ok {
		return path
	}
	return ""
}

// providerSourcePath extracts the on-disk path a provider SourceRef points to,
// preferring the display path and falling back to the fingerprint key or key.
func providerSourcePath(source parser.SourceRef) (string, bool) {
	for _, candidate := range []string{
		source.DisplayPath,
		source.FingerprintKey,
		source.Key,
	} {
		if candidate != "" {
			return candidate, true
		}
	}
	return "", false
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

	ctx := context.Background()
	backend, cleanup, err := resolveArchiveQueryBackendWithConfig(
		ctx,
		appCfg,
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
