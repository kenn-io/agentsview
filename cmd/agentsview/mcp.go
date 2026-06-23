// ABOUTME: `agentsview mcp` subcommand — serves the read-only MCP tools
// ABOUTME: over the SessionService seam (stdio by default, or HTTP).
package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	mcpserver "go.kenn.io/agentsview/internal/mcp"
)

func newMCPCommand() *cobra.Command {
	var httpAddr string
	var httpAllowInsecure bool

	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Run an MCP server exposing read-only session retrieval tools",
		Long: `Start an MCP (Model Context Protocol) server over stdio (default) or
StreamableHTTP, exposing read-only tools for searching and reading
recorded agent sessions: search_sessions, list_sessions,
get_session_overview, get_messages, search_content, and
get_usage_summary.

The server reads through the same path as the CLI: it talks to a running
agentsview daemon over HTTP when one is discoverable (or --server/--pg),
and otherwise opens the local SQLite archive read-only.

Add to your MCP client config (e.g. Claude Desktop):
  {
    "mcpServers": {
      "agentsview": {
        "command": "agentsview",
        "args": ["mcp"]
      }
    }
  }`,
		GroupID:      groupData,
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			svc, cleanup, err := resolveService(cmd)
			if err != nil {
				return err
			}
			defer cleanup()

			// The CLI runs commands with a plain context (no signal
			// handling), so install our own: a long-lived MCP server must
			// shut down cleanly on SIGINT/SIGTERM.
			ctx, stop := signal.NotifyContext(
				cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			opts := mcpserver.ServeOptions{Service: svc, Version: version}

			if httpAddr != "" {
				addr, err := normalizeMCPHTTPAddr(httpAddr, httpAllowInsecure)
				if err != nil {
					return err
				}
				return mcpserver.ServeHTTP(ctx, opts, addr)
			}
			return mcpserver.ServeStdio(ctx, opts)
		},
	}

	cmd.Flags().StringVar(&httpAddr, "http", "",
		"Serve over StreamableHTTP on this address (e.g. 127.0.0.1:8085) "+
			"instead of stdio. Bare port forms (':8085', '8085') bind to "+
			"loopback only; non-loopback hosts require --http-allow-insecure.")
	cmd.Flags().BoolVar(&httpAllowInsecure, "http-allow-insecure", false,
		"Allow --http to bind a non-loopback address. The MCP server has no "+
			"built-in authentication, so any reachable client can read your "+
			"sessions. Only set this on trusted networks (Tailscale, VPN-only) "+
			"or behind an authenticating reverse proxy.")

	// Transport-selection flags, mirroring the `session` command, so the
	// MCP server can target a remote daemon or PostgreSQL read store.
	cmd.Flags().String("server", "", "Remote daemon URL")
	cmd.Flags().String("server-token-file", "",
		"File containing bearer token for explicit --server requests")
	cmd.Flags().Bool("pg", false,
		"Read session data from configured PostgreSQL")

	return cmd
}

// normalizeMCPHTTPAddr canonicalises a --http argument and rejects values
// that would expose the unauthenticated MCP server on a non-loopback
// interface unless the user has explicitly opted in.
//
// Forms accepted:
//   - "8085"            -> "127.0.0.1:8085" (loopback)
//   - ":8085"           -> "127.0.0.1:8085" (loopback; Go's default would be
//     all-interfaces, which is the footgun this guards against)
//   - "127.0.0.1:8085"  -> unchanged (loopback, allowed)
//   - "[::1]:8085"      -> unchanged (loopback, allowed)
//   - "192.168.1.5:8085", "0.0.0.0:8085", "host.local:8085" -> rejected
//     unless allowInsecure is set
func normalizeMCPHTTPAddr(addr string, allowInsecure bool) (string, error) {
	trimmed := strings.TrimSpace(addr)
	if trimmed == "" {
		return "", errors.New("--http requires an address")
	}

	// Bare port: "8085" or ":8085".
	if !strings.Contains(trimmed, ":") {
		if _, convErr := strconv.Atoi(trimmed); convErr == nil {
			return "127.0.0.1:" + trimmed, nil
		}
		return "", fmt.Errorf("--http %q: not a port and not host:port", trimmed)
	}
	if strings.HasPrefix(trimmed, ":") {
		return "127.0.0.1" + trimmed, nil
	}

	host, _, splitErr := net.SplitHostPort(trimmed)
	if splitErr != nil {
		return "", fmt.Errorf("--http %q: %w", trimmed, splitErr)
	}

	// isLoopbackHost (shared with managed_caddy.go) treats an empty host as
	// NOT loopback, which guards the "[]:8085" footgun where an empty host
	// passes net.SplitHostPort yet binds to all interfaces.
	if isLoopbackHost(host) {
		return trimmed, nil
	}
	if !allowInsecure {
		return "", fmt.Errorf(
			"--http %q: refusing to bind a non-loopback address without "+
				"--http-allow-insecure (the MCP server has no built-in "+
				"authentication; only opt in on trusted networks or behind "+
				"an authenticating reverse proxy)", trimmed)
	}
	return trimmed, nil
}
