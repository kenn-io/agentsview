package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"go.kenn.io/agentsview/internal/sshremote"
)

// newRemoteSyncCommand is the remote half of SSH sync. Collectors run it
// over ssh; it is not meant for people.
func newRemoteSyncCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "remote-sync",
		Short:  "Serve one SSH remote-sync operation",
		Hidden: true,
		Args:   cobra.NoArgs,
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "probe",
		Short: "Print this binary's version and remote-sync protocol",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return sshremote.WriteProbe(cmd.OutOrStdout(), version)
		},
	})
	var opName string
	serve := &cobra.Command{
		Use:   "serve",
		Short: "Run one remote-sync operation over stdin and stdout",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			op, err := sshremote.ParseOp(opName)
			if err != nil {
				return err
			}
			// RunServeProcess already reported the failure on stderr.
			if code := sshremote.RunServeProcess(op, os.Stdin, os.Stderr); code != 0 {
				return withSilentExitCode(
					fmt.Errorf("remote-sync serve exited with status %d", code), code)
			}
			return nil
		},
	}
	serve.Flags().StringVar(&opName, "op", "", "operation: targets, manifest, or archive")
	_ = serve.MarkFlagRequired("op")
	cmd.AddCommand(serve)
	return cmd
}
