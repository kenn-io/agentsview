// ABOUTME: Builds and serves the agentsview MCP server (stdio or
// ABOUTME: StreamableHTTP) over the six read-only retrieval tools.
package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"go.kenn.io/agentsview/internal/service"
)

// Tool names. The same constant is used to register a tool and to refer
// to it in tests.
const (
	ToolSearchSessions     = "search_sessions"
	ToolListSessions       = "list_sessions"
	ToolGetSessionOverview = "get_session_overview"
	ToolGetMessages        = "get_messages"
	ToolSearchContent      = "search_content"
	ToolGetUsageSummary    = "get_usage_summary"
)

// ServeOptions configures the MCP server. Service is required; Version is
// reported in the server's implementation info; Now is injectable so
// tests can control the self-reference exclusion window (defaults to
// time.Now).
type ServeOptions struct {
	Service service.SessionService
	Version string
	Now     func() time.Time
}

// newServer builds an MCP server with all six read-only tools
// registered. Shared by the stdio and StreamableHTTP transports.
func newServer(opts ServeOptions) *mcp.Server {
	version := opts.Version
	if version == "" {
		version = "dev"
	}
	s := mcp.NewServer(&mcp.Implementation{
		Name:    "agentsview",
		Title:   "agentsview session history",
		Version: version,
	}, nil)

	t := &toolset{svc: opts.Service, now: opts.Now}
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true}

	mcp.AddTool(s, &mcp.Tool{
		Name: ToolSearchSessions,
		Description: "Full-text search across all recorded AI agent sessions (Claude Code, Codex, Gemini, " +
			"Antigravity, and others) from every project and machine. Returns ranked snippets with a " +
			"match_ordinal usable with get_messages to read the surrounding conversation. Use this to " +
			"answer questions like 'have I solved this before?' or to find prior work on a topic. " +
			"Every term must appear (AND); wrap the query in double quotes for an exact phrase. " +
			"Sessions active in the last 10 minutes (including the current conversation) are excluded " +
			"unless include_active is set.",
		Annotations: readOnly,
	}, t.searchSessions)

	mcp.AddTool(s, &mcp.Tool{
		Name: ToolListSessions,
		Description: "List recorded agent sessions with filters (project, agent, machine, date range). " +
			"Returns compact metadata rows, newest first. Use search_sessions instead when looking for " +
			"specific content.",
		Annotations: readOnly,
	}, t.listSessions)

	mcp.AddTool(s, &mcp.Tool{
		Name: ToolGetSessionOverview,
		Description: "Cheap summary of one session: metadata, the opening user message, and the last few " +
			"conversation messages. Call this before get_messages to decide whether a session is relevant.",
		Annotations: readOnly,
	}, t.sessionOverview)

	mcp.AddTool(s, &mcp.Tool{
		Name: ToolGetMessages,
		Description: "Read a slice of one session's transcript, paginated by message ordinal. Defaults " +
			"return only user and assistant messages, each truncated to 2000 characters; truncated " +
			"messages are flagged so you can re-fetch with a higher max_chars_per_message.",
		Annotations: readOnly,
	}, t.getMessages)

	mcp.AddTool(s, &mcp.Tool{
		Name: ToolSearchContent,
		Description: "Exact substring or regex search over raw session text, including tool inputs and results. " +
			"Slower but more precise than search_sessions; use it for error messages, identifiers, or " +
			"code fragments. Matches from the last 10 minutes (including the current conversation) are " +
			"excluded unless include_active is set.",
		Annotations: readOnly,
	}, t.searchContent)

	mcp.AddTool(s, &mcp.Tool{
		Name: ToolGetUsageSummary,
		Description: "Aggregate token usage and cost across all agents: totals plus per-day breakdown, " +
			"filterable by project, agent, machine, and date range.",
		Annotations: readOnly,
	}, t.usageSummary)

	return s
}

// ServeStdio runs the MCP server over stdio. It blocks until stdin is
// closed (the client disconnected) or ctx is cancelled, both of which are
// the normal end of life for a stdio server and return nil. Only an
// unexpected transport failure is surfaced as an error.
func ServeStdio(ctx context.Context, opts ServeOptions) error {
	err := newServer(opts).Run(ctx, &mcp.StdioTransport{})
	if isCleanStdioShutdown(err) {
		return nil
	}
	return fmt.Errorf("serve MCP over stdio: %w", err)
}

// isCleanStdioShutdown reports whether a Run error represents the normal
// end of a stdio session (client disconnect or signal) rather than a
// failure. When stdin closes while requests are in flight, the SDK
// surfaces the internal jsonrpc2 "server is closing" wire error, which it
// neither exports nor wraps in an errors.Is-traversable way - so that one
// case is matched on its stable message. A stdio MCP server that loses
// its client has simply finished its job.
func isCleanStdioShutdown(err error) bool {
	if err == nil {
		return true
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, io.EOF) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "server is closing") ||
		strings.Contains(msg, "connection closed")
}

// ServeHTTP runs the MCP server over StreamableHTTP on addr. When ctx is
// cancelled the HTTP server is shut down gracefully so in-flight tool
// calls can finish. addr must already be validated as a safe bind
// address (see the cmd layer's loopback guard).
func ServeHTTP(ctx context.Context, opts ServeOptions, addr string) error {
	srv := newServer(opts)
	handler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return srv }, nil)
	httpServer := &http.Server{Addr: addr, Handler: handler}
	fmt.Fprintf(os.Stderr, "agentsview mcp: serving on %s\n", addr)

	errCh := make(chan error, 1)
	go func() {
		err := httpServer.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		// Graceful shutdown with a short bound; in-flight tool calls
		// usually finish in milliseconds, so 10s is plenty.
		shutdownCtx, cancel := context.WithTimeout(
			context.Background(), 10*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
		return ctx.Err()
	}
}
