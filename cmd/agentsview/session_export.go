// ABOUTME: `session export <id>` subcommand — streams the raw source
// ABOUTME: JSONL file for a locally-synced session. Local-only by
// ABOUTME: design; bypasses the SessionService layer.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/secrets"
)

func newSessionExportCommand() *cobra.Command {
	var reveal bool
	command := &cobra.Command{
		Use:   "export <id>",
		Short: "Export provider-specific source content for a local session",
		Long: "Export provider-specific source content for one locally archived session. " +
			"Aider emits the selected run as Markdown; Windsurf and Trae reconstruct session JSON; supported shared or database sources emit only the selected session as JSONL. " +
			"Other file sources are copied as bytes. " +
			"Detected secrets are masked by default; --reveal writes the source unchanged. " +
			"Local source access is required, and usage-only archives cannot export it. " +
			"--server, --pg, --json, and --format are rejected. " +
			"Use the top-level export group for normalized archive reports.",
		Example:      "  agentsview session export <id> > session-export.out",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmd.Flags().Changed("server") {
				return cliUsageError(errors.New("session export: local-only command; --server not supported"), "omit --server and run this command on the host that owns the local archive")
			}
			if err := rejectFormatFlags(
				cmd, "session export", "raw bytes",
			); err != nil {
				return err
			}
			if pgReadRequested(cmd) {
				return cliUsageError(errors.New("session export: local-only command; --pg not supported"), "omit --pg and run this command on the host that owns the local archive")
			}
			cfg, err := config.LoadPFlagsFor(cmd.Flags(), config.ScopeLocal)
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			if cfg.ArchiveContent.UsageOnly() {
				return errors.New("session export is unavailable with archive_content=usage")
			}
			d, err := openReadOnlyDB(cmd.Context(), cfg)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return fmt.Errorf(
						"session not in local archive: %s", args[0],
					)
				}
				return fmt.Errorf("open local archive: %w", err)
			}
			defer d.Close()

			id, err := resolveSessionID(cmd.Context(), d, args[0])
			if err != nil {
				return err
			}
			if id == "" {
				return fmt.Errorf(
					"session not in local archive: %s", args[0],
				)
			}
			session, err := d.GetSession(cmd.Context(), id)
			if err != nil {
				return err
			}
			if session == nil {
				return fmt.Errorf(
					"session not in local archive: %s", args[0],
				)
			}
			storedPath := d.GetSessionFilePath(cmd.Context(), id)
			if storedPath == "" {
				return fmt.Errorf(
					"source file not found for session %s", id,
				)
			}
			out := cmd.OutOrStdout()
			var unmasked *bytes.Buffer
			if !reveal {
				// Buffer the whole export: a JSONL source is masked in decoded
				// strings line by line, and any other source is masked whole
				// so a key block spanning lines is still caught.
				unmasked = new(bytes.Buffer)
				out = unmasked
			}
			if err := writeSessionSource(cmd.Context(), out, cfg.AgentDirs, session, id, storedPath); err != nil || unmasked == nil {
				return err
			}
			_, err = io.WriteString(cmd.OutOrStdout(), secrets.RedactJSONLines(unmasked.String()))
			return err
		},
	}
	command.Flags().BoolVar(&reveal, "reveal", false, "Show full secret values (unredacted)")
	return command
}

