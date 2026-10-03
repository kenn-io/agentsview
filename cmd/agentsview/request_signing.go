package main

import (
	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/requestsign"
)

func newSigningCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "signing", Short: "Prepare private native HTTP signing keys and replay state"}
	cmd.AddCommand(&cobra.Command{Use: "keygen <key-file>", Short: "Create a private random signing key without printing it", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, args []string) error { return requestsign.GenerateSecretFile(args[0]) }})
	cmd.AddCommand(&cobra.Command{Use: "init-replay <state-file>", Short: "Explicitly create new durable signing replay state", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, args []string) error { return requestsign.InitReplay(args[0]) }})
	return cmd
}
