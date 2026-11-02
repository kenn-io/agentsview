package main

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	gitdb "go.kenn.io/agentsview/internal/db/git"
)

type whyCommit struct {
	SHA     string          `json:"sha"`
	Subject string          `json:"subject,omitempty"`
	Lines   *int            `json:"lines,omitempty"`
	Links   []db.CommitLink `json:"links"`
}

type whyResult struct {
	Commits     []whyCommit `json:"commits"`
	Uncommitted *int        `json:"uncommitted,omitempty"`
}

var whyRangeRE = regexp.MustCompile(`:([0-9]+)(?:-([0-9]+))?$`)

func splitWhyPath(arg string) (path string, start, end int, err error) {
	match := whyRangeRE.FindStringSubmatchIndex(arg)
	if match == nil {
		return arg, 0, 0, nil
	}
	path = arg[:match[0]]
	start, err = strconv.Atoi(arg[match[2]:match[3]])
	if err != nil {
		return "", 0, 0, fmt.Errorf("invalid line range: %w", err)
	}
	end = start
	if match[4] >= 0 {
		end, err = strconv.Atoi(arg[match[4]:match[5]])
	}
	if err != nil || start < 1 || end < start || path == "" {
		return "", 0, 0, fmt.Errorf("invalid line range in %q", arg)
	}
	return path, start, end, nil
}

func newWhyCommand() *cobra.Command {
	var commit string
	cmd := &cobra.Command{
		Use:     "why <path>[:<start>[-<end>]]",
		Short:   "Find recorded sessions and prompts behind file lines or a commit",
		GroupID: groupData,
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if (len(args) == 1) == cmd.Flags().Changed("commit") {
				return errors.New("provide exactly one path or --commit SHA")
			}
			result := whyResult{}
			shas := []string{commit}
			if len(args) == 1 {
				path, start, end, err := splitWhyPath(args[0])
				if err != nil {
					return err
				}
				abs, err := filepath.Abs(path)
				if err != nil {
					return err
				}
				blame, err := gitdb.BlameCommits(cmd.Context(), filepath.Dir(abs), filepath.Base(abs), start, end)
				if err != nil {
					return err
				}
				result.Uncommitted = new(blame.Uncommitted)
				shas = nil
				for _, c := range blame.Commits {
					shas = append(shas, c.SHA)
					result.Commits = append(result.Commits, whyCommit{SHA: c.SHA, Subject: c.Subject, Lines: new(c.Lines)})
				}
			}
			cfg, err := config.LoadPFlagsFor(cmd.Flags(), config.ScopeLocal)
			if err != nil {
				return err
			}
			tr, err := detectClientTransportContext(cmd.Context(), &cfg, 0)
			if err != nil {
				return err
			}
			svc, cleanup, err := newService(cmd.Context(), cfg, tr)
			if err != nil {
				return err
			}
			defer cleanup()
			for offset := 0; offset < len(shas); offset += db.MaxCommitSHAs {
				matches, err := svc.CommitSessions(cmd.Context(), shas[offset:min(offset+db.MaxCommitSHAs, len(shas))])
				if err != nil {
					return err
				}
				for i, match := range matches.Commits {
					if result.Uncommitted == nil {
						result.Commits = append(result.Commits, whyCommit{SHA: match.SHA, Links: match.Links})
					} else {
						result.Commits[offset+i].Links = match.Links
					}
				}
			}
			if outputFormat(cmd) == "json" {
				return json.MarshalWrite(cmd.OutOrStdout(), result)
			}
			w := &errWriter{w: cmd.OutOrStdout()}
			for _, c := range result.Commits {
				header := commit
				if c.Lines != nil {
					header = fmt.Sprintf("%.12s  %s  (%d lines)", c.SHA, c.Subject, *c.Lines)
				}
				fmt.Fprintln(w, sanitizeTerminal(header))
				if len(c.Links) == 0 {
					fmt.Fprintln(w, "  No recorded session.")
				}
				for _, link := range c.Links {
					line := fmt.Sprintf("  %s  %s  %s  %s", link.Source, link.SessionID, link.Repository, link.Subject)
					if link.Ambiguous {
						line += "  (ambiguous: prefix matches several commits)  " + link.SHA
					}
					if link.Missing {
						fmt.Fprintln(w, sanitizeTerminal(line+"  (deleted or in trash)"))
						continue
					}
					line += fmt.Sprintf("  %s  %s  message %d", link.Branch, link.CommittedAt, link.MessageOrdinal)
					fmt.Fprintln(w, sanitizeTerminal(line))
					if link.Turn != nil {
						for text := range strings.SplitSeq(sanitizeTerminal(link.Turn.Text), "\n") {
							fmt.Fprintln(w, "    "+text)
						}
					}
				}
			}
			if result.Uncommitted != nil && *result.Uncommitted > 0 {
				fmt.Fprintf(w, "%d selected lines are not committed yet.\n", *result.Uncommitted)
			}
			return w.err
		},
	}
	cmd.Flags().StringVar(&commit, "commit", "", "Commit SHA or prefix")
	registerFormatFlags(cmd.Flags())
	return cmd
}