// writeSessionSource writes the provider source content for one session.
func writeSessionSource(
	ctx context.Context, out io.Writer, agentDirs map[parser.AgentType][]string,
	session *db.Session, id, storedPath string,
) error {
	// Aider stores many repo runs in one Markdown history file,
	// with sessions keyed by a <history>#<idx> virtual path.
	// Export only the selected run, not sibling runs from the
	// same repository.
	if historyPath, idx, ok := parser.ParseAiderVirtualPath(storedPath); ok {
		rawID, ok := rawAiderSessionID(id)
		if !ok {
			return fmt.Errorf(
				"stale aider source for session %s: invalid aider session id",
				id,
			)
		}
		if got, ok := parser.AiderRawIDAt(historyPath, idx); !ok || got != rawID {
			if _, statErr := os.Stat(historyPath); statErr != nil {
				if os.IsNotExist(statErr) {
					return fmt.Errorf(
						"source file not found: %s", historyPath,
					)
				}
				return statErr
			}
			resolved, found := parser.AiderVirtualPathForRawID(
				historyPath, rawID,
			)
			if !found {
				return fmt.Errorf(
					"stale aider source for session %s: %s no longer contains the archived run",
					id, historyPath,
				)
			}
			historyPath, idx, _ = parser.ParseAiderVirtualPath(resolved)
		}
		err := parser.WriteAiderRunMarkdown(
			out, historyPath, idx,
		)
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf(
				"source file not found: %s", historyPath,
			)
		}
		return err
	}
	// A Visual Studio Copilot trace file holds spans for several
	// conversations, so streaming the whole file would disclose
	// unrelated conversations. Filter to the requested conversation.
	if tracePath, conversationID, ok := parser.SplitVisualStudioCopilotVirtualPath(storedPath); ok {
		err := parser.WriteVisualStudioCopilotConversationJSONL(
			out, tracePath, conversationID,
		)
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf(
				"source file not found: %s", tracePath,
			)
		}
		return err
	}
	switch session.Agent {
	case string(parser.AgentWindsurf):
		if dbPath, sessionID, ok := parser.SplitWindsurfVirtualPath(storedPath); ok {
			err := parser.WriteWindsurfSessionJSON(
				ctx, out, dbPath, sessionID,
			)
			if errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf(
					"source file not found: %s", dbPath,
				)
			}
			return err
		}
	case string(parser.AgentTrae):
		if dbPath, sessionID, ok := parser.SplitTraeVirtualPath(storedPath); ok {
			err := parser.WriteTraeSessionJSON(ctx,
				out, dbPath, sessionID,
			)
			if errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf(
					"source file not found: %s", dbPath,
				)
			}
			return err
		}
	}
	// Hermes and its Augure Desktop fork store state.db#<id>
	// virtual paths; streaming the selected session's JSONL beats
	// streaming the SQLite file. The fork keeps the raw
	// (unprefixed) ID in SourceSessionID, so the same writer serves
	// both agents.
	agent := parser.AgentType(session.Agent)
	if (agent == parser.AgentHermes ||
		agent == parser.AgentAugureDesktop) &&
		filepath.Base(parser.ResolveSourceFilePath(storedPath)) == "state.db" {
		rawSessionID := session.SourceSessionID
		if rawSessionID == "" {
			rawSessionID, _ = rawHermesSessionID(id, agent)
		}
		// Search only the session's own agent roots. The fork's raw
		// IDs are a separate namespace from Hermes, so falling back
		// to Hermes roots could export an unrelated session that
		// happens to share the raw ID.
		err := parser.WriteHermesSessionJSONL(ctx,
			out,
			storedPath,
			agentDirs[agent],
			rawSessionID,
		)
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf(
				"source file not found for session %s", id,
			)
		}
		return err
	}
	path := parser.ResolveSourceFilePath(storedPath)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf(
				"source file not found: %s", path,
			)
		}
		return err
	}
	defer f.Close()
	_, err = io.Copy(out, f)
	return err
}

func rawAiderSessionID(sessionID string) (string, bool) {
	def, ok := parser.AgentByPrefix(sessionID)
	if !ok || def.Type != parser.AgentAider {
		return "", false
	}
	_, rawID := parser.StripHostPrefix(sessionID)
	rawID = strings.TrimPrefix(rawID, def.IDPrefix)
	return rawID, rawID != ""
}

func rawHermesSessionID(sessionID string, agent parser.AgentType) (string, bool) {
	def, ok := parser.AgentByPrefix(sessionID)
	if !ok || def.Type != agent {
		return "", false
	}
	_, rawID := parser.StripHostPrefix(sessionID)
	rawID = strings.TrimPrefix(rawID, def.IDPrefix)
	return rawID, rawID != ""
}
