// ABOUTME: `secrets` command group: scan for and list detected secret leaks
// ABOUTME: across sessions, redacted by default.
package main

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/secrets"
	"go.kenn.io/agentsview/internal/service"
)

func newSecretsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "secrets",
		Short:        "Scan for and list detected secret leaks",
		GroupID:      groupData,
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	registerFormatFlags(cmd.PersistentFlags())
	cmd.AddCommand(newSecretsListCommand())
	cmd.AddCommand(newSecretsScanCommand())
	return cmd
}

func newSecretsScanCommand() *cobra.Command {
	var (
		backfill                         bool
		project, agent, dateFrom, dateTo string
	)
	cmd := &cobra.Command{
		Use:   "scan",
		Short: "Run a full-ruleset scan (definite + candidate rules) and persist findings",
		Long: `Inline sync runs only the fast, well-anchored definite rules. This command
runs the full ruleset, including candidate-tier detectors (generic
high-entropy env values, JWTs, basic-auth URLs).

By default every matching session is rescanned. Use --backfill to skip
sessions already at the current ruleset version — the efficient choice
after a binary upgrade.

Findings from candidate rules are hidden by 'secrets list' unless you pass
--confidence candidate or --confidence all.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Defense in depth: main enables this for normal binaries, but
			// command-level wiring keeps ad-hoc command execution from scanning
			// agentsview's own fixtures as leaks.
			secrets.EnableFixtureDeny()
			svc, cleanup, err := resolveWritableService(cmd)
			if err != nil {
				return err
			}
			defer cleanup()
			jsonOut := outputFormat(cmd) == "json"
			sum, err := svc.ScanSecrets(cmd.Context(), service.SecretScanInput{
				Backfill: backfill, Project: project, Agent: agent,
				DateFrom: dateFrom, DateTo: dateTo,
			}, func(p service.SecretScanProgress) {
				if !jsonOut {
					fmt.Fprintf(cmd.ErrOrStderr(),
						"\rscanned %d / %d", p.Scanned, p.Total)
				}
			})
			if err != nil {
				return err
			}
			if !jsonOut {
				fmt.Fprintln(cmd.ErrOrStderr())
			}
			if jsonOut {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(sum)
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"Scanned %d sessions; %d with definite leaks; "+
					"%d findings (%d definite, %d candidate).\n",
				sum.Scanned, sum.WithSecrets, sum.TotalFindings,
				sum.DefiniteFindings, sum.CandidateFindings)
			if sum.CandidateFindings > 0 {
				fmt.Fprintln(cmd.OutOrStdout(),
					"\nCandidate findings are hidden by default in 'secrets list'. "+
						"To see them:")
				fmt.Fprintln(cmd.OutOrStdout(),
					"  agentsview secrets list --confidence all")
			}
			return nil
		},
	}
	flags := cmd.Flags()
	flags.BoolVar(&backfill, "backfill", false,
		"Skip sessions already scanned at the current ruleset version")
	flags.StringVar(&project, "project", "", "Limit to a project")
	flags.StringVar(&agent, "agent", "", "Limit to an agent")
	flags.StringVar(&dateFrom, "date-from", "", "Sessions on or after YYYY-MM-DD")
	flags.StringVar(&dateTo, "date-to", "", "Sessions on or before YYYY-MM-DD")
	return cmd
}

func newSecretsListCommand() *cobra.Command {
	var (
		project, agent, dateFrom, dateTo string
		rule, confidence                 string
		reveal                           bool
		limit, cursor                    int
	)
	cmd := &cobra.Command{
		Use:          "list",
		Short:        "List detected secret findings (redacted by default)",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			svc, cleanup, err := resolveService(cmd)
			if err != nil {
				return err
			}
			defer cleanup()
			res, err := svc.ListSecrets(cmd.Context(), service.SecretListFilter{
				Project: project, Agent: agent,
				DateFrom: dateFrom, DateTo: dateTo,
				Rule: rule, Confidence: confidence, Reveal: reveal,
				Limit: limit, Cursor: cursor,
			})
			if err != nil {
				return err
			}
			if reveal {
				fmt.Fprintln(cmd.ErrOrStderr(),
					"WARNING: --reveal prints full secret values; "+
						"this terminal/session may itself be recorded.")
			}
			if outputFormat(cmd) == "json" {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(res)
			}
			return printSecretFindingsHuman(cmd.OutOrStdout(), res)
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&project, "project", "", "Filter by project")
	flags.StringVar(&agent, "agent", "", "Filter by agent")
	flags.StringVar(&dateFrom, "date-from", "", "Sessions on or after YYYY-MM-DD")
	flags.StringVar(&dateTo, "date-to", "", "Sessions on or before YYYY-MM-DD")
	flags.StringVar(&rule, "rule", "", "Filter by rule name")
	flags.StringVar(&confidence, "confidence", "",
		"Filter by confidence: definite, candidate, or all "+
			"(default definite; candidates are opt-in)")
	flags.BoolVar(&reveal, "reveal", false, "Show full secret values (unredacted)")
	flags.IntVar(&limit, "limit", 0, "Max findings (default 50, max 500)")
	flags.IntVar(&cursor, "cursor", 0, "Pagination cursor")
	return cmd
}

func printSecretFindingsHuman(w io.Writer, res *service.SecretFindingList) error {
	if len(res.Findings) == 0 {
		fmt.Fprintln(w, "(no findings)")
		return nil
	}
	fmt.Fprintf(w, "%-40s  %-16s  %-22s  %-18s  %s\n",
		"SESSION", "PROJECT", "RULE", "LOCATION", "VALUE")
	for _, f := range res.Findings {
		fmt.Fprintf(w, "%-40s  %-16s  %-22s  %-18s  %s\n",
			sanitizeTerminal(f.SessionID), sanitizeTerminal(f.Project),
			sanitizeTerminal(f.RuleName), sanitizeTerminal(f.LocationKind),
			sanitizeTerminal(f.RedactedMatch))
	}
	if res.NextCursor != 0 {
		fmt.Fprintf(w, "\nMore results: --cursor %d\n", res.NextCursor)
	}
	return nil
}
