package main

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/chatgpt"
	"go.kenn.io/agentsview/internal/config"
)

func newInsightChatGPTCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use: "chatgpt", Short: "Manage local ChatGPT authorization for generated insights",
		Long: "Manage ChatGPT authorization in this machine's configured data directory. Run on the server as its operating user; these commands do not contact the AgentsView daemon.",
		Args: cobra.NoArgs, SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	for _, action := range []string{"login", "status", "logout"} {
		child := &cobra.Command{Use: action, Args: cobra.NoArgs, SilenceUsage: true}
		switch action {
		case "login":
			child.Short = "Connect with a browser-approved device code"
		case "status":
			child.Short = "Show local connection state without contacting OpenAI"
		case "logout":
			child.Short = "Clear local credentials and cancel pending login"
		}
		child.RunE = func(cmd *cobra.Command, _ []string) error {
			for _, name := range []string{"server", "server-token-file"} {
				if flag := cmd.Flag(name); flag != nil && flag.Changed {
					return errors.New("ChatGPT authorization commands are local-only; omit --server and --server-token-file and run on the server")
				}
			}
			cfg, err := config.LoadReadOnlyFor(config.ScopeLocal)
			if err != nil {
				return err
			}
			client := chatgpt.New(cfg.DataDir)
			switch action {
			case "login":
				return runChatGPTLogin(cmd, client)
			case "logout":
				if err := client.Logout(cmd.Context()); err != nil {
					return err
				}
				_, err := fmt.Fprintln(cmd.OutOrStdout(), "Local ChatGPT credentials cleared.")
				return err
			default:
				status, err := client.Status(cmd.Context())
				if err != nil {
					return err
				}
				if outputFormat(cmd) == "json" {
					return json.MarshalEncode(jsontext.NewEncoder(cmd.OutOrStdout()), status)
				}
				state := status.State
				if state == "reconnect_required" {
					state = "Log in again"
				}
				if status.ExpiresAt.IsZero() {
					_, err = fmt.Fprintln(cmd.OutOrStdout(), state)
				} else {
					_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s (access token expires %s)\n", state, status.ExpiresAt.UTC().Format("2006-01-02T15:04:05Z"))
				}
				return err
			}
		}
		cmd.AddCommand(child)
	}
	return cmd
}

type chatGPTLoginClient interface {
	Login(context.Context, func(chatgpt.DeviceCode) error) error
}

func runChatGPTLogin(cmd *cobra.Command, client chatGPTLoginClient) error {
	if err := client.Login(cmd.Context(), func(code chatgpt.DeviceCode) error {
		_, err := fmt.Fprintf(cmd.OutOrStdout(), "Open %s\nEnter code: %s\nWaiting for approval...\n", code.URL, code.Code)
		return err
	}); err != nil {
		return err
	}
	_, err := fmt.Fprintln(cmd.OutOrStdout(), "ChatGPT connected. Enable provider = \"chatgpt\" and set model in [insights] to use it.")
	return err
}
