package main

import (
	"fmt"
	"net/http"
	"time"

	"github.com/spf13/cobra"
	"github.com/zalando/go-keyring"

	"go.kenn.io/agentsview/internal/cloudsync/claudeai"
)

const (
	claudeKeychainService = "io.agentsview.desktop"
	claudeKeychainAccount = "claude-ai/default"
)

func newCloudCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "cloud",
		Short:        "Manage cloud conversation sources",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
	}
	cmd.AddCommand(newCloudClaudeAICommand())
	return cmd
}

func newCloudClaudeAICommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "claude-ai",
		Short:        "Manage the Claude.ai connection",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
	}
	cmd.AddCommand(newCloudClaudeAITestConnectionCommand(keyring.Get))
	return cmd
}

type keychainGet func(service, account string) (string, error)

func newCloudClaudeAITestConnectionCommand(getSecret keychainGet) *cobra.Command {
	return &cobra.Command{
		Use:          "test-connection",
		Short:        "Verify the saved Claude.ai session",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cookie, err := getSecret(claudeKeychainService, claudeKeychainAccount)
			if err != nil {
				return fmt.Errorf("read saved Claude.ai session: %w", err)
			}
			client, err := claudeai.NewClient(&http.Client{Timeout: 20 * time.Second}, "", claudeai.Credentials{Cookie: cookie})
			if err != nil {
				return err
			}
			page, err := client.FirstConversationPage(cmd.Context())
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Claude.ai connection verified; first page returned %d conversation(s).\n", len(page.Conversations))
			return nil
		},
	}
}
