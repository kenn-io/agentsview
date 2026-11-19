package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"time"
	"uuid"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/service"
)

func newDiagnosticsCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "diagnostics", Short: "Record failures outside agent sessions", Long: "Record failures that happen outside agent sessions so they can appear in the Friction Log.", GroupID: groupData, SilenceUsage: true, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() }}
	cmd.PersistentFlags().String("server", "", "Remote daemon URL")
	cmd.PersistentFlags().String("server-token-file", "", "File containing bearer token")
	cmd.PersistentFlags().Bool("pg", false, "Read from configured PostgreSQL")
	registerFormatFlags(cmd.PersistentFlags())
	var source, message, at, id string
	appendCmd := &cobra.Command{Use: "append", Short: "Append a diagnostic", Args: cobra.NoArgs, SilenceUsage: true, RunE: func(cmd *cobra.Command, _ []string) error {
		generatedID := id == ""
		if generatedID {
			id = uuid.NewV7().String()
		}
		var occurred time.Time
		if generatedID {
			occurred = time.Now().UTC()
		}
		if at != "" {
			var err error
			occurred, err = time.Parse(time.RFC3339Nano, at)
			if err != nil {
				return fmt.Errorf("invalid --at: %w", err)
			}
		}
		e := db.DiagnosticEvent{ID: id, Source: source, Message: message, OccurredAt: occurred, OccurredAtSupplied: at != ""}
		svc, cleanup, err := resolveWritableService(cmd)
		if err != nil {
			return err
		}
		defer cleanup()
		appender, ok := svc.(service.DiagnosticAppender)
		if !ok {
			return errors.New("diagnostic writes are unavailable")
		}
		stored, err := appender.AppendDiagnostic(cmd.Context(), e)
		if err != nil {
			if occurred.IsZero() {
				return fmt.Errorf("append diagnostic %s: %w", id, err)
			}
			return fmt.Errorf("append diagnostic %s at %s: %w", id, occurred.UTC().Format(time.RFC3339Nano), err)
		}
		if outputFormat(cmd) == "json" {
			return json.MarshalEncode(jsontext.NewEncoder(cmd.OutOrStdout()), stored)
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", stored.ID, stored.OccurredAt.UTC().Format(time.RFC3339Nano))
		return err
	}}
	appendCmd.Flags().StringVar(&source, "source", "", "Diagnostic source")
	appendCmd.Flags().StringVar(&message, "message", "", "Diagnostic message")
	appendCmd.Flags().StringVar(&at, "at", "", "Occurrence time (RFC3339)")
	appendCmd.Flags().StringVar(&id, "id", "", "Event UUID for retries")
	cmd.AddCommand(appendCmd)
	return cmd
}
