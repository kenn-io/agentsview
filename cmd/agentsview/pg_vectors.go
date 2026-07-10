// ABOUTME: `pg vectors` command group — list and drop PostgreSQL semantic
// ABOUTME: search vector generations for maintenance and cleanup.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/postgres"
)

func newPGVectorsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "vectors",
		Short:        "Inspect and drop PostgreSQL vector generations",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newPGVectorsListCommand())
	cmd.AddCommand(newPGVectorsDropCommand())
	return cmd
}

func newPGVectorsListCommand() *cobra.Command {
	var targetName string
	cmd := &cobra.Command{
		Use:          "list",
		Short:        "List PostgreSQL vector generations",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := runPGVectorsList(cmd.OutOrStdout(), targetName); err != nil {
				return fmt.Errorf("pg vectors list: %w", err)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&targetName, "target", "",
		"PG target name (default: the default configured target)")
	return cmd
}

func newPGVectorsDropCommand() *cobra.Command {
	var targetName string
	var yes bool
	cmd := &cobra.Command{
		Use:          "drop <id>",
		Short:        "Drop a PostgreSQL vector generation and its embeddings",
		SilenceUsage: true,
		Args:         cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseGenerationID(args[0])
			if err != nil {
				return err
			}
			if err := runPGVectorsDrop(
				cmd.InOrStdin(), cmd.OutOrStdout(), targetName, id, yes,
			); err != nil {
				return fmt.Errorf("pg vectors drop: %w", err)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&targetName, "target", "",
		"PG target name (default: the default configured target)")
	cmd.Flags().BoolVar(&yes, "yes", false, "Skip confirmation prompt")
	return cmd
}

// openPGVectorTarget resolves a single PG target the same way `pg status` does
// (resolvePGTargetSelections + postgres.Open, no --all), opening a connection
// to it. The returned cleanup closes the pool.
func openPGVectorTarget(targetName string) (*sql.DB, func(), error) {
	appCfg, err := config.LoadMinimal()
	if err != nil {
		return nil, nil, fmt.Errorf("loading config: %w", err)
	}
	if err := os.MkdirAll(appCfg.DataDir, 0o755); err != nil {
		return nil, nil, fmt.Errorf("creating data dir: %w", err)
	}
	setupLogFile(appCfg.DataDir)

	targets, err := resolvePGTargetSelections(appCfg, targetName, false)
	if err != nil {
		return nil, nil, err
	}
	target, err := resolvePGTargetConfig(appCfg, targets[0])
	if err != nil {
		return nil, nil, err
	}
	if target.PG.URL == "" {
		return nil, nil, fmt.Errorf("url not configured")
	}
	applyClassifierConfig(appCfg)
	pg, err := postgres.Open(
		target.PG.URL, target.PG.Schema, target.PG.AllowInsecure,
	)
	if err != nil {
		return nil, nil, err
	}
	return pg, func() { _ = pg.Close() }, nil
}

func runPGVectorsList(out io.Writer, targetName string) error {
	pg, cleanup, err := openPGVectorTarget(targetName)
	if err != nil {
		return err
	}
	defer cleanup()

	gens, err := postgres.ListVectorGenerations(context.Background(), pg)
	if err != nil {
		return err
	}
	printPGVectorGenerations(out, gens)
	return nil
}

// printPGVectorGenerations renders generations as a tabwriter table, matching
// the column layout of the other CLI list commands. An empty set prints just
// the header row.
func printPGVectorGenerations(out io.Writer, gens []postgres.VectorGenerationRow) {
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tMODEL\tDIM\tDOCS\tCHUNKS\tMACHINES\tCREATED")
	for _, g := range gens {
		machines := strings.Join(g.Machines, ",")
		if machines == "" {
			machines = "-"
		}
		fmt.Fprintf(tw, "%d\t%s\t%d\t%d\t%d\t%s\t%s\n",
			g.ID, g.Model, g.Dimension, g.Docs, g.Chunks,
			machines, g.CreatedAt.UTC().Format(time.RFC3339))
	}
	_ = tw.Flush()
}

func runPGVectorsDrop(
	in io.Reader, out io.Writer, targetName string, id int64, yes bool,
) error {
	pg, cleanup, err := openPGVectorTarget(targetName)
	if err != nil {
		return err
	}
	defer cleanup()

	if !yes {
		msg := fmt.Sprintf(
			"Drop vector generation %d and all of its embeddings?", id)
		if !confirm(in, out, msg) {
			fmt.Fprintln(out, "Aborted.")
			return nil
		}
	}
	if err := postgres.DropVectorGeneration(context.Background(), pg, id); err != nil {
		return err
	}
	fmt.Fprintf(out, "Dropped vector generation %d.\n", id)
	return nil
}
