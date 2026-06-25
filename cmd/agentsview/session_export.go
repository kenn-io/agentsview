// ABOUTME: `session export <id>` subcommand — streams the raw source
// ABOUTME: JSONL file for a locally-synced session. Local-only by
// ABOUTME: design; bypasses the SessionService layer.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/parser"
)

func newSessionExportCommand() *cobra.Command {
	return &cobra.Command{
		Use:          "export <id>",
		Short:        "Stream the raw source JSONL for a session (local only)",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmd.Flags().Changed("server") {
				return fmt.Errorf(
					"session export: local-only command; --server not supported",
				)
			}
			if cmd.Flags().Changed("format") {
				return fmt.Errorf(
					"session export: streams raw bytes; --format not supported",
				)
			}
			if pgReadRequested(cmd) {
				return fmt.Errorf(
					"session export: local-only command; --pg not supported",
				)
			}
			cfg, err := config.LoadPFlags(cmd.Flags())
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			d, err := openReadOnlyDB(cfg)
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
			storedPath := d.GetSessionFilePath(id)
			if storedPath == "" {
				return fmt.Errorf(
					"source file not found for session %s", id,
				)
			}
			// Aider stores many repo runs in one Markdown history file,
			// with sessions keyed by a <history>#<idx> virtual path.
			// Export only the selected run, not sibling runs from the
			// same repository.
			if historyPath, idx, ok :=
				parser.ParseAiderVirtualPath(storedPath); ok {
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
					cmd.OutOrStdout(), historyPath, idx,
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
			if tracePath, conversationID, ok :=
				parser.SplitVisualStudioCopilotVirtualPath(storedPath); ok {
				err := parser.WriteVisualStudioCopilotConversationJSONL(
					cmd.OutOrStdout(), tracePath, conversationID,
				)
				if errors.Is(err, os.ErrNotExist) {
					return fmt.Errorf(
						"source file not found: %s", tracePath,
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
			_, err = io.Copy(cmd.OutOrStdout(), f)
			return err
		},
	}
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
