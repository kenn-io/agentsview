package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
)

func newExportConversationsCommand() *cobra.Command {
	command := &cobra.Command{
		Use: "conversations", Short: "Export stored conversation text and changes",
		Args: cobra.NoArgs, SilenceUsage: true,
	}
	command.AddCommand(newConversationChangesCommand(), newConversationMessageCommand())
	return command
}

func newConversationChangesCommand() *cobra.Command {
	var options db.ConversationExportOptions
	command := &cobra.Command{
		Use: "changes", Short: "List changed messages without exporting their text",
		Args: cobra.NoArgs, SilenceUsage: true,
		RunE: func(command *cobra.Command, _ []string) error {
			if options.Checkpoint != "" && options.Cursor != "" {
				return errors.New("--checkpoint and --cursor are mutually exclusive")
			}
			if options.Limit < 1 || options.Limit > db.MaxSessionLimit {
				return fmt.Errorf("--limit must be between 1 and %d", db.MaxSessionLimit)
			}
			database, err := openConversationExportDB(command)
			if err != nil {
				return err
			}
			defer database.Close()
			result, err := database.ExportConversationChanges(command.Context(), options)
			if err != nil {
				return writeConversationExportError(command, err)
			}
			return json.MarshalEncode(jsontext.NewEncoder(command.OutOrStdout()), result)
		},
	}
	command.Flags().StringVar(&options.Checkpoint, "checkpoint", "", "Resume after a completed changes walk")
	command.Flags().StringVar(&options.Cursor, "cursor", "", "Continue an unfinished changes walk")
	command.Flags().IntVar(&options.Limit, "limit", db.MaxSessionLimit, "Maximum changes in this page")
	return command
}

func newConversationMessageCommand() *cobra.Command {
	var options db.ConversationMessageOptions
	command := &cobra.Command{
		Use: "message SESSION_ID MESSAGE_ID", Short: "Read a bounded chunk of one message revision",
		Args: cobra.ExactArgs(2), SilenceUsage: true,
		RunE: func(command *cobra.Command, args []string) error {
			if options.Revision == "" || options.DatabaseID == "" {
				return errors.New("--revision and --database-id are required; use the values from conversations changes")
			}
			if options.Offset < 0 || options.MaxBytes < 4 {
				return errors.New("--offset must be non-negative and --max-bytes must be at least 4")
			}
			options.SessionID, options.MessageID = args[0], args[1]
			database, err := openConversationExportDB(command)
			if err != nil {
				return err
			}
			defer database.Close()
			result, err := database.GetConversationMessage(command.Context(), options)
			if err != nil {
				return writeConversationExportError(command, err)
			}
			return json.MarshalEncode(jsontext.NewEncoder(command.OutOrStdout()), result)
		},
	}
	command.Flags().StringVar(&options.Revision, "revision", "", "Expected message revision from the changes listing")
	command.Flags().StringVar(&options.DatabaseID, "database-id", "", "Expected archive generation from the changes listing")
	command.Flags().Int64Var(&options.Offset, "offset", 0, "Starting UTF-8 byte offset from the previous chunk")
	command.Flags().IntVar(&options.MaxBytes, "max-bytes", 64*1024, "Maximum message text bytes in this chunk")
	return command
}

func openConversationExportDB(command *cobra.Command) (*db.DB, error) {
	appConfig, err := config.LoadPFlags(command.Flags())
	if err != nil {
		return nil, fmt.Errorf("loading config: %w", err)
	}
	// Exports do not migrate or reparse the archive. The normal writable
	// database initialization establishes the stored export index.
	database, err := openReadOnlyDB(command.Context(), appConfig)
	if err != nil {
		return nil, fmt.Errorf("open local archive: %w", err)
	}
	return database, nil
}

func writeConversationExportError(command *cobra.Command, cause error) error {
	var code int
	result := struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}{}
	switch {
	case errors.Is(cause, db.ErrConversationInitializationRequired):
		return appendDaemonRestartUpgradeHint(cause)
	case errors.Is(cause, db.ErrConversationReconciliationRequired):
		code, result.Error = sessionExportCursorResetExitCode, "reconciliation_required"
		result.Message = "conversation checkpoint belongs to another archive generation; reconcile from a new changes walk"
	case errors.Is(cause, db.ErrConversationRevisionChanged):
		code, result.Error = 5, "revision_changed"
		result.Message = "this message revision was superseded; read the next changes cycle for its current state"
	default:
		return schemaUpgradeHint(cause)
	}
	if err := json.MarshalEncode(jsontext.NewEncoder(command.ErrOrStderr()), result); err != nil {
		return err
	}
	return withSilentExitCode(cause, code)
}
