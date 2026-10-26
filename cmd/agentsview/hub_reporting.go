package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/vector"
)

func readHubPullStatus(ctx context.Context, cfg config.Config) (*vector.HubPullStatus, error) {
	if cfg.Vector.Producer != "remote" {
		return nil, nil
	}
	path := cfg.Vector.ResolvedDBPath(cfg.DataDir)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, nil
	}
	ix, err := vector.Open(ctx, path, true, cfg.Vector.Embeddings.MaxInputChars)
	if err != nil {
		return nil, err
	}
	defer ix.Close()
	return ix.HubPullStatus(ctx)
}

func printHubPullStatus(out io.Writer, status *vector.HubPullStatus) {
	if status == nil {
		return
	}
	if status.Unmatched < 0 {
		fmt.Fprint(out, "Hub vectors: unmatched document count unavailable")
	} else {
		fmt.Fprintf(out, "Hub vectors: %d unmatched documents", status.Unmatched)
	}
	if status.Reason != "" {
		fmt.Fprintf(out, "; %s", status.Reason)
	}
	fmt.Fprintln(out)
	if status.Unmatched > 0 {
		fmt.Fprintln(out, "Fill unmatched documents with 'agentsview embeddings build --local-producer'.")
	}
}

func newDoctorEmbeddingsCommand() *cobra.Command {
	return &cobra.Command{
		Use: "embeddings", Short: "Inspect local hub-vector coverage", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.LoadReadOnlyFor(config.ScopeLocal)
			if err != nil {
				return err
			}
			status, err := readHubPullStatus(cmd.Context(), cfg)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Embedding producer: %s\n", cfg.Vector.Producer)
			if status == nil && cfg.Vector.Producer == "remote" {
				fmt.Fprintln(cmd.OutOrStdout(), "No hub pull recorded.")
			}
			printHubPullStatus(cmd.OutOrStdout(), status)
			return nil
		},
	}
}
