package main

import "github.com/spf13/cobra"

// newCloudCommand is retained as a stable CLI group. Claude.ai connection and
// synchronization are desktop transport features; the Go CLI never reads
// browser credentials or makes provider HTTP requests.
func newCloudCommand() *cobra.Command {
	return &cobra.Command{
		Use:          "cloud",
		Short:        "Manage cloud conversation sources",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
	}
}
