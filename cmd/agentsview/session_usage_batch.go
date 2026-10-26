// ABOUTME: Batch session usage with one backend selection and ordered results.
package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
)

type sessionUsageBatchResult struct {
	RequestedID string              `json:"requested_id"`
	ExitCode    int                 `json:"exit_code"`
	Usage       *sessionUsageOutput `json:"usage,omitempty"`
	Error       string              `json:"error,omitempty"`
}

type sessionUsageBatchOutput struct {
	Results []sessionUsageBatchResult `json:"results"`
}

func newSessionUsageBatchCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "usage-batch <id>...",
		Short:        "Show token usage and cost estimates for several sessions",
		Args:         cobra.MinimumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			out, err := sessionUsageBatchDataForCommand(cmd, args)
			if err != nil {
				return err
			}
			return renderSessionUsageBatch(cmd.OutOrStdout(), out, outputFormat(cmd))
		},
	}
	cmd.Flags().Bool("no-sync", false,
		"Use archived usage without synchronizing source transcripts")
	cmd.Flags().Bool("own-only", false,
		"Report only each session's own usage, excluding subagents")
	return cmd
}

func sessionUsageBatchDataForCommand(cmd *cobra.Command, ids []string) (*sessionUsageBatchOutput, error) {
	read, cleanup, err := resolveSessionUsageReader(cmd)
	if err != nil {
		return nil, err
	}
	defer closeArchiveQueryBackend(cleanup)
	out := &sessionUsageBatchOutput{Results: make([]sessionUsageBatchResult, 0, len(ids))}
	for _, id := range ids {
		usage, code, err := read(id)
		result := sessionUsageBatchResult{RequestedID: id, ExitCode: code, Usage: usage}
		if err != nil {
			result.Error = err.Error()
		} else if code == tokenUseExitNotFound {
			result.Error = "session not found: " + id
		}
		out.Results = append(out.Results, result)
	}
	return out, nil
}

func renderSessionUsageBatch(w io.Writer, out *sessionUsageBatchOutput, format string) error {
	if format == "json" {
		return json.MarshalEncode(jsontext.NewEncoder(w, jsontext.WithIndent("  ")), out)
	}
	var text strings.Builder
	for i, result := range out.Results {
		if i > 0 {
			fmt.Fprintln(&text)
		}
		fmt.Fprintf(&text, "%s (exit %d)\n", sanitizeTerminal(result.RequestedID), result.ExitCode)
		if result.Error != "" {
			fmt.Fprintln(&text, sanitizeTerminal(result.Error))
		} else if result.Usage != nil {
			_ = renderSessionUsageHuman(&text, result.Usage)
		}
	}
	_, err := io.WriteString(w, text.String())
	return err
}
