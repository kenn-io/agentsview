package main

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"go.kenn.io/agentsview/internal/config"
)

func registerRemoteReadFlags(flags *pflag.FlagSet) {
	flags.String("server", "", "Explicit server URL")
	flags.String("server-token-file", "", "Explicit server token file")
	flags.Bool("pg", false, "Select configured PostgreSQL")
}

func noticeLocalArchive(cmd *cobra.Command) {
	remote, err := config.LoadRemote()
	if err == nil && remote.URL != "" {
		fmt.Fprintln(cmd.ErrOrStderr(), "note: this command reads the local archive; the configured remote is not used")
	}
}
