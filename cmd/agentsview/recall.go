package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	corerecall "go.kenn.io/agentsview/internal/recall"
	"go.kenn.io/agentsview/internal/service"
)

func newRecallCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "recall",
		Short:        "Build and inspect recalled knowledge from past sessions",
		GroupID:      groupData,
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	registerFormatFlags(cmd.PersistentFlags())
	cmd.PersistentFlags().String(
		"server", "",
		"Remote daemon URL for recall API requests",
	)
	cmd.PersistentFlags().String(
		"server-token-file", "",
		"File containing bearer token for explicit --server requests",
	)

	cmd.AddCommand(newRecallListCommand())
	cmd.AddCommand(newRecallGetCommand())
	cmd.AddCommand(newRecallQueryCommand())
	cmd.AddCommand(newRecallStatsCommand())
	cmd.AddCommand(newRecallBriefCommand())
	cmd.AddCommand(newRecallExtractCommand())
	cmd.AddCommand(newRecallImportCommand())
	return cmd
}

func resolveRecallEntryService(
	cmd *cobra.Command,
) (service.SessionService, func(), error) {
	remote, _ := cmd.Flags().GetString("server")
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return resolveService(cmd)
	}
	token, err := explicitServerToken(cmd)
	if err != nil {
		return nil, nil, err
	}
	return service.NewHTTPBackend(remote, token, false), func() {}, nil
}

// resolveWritableRecallEntryService is the write-capable counterpart of
// resolveRecallEntryService for `recall import`. Local imports go through
// resolveWritableService so a read-only daemon (pg serve) is refused up front
// with actionable guidance instead of failing at the import endpoint.
func resolveWritableRecallEntryService(
	cmd *cobra.Command,
) (service.SessionService, func(), error) {
	remote, _ := cmd.Flags().GetString("server")
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return resolveWritableService(cmd)
	}
	token, err := explicitServerToken(cmd)
	if err != nil {
		return nil, nil, err
	}
	return service.NewHTTPBackend(remote, token, false), func() {}, nil
}

func newRecallListCommand() *cobra.Command {
	var f service.RecallFilter
	var currentCWD bool
	var currentGitBranch bool
	var currentWorktree bool
	cmd := &cobra.Command{
		Use:          "list",
		Short:        "List accepted entries",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, cleanup, err := resolveRecallEntryService(cmd)
			if err != nil {
				return err
			}
			defer cleanup()
			if err := applyRecallEntryCurrentScope(
				&f.CWD, &f.GitBranch, currentCWD, currentGitBranch,
				currentWorktree,
			); err != nil {
				return err
			}
			list, err := svc.ListRecallEntries(cmd.Context(), f)
			if err != nil {
				return err
			}
			if outputFormat(cmd) == "json" {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(list)
			}
			out := cmd.OutOrStdout()
			printRecallEntryTrustedOnlyHuman(out, list.TrustedOnly)
			return printRecallResultsHuman(out, list.RecallEntries)
		},
	}
	addRecallFilterFlags(cmd, &f)
	addRecallEntryCurrentCWDFlag(cmd, &currentCWD)
	addRecallEntryCurrentGitBranchFlag(cmd, &currentGitBranch)
	addRecallEntryCurrentWorktreeFlag(cmd, &currentWorktree)
	return cmd
}

type recallStatsResult struct {
	Count           int            `json:"count"`
	Limit           int            `json:"limit"`
	Truncated       bool           `json:"truncated"`
	TrustedOnly     bool           `json:"trusted_only"`
	ByType          map[string]int `json:"by_type"`
	ByScope         map[string]int `json:"by_scope"`
	ByStatus        map[string]int `json:"by_status"`
	ByProject       map[string]int `json:"by_project"`
	ByAgent         map[string]int `json:"by_agent"`
	ByExtractor     map[string]int `json:"by_extractor"`
	BySourceRun     map[string]int `json:"by_source_run"`
	BySourceSession map[string]int `json:"by_source_session"`
	BySourceEpisode map[string]int `json:"by_source_episode"`
	ByTransferable  map[string]int `json:"by_transferability"`
	ByProvenance    map[string]int `json:"by_provenance_audit"`
	ByEvidence      map[string]int `json:"by_evidence"`
	ByLifecycle     map[string]int `json:"by_lifecycle"`
}

func newRecallStatsCommand() *cobra.Command {
	var f service.RecallFilter
	var currentCWD bool
	var currentGitBranch bool
	var currentWorktree bool
	cmd := &cobra.Command{
		Use:          "stats",
		Short:        "Summarize accepted entries",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, cleanup, err := resolveRecallEntryService(cmd)
			if err != nil {
				return err
			}
			defer cleanup()
			if err := applyRecallEntryCurrentScope(
				&f.CWD, &f.GitBranch, currentCWD, currentGitBranch,
				currentWorktree,
			); err != nil {
				return err
			}
			if f.Limit <= 0 || f.Limit > db.MaxRecallEntryLimit {
				f.Limit = db.MaxRecallEntryLimit
			}
			list, err := svc.ListRecallEntries(cmd.Context(), f)
			if err != nil {
				return err
			}
			stats := buildRecallStats(list.RecallEntries, f.Limit)
			stats.TrustedOnly = list.TrustedOnly
			if outputFormat(cmd) == "json" {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(stats)
			}
			return printRecallStatsHuman(cmd.OutOrStdout(), stats)
		},
	}
	addRecallFilterFlags(cmd, &f)
	addRecallEntryCurrentCWDFlag(cmd, &currentCWD)
	addRecallEntryCurrentGitBranchFlag(cmd, &currentGitBranch)
	addRecallEntryCurrentWorktreeFlag(cmd, &currentWorktree)
	return cmd
}

func buildRecallStats(
	entries []db.RecallResult,
	limit int,
) recallStatsResult {
	stats := recallStatsResult{
		Count:           len(entries),
		Limit:           limit,
		Truncated:       limit > 0 && len(entries) >= limit,
		TrustedOnly:     false,
		ByType:          map[string]int{},
		ByScope:         map[string]int{},
		ByStatus:        map[string]int{},
		ByProject:       map[string]int{},
		ByAgent:         map[string]int{},
		ByExtractor:     map[string]int{},
		BySourceRun:     map[string]int{},
		BySourceSession: map[string]int{},
		BySourceEpisode: map[string]int{},
		ByTransferable:  map[string]int{},
		ByProvenance:    map[string]int{},
		ByEvidence:      map[string]int{},
		ByLifecycle:     map[string]int{},
	}
	for _, recall := range entries {
		countRecallEntryStat(stats.ByType, recall.Type)
		countRecallEntryStat(stats.ByScope, recall.Scope)
		countRecallEntryStat(stats.ByStatus, recall.Status)
		countRecallEntryStat(stats.ByProject, recall.Project)
		countRecallEntryStat(stats.ByAgent, recall.Agent)
		countRecallEntryStat(stats.ByExtractor, recall.ExtractorMethod)
		countRecallEntryStat(stats.BySourceRun, recall.SourceRunID)
		countRecallEntryStat(stats.BySourceSession, recall.SourceSessionID)
		countRecallEntryStat(stats.BySourceEpisode, recall.SourceEpisodeID)
		countRecallEntryStat(
			stats.ByTransferable,
			recallStatsBoolLabel(
				recall.Transferable, "transferable", "not_transferable",
			),
		)
		countRecallEntryStat(
			stats.ByProvenance,
			recallStatsBoolLabel(
				recall.ProvenanceOK,
				"provenance_ok",
				"provenance_unverified",
			),
		)
		countRecallEntryStat(
			stats.ByEvidence,
			recallStatsBoolLabel(
				len(recall.Evidence) > 0,
				"with_evidence",
				"without_evidence",
			),
		)
		countRecallEntryStat(stats.ByLifecycle, recall.LifecycleBucket())
	}
	return stats
}

func recallStatsBoolLabel(ok bool, trueLabel, falseLabel string) string {
	if ok {
		return trueLabel
	}
	return falseLabel
}

func countRecallEntryStat(counts map[string]int, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		value = "(none)"
	}
	counts[value]++
}

func printRecallStatsHuman(w io.Writer, stats recallStatsResult) error {
	fmt.Fprintf(w, "Total: %d\n", stats.Count)
	fmt.Fprintf(w, "Limit: %d\n", stats.Limit)
	fmt.Fprintf(w, "Truncated: %t\n", stats.Truncated)
	fmt.Fprintf(w, "Trusted-only: %t\n", stats.TrustedOnly)
	printRecallStatsSection(w, "By type:", stats.ByType)
	printRecallStatsSection(w, "By scope:", stats.ByScope)
	printRecallStatsSection(w, "By status:", stats.ByStatus)
	printRecallStatsSection(w, "By project:", stats.ByProject)
	printRecallStatsSection(w, "By agent:", stats.ByAgent)
	printRecallStatsSection(w, "By extractor:", stats.ByExtractor)
	printRecallStatsSection(w, "By source run:", stats.BySourceRun)
	printRecallStatsSection(w, "By source session:", stats.BySourceSession)
	printRecallStatsSection(w, "By source episode:", stats.BySourceEpisode)
	printRecallStatsSection(w, "By transferability:", stats.ByTransferable)
	printRecallStatsSection(w, "By provenance audit:", stats.ByProvenance)
	printRecallStatsSection(w, "By evidence:", stats.ByEvidence)
	printRecallStatsSection(w, "By lifecycle:", stats.ByLifecycle)
	return nil
}

func printRecallStatsSection(
	w io.Writer,
	title string,
	counts map[string]int,
) {
	fmt.Fprintln(w, title)
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fmt.Fprintf(w, "  %s  %d\n", sanitizeTerminal(key), counts[key])
	}
}

func newRecallGetCommand() *cobra.Command {
	var showEvidence bool
	cmd := &cobra.Command{
		Use:          "get <id>",
		Short:        "Get one accepted recall entry",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, cleanup, err := resolveRecallEntryService(cmd)
			if err != nil {
				return err
			}
			defer cleanup()
			recall, err := svc.GetRecallEntry(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if recall == nil {
				return fmt.Errorf("recall entry %s not found", args[0])
			}
			if outputFormat(cmd) == "json" {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(recall)
			}
			if err := printRecallEntryHuman(cmd.OutOrStdout(), recall); err != nil {
				return err
			}
			if showEvidence {
				printRecallEvidenceDetailsHuman(cmd.OutOrStdout(), recall.Evidence)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(
		&showEvidence,
		"evidence",
		false,
		"Show evidence provenance snippets in human output",
	)
	return cmd
}

func newRecallQueryCommand() *cobra.Command {
	var req service.RecallQuery
	var showScores bool
	var showEvidence bool
	var showSummary bool
	var currentCWD bool
	var currentGitBranch bool
	var currentWorktree bool
	cmd := &cobra.Command{
		Use:          "query <text>",
		Short:        "Query accepted entries",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, cleanup, err := resolveRecallEntryService(cmd)
			if err != nil {
				return err
			}
			defer cleanup()
			req.Query = args[0]
			req.Surface = db.RecallQuerySurfaceQuery
			if err := applyRecallEntryCurrentScope(
				&req.CWD, &req.GitBranch, currentCWD, currentGitBranch,
				currentWorktree,
			); err != nil {
				return err
			}
			result, err := svc.QueryRecallEntries(cmd.Context(), req)
			if err != nil {
				return err
			}
			if outputFormat(cmd) == "json" {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
			}
			out := cmd.OutOrStdout()
			printRecallEntryTrustedOnlyHuman(out, result.TrustedOnly)
			if req.IncludeContext && (result.Context != "" || result.ContextMeta != nil) {
				if result.Context != "" {
					fmt.Fprintln(out, sanitizeTerminal(result.Context))
				} else {
					fmt.Fprintln(out, "(no recall context fit)")
				}
				printRecallContextMetaHuman(out, result.ContextMeta)
				if showSummary {
					printRecallQuerySummaryHuman(out, result.RecallEntries)
					printRecallQuerySummaryStructHuman(
						out, "Context summary", result.ContextSummary,
					)
				}
				if showScores || showEvidence {
					return printRecallResultsDetailedHumanWithOptions(
						out, result.RecallEntries, recallDetailedPrintOptions{
							ShowScores:   showScores,
							ShowEvidence: showEvidence,
							ContextMeta:  result.ContextMeta,
						})
				}
				return nil
			}
			if showSummary {
				printRecallQuerySummaryHuman(out, result.RecallEntries)
			}
			if showScores || showEvidence {
				return printRecallResultsDetailedHuman(
					out, result.RecallEntries,
					showScores, showEvidence,
				)
			}
			return printRecallResultsHuman(out, result.RecallEntries)
		},
	}
	addRecallQueryFlags(cmd, &req)
	addRecallEntryCurrentCWDFlag(cmd, &currentCWD)
	addRecallEntryCurrentGitBranchFlag(cmd, &currentGitBranch)
	addRecallEntryCurrentWorktreeFlag(cmd, &currentWorktree)
	cmd.Flags().BoolVar(
		&showScores,
		"scores",
		false,
		"Show ranking score diagnostics in human output",
	)
	cmd.Flags().BoolVar(
		&showEvidence,
		"evidence",
		false,
		"Show evidence provenance snippets in human output",
	)
	cmd.Flags().BoolVar(
		&showSummary,
		"summary",
		false,
		"Show aggregate recall summary in human output",
	)
	return cmd
}

func printRecallEntryTrustedOnlyHuman(w io.Writer, trustedOnly bool) {
	fmt.Fprintf(w, "Trusted-only: %t\n", trustedOnly)
}

func printRecallQuerySummaryHuman(
	w io.Writer,
	entries []db.RecallResult,
) {
	summary := service.BuildRecallQuerySummary(entries)
	printRecallQuerySummaryStructHuman(w, "Summary", summary)
}

func printRecallQuerySummaryStructHuman(
	w io.Writer,
	label string,
	summary *service.RecallQuerySummary,
) {
	if summary == nil {
		return
	}
	fmt.Fprintf(
		w,
		"%s: %d %s\n",
		label,
		summary.Count,
		recallCountNoun(summary.Count),
	)
	printRecallStatsSection(w, "By type:", summary.ByType)
	printRecallStatsSection(w, "By scope:", summary.ByScope)
	printRecallStatsSection(w, "By status:", summary.ByStatus)
	printRecallStatsSection(w, "By project:", summary.ByProject)
	printRecallStatsSection(w, "By agent:", summary.ByAgent)
	printRecallStatsSection(w, "By cwd:", summary.ByCWD)
	printRecallStatsSection(w, "By git branch:", summary.ByGitBranch)
	printRecallStatsSection(w, "By match reason:", summary.ByMatchReason)
	printRecallStatsSection(w, "By extractor:", summary.ByExtractorMethod)
	printRecallStatsSection(w, "By model:", summary.ByModel)
	printRecallStatsSection(w, "By source run:", summary.BySourceRun)
	printRecallStatsSection(w, "By source session:", summary.BySourceSession)
	printRecallStatsSection(w, "By source episode:", summary.BySourceEpisode)
	printRecallStatsSection(w, "By transferability:", summary.ByTransferability)
	printRecallStatsSection(w, "By provenance audit:", summary.ByProvenanceAudit)
	printRecallStatsSection(w, "By evidence:", summary.ByEvidence)
	printRecallStatsSection(w, "By lifecycle:", summary.ByLifecycle)
}

func recallCountNoun(count int) string {
	if count == 1 {
		return "entry"
	}
	return "entries"
}

type recallBriefResult struct {
	QueryID        string                      `json:"query_id"`
	MissReason     string                      `json:"miss_reason"`
	Task           string                      `json:"task"`
	TrustedOnly    bool                        `json:"trusted_only"`
	Context        string                      `json:"context"`
	ContextMeta    *service.RecallContextMeta  `json:"context_meta,omitempty"`
	Summary        *service.RecallQuerySummary `json:"summary,omitempty"`
	ContextSummary *service.RecallQuerySummary `json:"context_summary,omitempty"`
	EntryIDs       []string                    `json:"entry_ids"`
	ContextEntries []db.RecallResult           `json:"context_entries,omitempty"`
	RecallEntries  []db.RecallResult           `json:"entries"`
}

func newRecallBriefCommand() *cobra.Command {
	var req service.RecallQuery
	var currentCWD bool
	var currentGitBranch bool
	var currentWorktree bool
	var showScores bool
	var showEvidence bool
	var showSummary bool
	req.TrustedOnly = true
	cmd := &cobra.Command{
		Use:          "brief <task>",
		Short:        "Write a task briefing from accepted entries",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, cleanup, err := resolveRecallEntryService(cmd)
			if err != nil {
				return err
			}
			defer cleanup()
			req.Query = args[0]
			req.Surface = db.RecallQuerySurfaceBrief
			req.IncludeContext = true
			if err := applyRecallEntryCurrentScope(
				&req.CWD, &req.GitBranch, currentCWD, currentGitBranch,
				currentWorktree,
			); err != nil {
				return err
			}
			result, err := svc.QueryRecallEntries(cmd.Context(), req)
			if err != nil {
				return err
			}
			summary := result.Summary
			if summary == nil {
				summary = service.BuildRecallQuerySummary(result.RecallEntries)
			}
			brief := recallBriefResult{
				QueryID:        result.QueryID,
				MissReason:     result.MissReason,
				Task:           req.Query,
				TrustedOnly:    req.TrustedOnly,
				Context:        result.Context,
				ContextMeta:    result.ContextMeta,
				Summary:        summary,
				ContextSummary: result.ContextSummary,
				EntryIDs:       recallBriefIDs(result),
				ContextEntries: result.ContextEntries,
				RecallEntries:  result.RecallEntries,
			}
			if outputFormat(cmd) == "json" {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(brief)
			}
			if err := printRecallBriefHuman(cmd.OutOrStdout(), brief); err != nil {
				return err
			}
			if showSummary {
				printRecallQuerySummaryHuman(cmd.OutOrStdout(), result.RecallEntries)
				printRecallQuerySummaryStructHuman(
					cmd.OutOrStdout(), "Context summary",
					result.ContextSummary,
				)
			}
			if showScores || showEvidence {
				return printRecallResultsDetailedHumanWithOptions(
					cmd.OutOrStdout(), result.RecallEntries,
					recallDetailedPrintOptions{
						ShowScores:   showScores,
						ShowEvidence: showEvidence,
						ContextMeta:  result.ContextMeta,
					})
			}
			return nil
		},
	}
	addRecallQueryFlags(cmd, &req)
	if err := cmd.Flags().MarkHidden("context"); err != nil {
		panic(err)
	}
	if flag := cmd.Flags().Lookup("context-max-bytes"); flag != nil {
		flag.Usage = "Maximum bytes of assembled context"
	}
	addRecallEntryCurrentCWDFlag(cmd, &currentCWD)
	addRecallEntryCurrentGitBranchFlag(cmd, &currentGitBranch)
	addRecallEntryCurrentWorktreeFlag(cmd, &currentWorktree)
	cmd.Flags().BoolVar(
		&showScores,
		"scores",
		false,
		"Show ranking score diagnostics in human output",
	)
	cmd.Flags().BoolVar(
		&showEvidence,
		"evidence",
		false,
		"Show evidence provenance snippets in human output",
	)
	cmd.Flags().BoolVar(
		&showSummary,
		"summary",
		false,
		"Show aggregate recall summary in human output",
	)
	return cmd
}

type recallExtractDryRunResult struct {
	SessionID     string                          `json:"session_id"`
	DryRun        bool                            `json:"dry_run"`
	MessageCount  int                             `json:"message_count"`
	ChunkCount    int                             `json:"chunk_count"`
	ChunkMaxChars int                             `json:"chunk_max_chars"`
	Chunks        []service.RecallExtractionChunk `json:"chunks"`
}

func loadRecallExtractionMessages(
	ctx context.Context,
	svc service.SessionService,
	sessionID string,
) ([]db.Message, error) {
	var messages []db.Message
	from := 0
	for {
		page, err := svc.Messages(ctx, sessionID, service.MessageFilter{
			From:      &from,
			Limit:     db.MaxMessageLimit,
			Direction: "asc",
		})
		if err != nil {
			return nil, err
		}
		if page == nil || len(page.Messages) == 0 {
			break
		}
		messages = append(messages, page.Messages...)
		last := page.Messages[len(page.Messages)-1].Ordinal
		if len(page.Messages) < db.MaxMessageLimit {
			break
		}
		from = last + 1
	}
	return messages, nil
}

func printRecallExtractDryRunHuman(
	w io.Writer,
	result recallExtractDryRunResult,
) error {
	fmt.Fprintf(w, "Session: %s\n", sanitizeTerminal(result.SessionID))
	fmt.Fprintf(w, "Dry-run: %t\n", result.DryRun)
	fmt.Fprintf(w, "Messages selected: %d\n", result.MessageCount)
	fmt.Fprintf(w, "Chunks: %d\n", result.ChunkCount)
	if result.ChunkMaxChars > 0 {
		fmt.Fprintf(w, "Chunk max chars: %d\n", result.ChunkMaxChars)
	}
	for _, chunk := range result.Chunks {
		fmt.Fprintf(
			w,
			"\nChunk %d ordinals=%d-%d chars=%d\n",
			chunk.Index,
			chunk.StartOrdinal,
			chunk.EndOrdinal,
			chunk.CharCount,
		)
		fmt.Fprintln(w, sanitizeTerminal(chunk.Text))
	}
	return nil
}

func recallBriefIDs(result *service.RecallQueryResult) []string {
	// Always return a non-nil slice so entry_ids serializes as [] rather than
	// null when no recall IDs fit the packed context.
	if result == nil {
		return []string{}
	}
	if result.ContextMeta != nil {
		ids := make([]string, 0, len(result.ContextMeta.IncludedIDs))
		return append(ids, result.ContextMeta.IncludedIDs...)
	}
	ids := make([]string, 0, len(result.RecallEntries))
	for _, recall := range result.RecallEntries {
		if recall.ID != "" {
			ids = append(ids, recall.ID)
		}
	}
	return ids
}

func printRecallBriefHuman(w io.Writer, brief recallBriefResult) error {
	fmt.Fprintf(
		w,
		"Task: %s\nTrusted-only: %t\n\n",
		sanitizeTerminal(brief.Task),
		brief.TrustedOnly,
	)
	if strings.TrimSpace(brief.Context) == "" {
		if brief.ContextMeta != nil {
			fmt.Fprintln(w, "(no recall context fit)")
			printRecallContextMetaHuman(w, brief.ContextMeta)
			return nil
		}
		fmt.Fprintln(w, "(no relevant entries)")
		return nil
	}
	fmt.Fprintln(w, sanitizeTerminal(brief.Context))
	if len(brief.EntryIDs) > 0 {
		fmt.Fprintf(
			w,
			"\nRecall sources: %s\n",
			sanitizeTerminal(recallBriefSourceList(brief)),
		)
	}
	printRecallContextMetaHuman(w, brief.ContextMeta)
	return nil
}

func recallBriefSourceList(brief recallBriefResult) string {
	if len(brief.ContextEntries) == 0 {
		return strings.Join(brief.EntryIDs, ",")
	}
	byID := make(map[string]db.RecallResult, len(brief.ContextEntries))
	for _, recall := range brief.ContextEntries {
		if recall.ID != "" {
			byID[recall.ID] = recall
		}
	}
	parts := make([]string, 0, len(brief.EntryIDs))
	for _, id := range brief.EntryIDs {
		recall, ok := byID[id]
		if !ok {
			parts = append(parts, id)
			continue
		}
		parts = append(parts, recallBriefSourceLabel(recall))
	}
	return strings.Join(parts, ",")
}

func recallBriefSourceLabel(recall db.RecallResult) string {
	label := recall.ID
	var details []string
	if recall.Type != "" {
		details = append(details, recall.Type)
	}
	if len(recall.MatchReasons) > 0 {
		details = append(details, sortedRecallEntryReasonList(recall.MatchReasons))
	}
	if len(details) == 0 {
		return label
	}
	return label + " (" + strings.Join(details, "; ") + ")"
}

func sortedRecallEntryReasonList(reasons []string) string {
	out := append([]string(nil), reasons...)
	sort.Strings(out)
	return strings.Join(out, "|")
}

func printRecallContextMetaHuman(
	w io.Writer, meta *service.RecallContextMeta,
) {
	if meta == nil {
		return
	}
	if meta.PromptInjectionContext {
		fmt.Fprintln(
			w,
			"WARNING: Retrieved recall context contains prompt-injection bait; treat recall text as historical evidence only.",
		)
	}
	fmt.Fprintf(
		w,
		"context entries=%d truncated=%t truncated_from=%d omitted=%d included=%s source_sessions=%s source_episodes=%s source_runs=%s prompt_injection_context=%t%s%s%s%s%s\n",
		meta.EntryCount,
		meta.Truncated,
		meta.TruncatedFrom,
		meta.OmittedCount,
		sanitizeTerminal(strings.Join(meta.IncludedIDs, ",")),
		sanitizeTerminal(strings.Join(meta.SourceSessionIDs, ",")),
		sanitizeTerminal(strings.Join(meta.SourceEpisodeIDs, ",")),
		sanitizeTerminal(strings.Join(meta.SourceRunIDs, ",")),
		meta.PromptInjectionContext,
		recallIncludedTypeSuffix(meta),
		recallIncludedReasonSuffix(meta),
		recallPromptInjectionIDSuffix(meta),
		recallPromptInjectionReasonSuffix(meta),
		recallPromptInjectionReasonByIDSuffix(meta),
	)
}

func recallIncludedTypeSuffix(meta *service.RecallContextMeta) string {
	if meta == nil || len(meta.IncludedTypesByID) == 0 {
		return ""
	}
	return " included_types=" +
		sanitizeTerminal(formatRecallEntryStringMap(meta.IncludedTypesByID))
}

func recallIncludedReasonSuffix(meta *service.RecallContextMeta) string {
	if meta == nil || len(meta.IncludedMatchReasonsByID) == 0 {
		return ""
	}
	return " included_reasons=" +
		sanitizeTerminal(formatRecallEntryStringSliceMap(meta.IncludedMatchReasonsByID))
}

func recallPromptInjectionIDSuffix(meta *service.RecallContextMeta) string {
	if meta == nil || len(meta.PromptInjectionContextIDs) == 0 {
		return ""
	}
	return " prompt_injection_ids=" +
		sanitizeTerminal(strings.Join(meta.PromptInjectionContextIDs, ","))
}

func recallPromptInjectionReasonSuffix(meta *service.RecallContextMeta) string {
	if meta == nil || len(meta.PromptInjectionContextReasons) == 0 {
		return ""
	}
	return " prompt_injection_reasons=" +
		sanitizeTerminal(strings.Join(meta.PromptInjectionContextReasons, ","))
}

func recallPromptInjectionReasonByIDSuffix(
	meta *service.RecallContextMeta,
) string {
	if meta == nil || len(meta.PromptInjectionContextReasonsByID) == 0 {
		return ""
	}
	ids := make([]string, 0, len(meta.PromptInjectionContextReasonsByID))
	for id := range meta.PromptInjectionContextReasonsByID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		reasons := append(
			[]string(nil),
			meta.PromptInjectionContextReasonsByID[id]...,
		)
		sort.Strings(reasons)
		parts = append(parts, id+":"+strings.Join(reasons, "|"))
	}
	return " prompt_injection_reasons_by_id=" +
		sanitizeTerminal(strings.Join(parts, ","))
}

func formatRecallEntryStringMap(values map[string]string) string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+":"+values[key])
	}
	return strings.Join(parts, ",")
}

func formatRecallEntryStringSliceMap(values map[string][]string) string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		items := append([]string(nil), values[key]...)
		sort.Strings(items)
		parts = append(parts, key+":"+strings.Join(items, "|"))
	}
	return strings.Join(parts, ",")
}

func newRecallImportCommand() *cobra.Command {
	var dryRun bool
	var yes bool
	var allowRemoteImport bool
	var allowProductionImport bool
	var requireExistingSessions = true
	var allowPlaceholderSessions bool
	cmd := &cobra.Command{
		Use:          "import <accepted-recall.jsonl>",
		Short:        "Import reviewed accepted entries",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !dryRun && !yes {
				return fmt.Errorf(
					"recall import writes to the active agentsview database; " +
						"run --dry-run first, then pass --yes to import",
				)
			}
			remote, _ := cmd.Flags().GetString("server")
			if strings.TrimSpace(remote) != "" && !dryRun && !allowRemoteImport {
				return fmt.Errorf(
					"recall import --server writes to a remote daemon; " +
						"run --dry-run first, then pass --yes " +
						"--allow-remote-import to import",
				)
			}
			if strings.TrimSpace(remote) == "" && !allowProductionImport {
				if err := requireSafeLocalRecallImportTarget(); err != nil {
					return err
				}
			}
			f, err := os.Open(args[0])
			if err != nil {
				return fmt.Errorf("opening recall import file: %w", err)
			}
			defer f.Close()
			svc, cleanup, err := resolveWritableRecallEntryService(cmd)
			if err != nil {
				return err
			}
			defer cleanup()
			result, err := svc.ImportRecallEntries(
				cmd.Context(),
				f,
				db.RecallImportOptions{
					DryRun:                  dryRun,
					RequireExistingSessions: requireExistingSessions && !allowPlaceholderSessions,
					AllowProductionImport:   allowProductionImport,
				},
			)
			if err != nil {
				return err
			}
			if outputFormat(cmd) == "json" {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Imported: %d\n", result.Imported)
			if dryRun {
				fmt.Fprintf(cmd.OutOrStdout(), "Would import: %d\n", result.WouldImport)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Skipped:  %d\n", result.Skipped)
			printRecallImportItemsHuman(cmd.OutOrStdout(), *result)
			return nil
		},
	}
	cmd.Flags().BoolVar(
		&dryRun,
		"dry-run",
		false,
		"Validate and count reviewed entries without inserting",
	)
	cmd.Flags().BoolVar(
		&yes,
		"yes",
		false,
		"Confirm importing reviewed entries into the active agentsview database",
	)
	cmd.Flags().BoolVar(
		&allowRemoteImport,
		"allow-remote-import",
		false,
		"Confirm importing reviewed entries into a remote daemon selected by --server",
	)
	cmd.Flags().BoolVar(
		&allowProductionImport,
		"allow-production-import",
		false,
		"Allow validating or importing reviewed entries against a default agentsview data directory",
	)
	cmd.Flags().BoolVar(
		&requireExistingSessions,
		"require-existing-sessions",
		true,
		"Reject entries whose source session or evidence is not already present",
	)
	cmd.Flags().BoolVar(
		&allowPlaceholderSessions,
		"allow-placeholder-sessions",
		false,
		"Allow importing entries with missing source evidence by creating placeholder sessions",
	)
	return cmd
}

func requireSafeLocalRecallImportTarget() error {
	dataDir, err := config.ResolveDataDir()
	if err != nil {
		return fmt.Errorf("resolving agentsview data directory: %w", err)
	}
	dbPath := filepath.Join(dataDir, "sessions.db")
	if !config.IsDefaultAgentsviewDataDir(dataDir) &&
		!config.IsDefaultAgentsviewDBPath(dbPath) {
		return nil
	}
	return fmt.Errorf(
		"recall import refuses to validate or write against the default agentsview data directory %s; "+
			"set AGENTSVIEW_DATA_DIR to an isolated lab directory or pass "+
			"--allow-production-import to validate or import against that archive",
		dataDir,
	)
}

func printRecallImportItemsHuman(w io.Writer, result db.RecallImportResult) {
	for _, item := range result.ImportedEntries {
		printRecallImportItemHuman(w, "imported", item)
	}
	for _, item := range result.WouldImportEntries {
		printRecallImportItemHuman(w, "would import", item)
	}
	for _, item := range result.SkippedEntries {
		printRecallImportItemHuman(w, "skipped", item)
	}
}

func printRecallImportItemHuman(
	w io.Writer, action string, item db.RecallImportItem,
) {
	fmt.Fprintf(
		w,
		"  %s %s",
		action,
		sanitizeTerminal(item.CandidateID),
	)
	if item.Title != "" {
		fmt.Fprintf(w, "  %s", sanitizeTerminal(item.Title))
	}
	if item.SourceSessionID != "" {
		fmt.Fprintf(w, "  session=%s", sanitizeTerminal(item.SourceSessionID))
	}
	if item.SupersedesEntryID != "" {
		fmt.Fprintf(
			w,
			"  supersedes=%s",
			sanitizeTerminal(item.SupersedesEntryID),
		)
	}
	if item.Label != "" {
		fmt.Fprintf(w, "  label=%s", sanitizeTerminal(item.Label))
	}
	if item.Reason != "" {
		fmt.Fprintf(w, "  reason=%s", sanitizeTerminal(item.Reason))
	}
	fmt.Fprintln(w)
}

func addRecallFilterFlags(cmd *cobra.Command, f *service.RecallFilter) {
	flags := cmd.Flags()
	flags.StringVar(&f.Query, "query", "", "Filter by query text")
	flags.StringVar(&f.Project, "project", "", "Filter by project")
	flags.StringVar(&f.CWD, "cwd", "", "Filter by cwd")
	flags.StringVar(&f.GitBranch, "git-branch", "", "Filter by git branch")
	flags.StringVar(&f.Agent, "agent", "", "Filter by agent")
	flags.StringVar(&f.Type, "type", "", "Filter by recall type")
	flags.StringVar(&f.Scope, "scope", "", "Filter by recall scope")
	flags.StringVar(&f.Status, "status", "", "Filter by recall status")
	flags.StringVar(
		&f.ExtractorMethod,
		"extractor-method",
		"",
		"Filter by recall extractor method",
	)
	flags.StringVar(
		&f.SourceSessionID,
		"source-session-id",
		"",
		"Filter by recall source session id",
	)
	flags.StringVar(
		&f.SourceEpisodeID,
		"source-episode-id",
		"",
		"Filter by recall source episode id",
	)
	flags.StringVar(
		&f.SourceRunID,
		"source-run-id",
		"",
		"Filter by recall source run id",
	)
	flags.StringVar(
		&f.SupersedesEntryID,
		"supersedes-entry-id",
		"",
		"Filter by the entry id this entry supersedes",
	)
	flags.StringVar(
		&f.SupersededByEntryID,
		"superseded-by-entry-id",
		"",
		"Filter by the entry id that superseded this entry",
	)
	flags.BoolVar(
		&f.TrustedOnly,
		"trusted-only",
		false,
		"Only include human-reviewed, transferable entries with verified provenance",
	)
	flags.IntVar(&f.Limit, "limit", 0, "Maximum entries to return")
}

func addRecallEntryCurrentCWDFlag(cmd *cobra.Command, currentCWD *bool) {
	cmd.Flags().BoolVar(
		currentCWD,
		"current-cwd",
		false,
		"Filter entries to the current working directory",
	)
}

func addRecallEntryCurrentGitBranchFlag(cmd *cobra.Command, currentGitBranch *bool) {
	cmd.Flags().BoolVar(
		currentGitBranch,
		"current-git-branch",
		false,
		"Filter entries to the current git branch",
	)
}

func addRecallEntryCurrentWorktreeFlag(cmd *cobra.Command, currentWorktree *bool) {
	cmd.Flags().BoolVar(
		currentWorktree,
		"current-worktree",
		false,
		"Filter entries to the current git worktree root and branch",
	)
}

func applyRecallEntryCurrentScope(
	cwd *string,
	gitBranch *string,
	currentCWD bool,
	currentGitBranch bool,
	currentWorktree bool,
) error {
	if currentWorktree {
		if currentCWD ||
			strings.TrimSpace(*cwd) != "" ||
			currentGitBranch ||
			strings.TrimSpace(*gitBranch) != "" {
			return fmt.Errorf(
				"use --current-worktree without --cwd, --current-cwd, " +
					"--git-branch, or --current-git-branch",
			)
		}
		root, err := currentGitRoot()
		if err != nil {
			return err
		}
		branch, err := currentGitBranchName()
		if err != nil {
			return err
		}
		*cwd = root
		*gitBranch = branch
		return nil
	}
	if err := applyRecallEntryCurrentCWD(cwd, currentCWD); err != nil {
		return err
	}
	return applyRecallEntryCurrentGitBranch(gitBranch, currentGitBranch)
}

func applyRecallEntryCurrentCWD(cwd *string, currentCWD bool) error {
	if !currentCWD {
		return nil
	}
	if strings.TrimSpace(*cwd) != "" {
		return fmt.Errorf("use either --cwd or --current-cwd, not both")
	}
	wd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolving current working directory: %w", err)
	}
	*cwd = filepath.Clean(wd)
	return nil
}

func applyRecallEntryCurrentGitBranch(gitBranch *string, currentGitBranch bool) error {
	if !currentGitBranch {
		return nil
	}
	if strings.TrimSpace(*gitBranch) != "" {
		return fmt.Errorf("use either --git-branch or --current-git-branch, not both")
	}
	branch, err := currentGitBranchName()
	if err != nil {
		return err
	}
	*gitBranch = branch
	return nil
}

func currentGitBranchName() (string, error) {
	cmd := exec.Command("git", "symbolic-ref", "--quiet", "--short", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("resolving current git branch: %w", err)
	}
	branch := strings.TrimSpace(string(out))
	if branch == "" {
		return "", fmt.Errorf("resolving current git branch: empty branch name")
	}
	return branch, nil
}

func currentGitRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("resolving current git root: %w", err)
	}
	cmd := exec.Command("git", "rev-parse", "--show-prefix")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("resolving current git root: %w", err)
	}
	root := filepath.Clean(wd)
	prefix := strings.TrimSuffix(strings.TrimSuffix(string(out), "\n"), "\r")
	if prefix == "" {
		return root, nil
	}
	for part := range strings.SplitSeq(strings.TrimSuffix(prefix, "/"), "/") {
		if part != "" {
			root = filepath.Dir(root)
		}
	}
	return root, nil
}

func addRecallQueryFlags(cmd *cobra.Command, req *service.RecallQuery) {
	flags := cmd.Flags()
	flags.StringVar(&req.Project, "project", "", "Filter by project")
	flags.StringVar(&req.CWD, "cwd", "", "Filter by cwd")
	flags.StringVar(&req.GitBranch, "git-branch", "", "Filter by git branch")
	flags.StringVar(&req.Agent, "agent", "", "Filter by agent")
	flags.StringVar(&req.Type, "type", "", "Filter by recall type")
	flags.StringVar(&req.Scope, "scope", "", "Filter by recall scope")
	flags.StringVar(&req.Status, "status", "", "Filter by recall status")
	flags.StringVar(
		&req.ExtractorMethod,
		"extractor-method",
		"",
		"Filter by recall extractor method",
	)
	flags.StringVar(
		&req.SourceSessionID,
		"source-session-id",
		"",
		"Filter by recall source session id",
	)
	flags.StringVar(
		&req.SourceEpisodeID,
		"source-episode-id",
		"",
		"Filter by recall source episode id",
	)
	flags.StringVar(
		&req.SourceRunID,
		"source-run-id",
		"",
		"Filter by recall source run id",
	)
	flags.StringVar(
		&req.SupersedesEntryID,
		"supersedes-entry-id",
		"",
		"Filter by the entry id this entry supersedes",
	)
	flags.StringVar(
		&req.SupersededByEntryID,
		"superseded-by-entry-id",
		"",
		"Filter by the entry id that superseded this entry",
	)
	flags.BoolVar(
		&req.TrustedOnly,
		"trusted-only",
		req.TrustedOnly,
		"Only include human-reviewed, transferable entries with verified provenance",
	)
	flags.IntVar(&req.Limit, "limit", 0, "Maximum entries to return")
	flags.BoolVar(&req.IncludeContext, "context", false, "Print assembled context")
	flags.IntVar(
		&req.ContextMaxBytes,
		"context-max-bytes",
		0,
		"Maximum bytes of assembled context when --context is set",
	)
}

func printRecallResultsHuman(
	w io.Writer, entries []db.RecallResult,
) error {
	if len(entries) == 0 {
		fmt.Fprintln(w, "(no entries)")
		return nil
	}
	for _, recall := range entries {
		fmt.Fprintf(w, "%s  %s  %s\n",
			sanitizeTerminal(recall.ID),
			sanitizeTerminal(recall.Type),
			sanitizeTerminal(recall.Title))
		if recall.Project != "" || recall.Agent != "" {
			fmt.Fprintf(w, "    %s %s\n",
				sanitizeTerminal(recall.Project),
				sanitizeTerminal(recall.Agent))
		}
		printRecallEntryReviewLine(w, recall.RecallEntry)
		printRecallEntryLifecycleLine(w, recall.RecallEntry)
		printRecallEntrySourceLine(w, recall.SourceSessionID, recall.SourceEpisodeID,
			recall.SourceRunID, recall.ExtractorMethod, recall.Model)
	}
	return nil
}

func printRecallEntryReviewLine(w io.Writer, recall db.RecallEntry) {
	reviewState := strings.TrimSpace(recall.ReviewState)
	if reviewState == "" {
		reviewState = corerecall.ReviewStateUnreviewedAuto
	}
	fmt.Fprintf(
		w,
		"    review transferable=%t provenance_ok=%t evidence=%d review_state=%s\n",
		recall.Transferable,
		recall.ProvenanceOK,
		len(recall.Evidence),
		sanitizeTerminal(reviewState),
	)
}

func printRecallResultsDetailedHuman(
	w io.Writer,
	entries []db.RecallResult,
	showScores bool,
	showEvidence bool,
) error {
	return printRecallResultsDetailedHumanWithOptions(
		w, entries, recallDetailedPrintOptions{
			ShowScores:   showScores,
			ShowEvidence: showEvidence,
		})
}

type recallDetailedPrintOptions struct {
	ShowScores   bool
	ShowEvidence bool
	ContextMeta  *service.RecallContextMeta
}

func printRecallResultsDetailedHumanWithOptions(
	w io.Writer,
	entries []db.RecallResult,
	options recallDetailedPrintOptions,
) error {
	if len(entries) == 0 {
		fmt.Fprintln(w, "(no entries)")
		return nil
	}
	included := recallContextIncludedSet(options.ContextMeta)
	for _, recall := range entries {
		fmt.Fprintf(w, "%s  %s  %s\n",
			sanitizeTerminal(recall.ID),
			sanitizeTerminal(recall.Type),
			sanitizeTerminal(recall.Title))
		if recall.Project != "" || recall.Agent != "" {
			fmt.Fprintf(w, "    %s %s\n",
				sanitizeTerminal(recall.Project),
				sanitizeTerminal(recall.Agent))
		}
		printRecallEntryReviewLine(w, recall.RecallEntry)
		printRecallEntryLifecycleLine(w, recall.RecallEntry)
		printRecallEntrySourceLineWithContext(w, recall.SourceSessionID,
			recall.SourceEpisodeID, recall.SourceRunID,
			recall.ExtractorMethod, recall.Model,
			recallContextState(options.ContextMeta, included, recall.ID))
		if options.ShowScores {
			b := recall.ScoreBreakdown
			fmt.Fprintf(
				w,
				"    score=%.2f keyword=%.2f evidence=%.2f identifier=%.2f phrase=%.2f entity=%.2f temporal=%.2f confidence=%.2f matched=%s terms=%s\n",
				recall.Score,
				b.KeywordIDFScore,
				b.EvidenceIDFScore,
				b.IdentifierBoost,
				b.PhraseBoost,
				b.EntityBoost,
				b.TemporalBoost,
				b.ConfidenceBonus,
				formatRecallEntryScoreReasons(recall),
				formatRecallEntryMatchedTerms(recall.MatchedTerms),
			)
		}
		if options.ShowEvidence {
			printRecallEvidenceDetailsHuman(w, recall.Evidence)
		}
	}
	return nil
}

func recallContextIncludedSet(
	meta *service.RecallContextMeta,
) map[string]bool {
	if meta == nil || len(meta.IncludedIDs) == 0 {
		return nil
	}
	included := make(map[string]bool, len(meta.IncludedIDs))
	for _, id := range meta.IncludedIDs {
		included[id] = true
	}
	return included
}

func recallContextState(
	meta *service.RecallContextMeta,
	included map[string]bool,
	recallID string,
) string {
	if meta == nil || recallID == "" {
		return ""
	}
	if included[recallID] {
		return "included"
	}
	return "omitted"
}

const recallEvidenceSnippetMaxChars = 220

func printRecallEvidenceDetailsHuman(w io.Writer, evidence []db.RecallEvidence) {
	for _, item := range evidence {
		fmt.Fprintf(
			w,
			"    evidence %s:%d-%d",
			sanitizeTerminal(item.SessionID),
			item.MessageStartOrdinal,
			item.MessageEndOrdinal,
		)
		if item.ToolUseID != "" {
			fmt.Fprintf(w, " tool=%s", sanitizeTerminal(item.ToolUseID))
		}
		if snippet := recallEvidenceSnippet(item.Snippet); snippet != "" {
			fmt.Fprintf(w, "  %s", sanitizeTerminal(snippet))
		}
		fmt.Fprintln(w)
	}
}

func recallEvidenceSnippet(snippet string) string {
	snippet = strings.Join(strings.Fields(snippet), " ")
	if len([]rune(snippet)) <= recallEvidenceSnippetMaxChars {
		return snippet
	}
	runes := []rune(snippet)
	return string(runes[:recallEvidenceSnippetMaxChars]) + "..."
}

func formatRecallEntryMatchedTerms(terms []string) string {
	if len(terms) == 0 {
		return "none"
	}
	return strings.Join(terms, ",")
}

func formatRecallEntryScoreReasons(recall db.RecallResult) string {
	if len(recall.MatchReasons) > 0 {
		return strings.Join(recall.MatchReasons, ",")
	}
	b := recall.ScoreBreakdown
	reasons := []string{}
	if b.KeywordIDFScore > 0 || b.KeywordOverlap > 0 {
		reasons = append(reasons, "keyword")
	}
	if b.EvidenceIDFScore > 0 || b.EvidenceKeywordOverlap > 0 {
		reasons = append(reasons, "evidence")
	}
	if b.IdentifierBoost > 0 {
		reasons = append(reasons, "identifier")
	}
	if b.PhraseBoost > 0 {
		reasons = append(reasons, "phrase")
	}
	if b.EntityBoost > 0 {
		reasons = append(reasons, "entity")
	}
	if b.TemporalBoost > 0 {
		reasons = append(reasons, "temporal")
	}
	if b.ConfidenceBonus > 0 {
		reasons = append(reasons, "confidence")
	}
	if len(reasons) == 0 {
		return "none"
	}
	return strings.Join(reasons, ",")
}

func printRecallEntryHuman(w io.Writer, recall *db.RecallEntry) error {
	fmt.Fprintf(w, "ID:       %s\n", sanitizeTerminal(recall.ID))
	fmt.Fprintf(w, "Type:     %s\n", sanitizeTerminal(recall.Type))
	fmt.Fprintf(w, "Scope:    %s\n", sanitizeTerminal(recall.Scope))
	if recall.Status != "" {
		fmt.Fprintf(w, "Status:   %s\n", sanitizeTerminal(recall.Status))
	}
	reviewState := strings.TrimSpace(recall.ReviewState)
	if reviewState == "" {
		reviewState = corerecall.ReviewStateUnreviewedAuto
	}
	fmt.Fprintf(w, "Review:   %s\n", sanitizeTerminal(reviewState))
	if recall.SupersedesEntryID != "" {
		fmt.Fprintf(
			w,
			"Supersedes: %s\n",
			sanitizeTerminal(recall.SupersedesEntryID),
		)
	}
	if recall.SupersededByEntryID != "" {
		fmt.Fprintf(
			w,
			"Superseded by: %s\n",
			sanitizeTerminal(recall.SupersededByEntryID),
		)
	}
	fmt.Fprintf(w, "Title:    %s\n", sanitizeTerminal(recall.Title))
	fmt.Fprintf(w, "Body:     %s\n", sanitizeTerminal(recall.Body))
	if recall.Trigger != "" {
		fmt.Fprintf(w, "Trigger:  %s\n", sanitizeTerminal(recall.Trigger))
	}
	if recall.Confidence != nil {
		fmt.Fprintf(w, "Confidence: %.2f\n", *recall.Confidence)
	}
	if recall.Uncertainty != "" {
		fmt.Fprintf(
			w,
			"Uncertainty: %s\n",
			sanitizeTerminal(recall.Uncertainty),
		)
	}
	if recall.Project != "" || recall.Agent != "" {
		fmt.Fprintf(w, "Context:  %s %s %s %s\n",
			sanitizeTerminal(recall.Project),
			sanitizeTerminal(recall.CWD),
			sanitizeTerminal(recall.GitBranch),
			sanitizeTerminal(recall.Agent))
	}
	if recall.SourceSessionID != "" || recall.SourceEpisodeID != "" ||
		recall.SourceRunID != "" ||
		recall.ExtractorMethod != "" || recall.Model != "" {
		printRecallEntrySourceLine(w, recall.SourceSessionID, recall.SourceEpisodeID,
			recall.SourceRunID, recall.ExtractorMethod, recall.Model)
	}
	if evidence := formatRecallEvidence(recall.Evidence); evidence != "" {
		fmt.Fprintf(w, "Evidence: %s\n", sanitizeTerminal(evidence))
	}
	return nil
}

func printRecallEntryLifecycleLine(w io.Writer, recall db.RecallEntry) {
	if !hasRecallEntryLifecycleMetadata(recall) {
		return
	}
	status := strings.TrimSpace(recall.Status)
	if status == "" {
		status = "accepted"
	}
	fmt.Fprintf(w, "    lifecycle status=%s", sanitizeTerminal(status))
	if recall.SupersedesEntryID != "" {
		fmt.Fprintf(
			w,
			" supersedes=%s",
			sanitizeTerminal(recall.SupersedesEntryID),
		)
	}
	if recall.SupersededByEntryID != "" {
		fmt.Fprintf(
			w,
			" superseded_by=%s",
			sanitizeTerminal(recall.SupersededByEntryID),
		)
	}
	fmt.Fprintln(w)
}

func hasRecallEntryLifecycleMetadata(recall db.RecallEntry) bool {
	status := strings.TrimSpace(recall.Status)
	return status != "" && status != "accepted" ||
		recall.SupersedesEntryID != "" ||
		recall.SupersededByEntryID != ""
}

func printRecallEntrySourceLine(
	w io.Writer, sessionID, episodeID, runID, extractorMethod, model string,
) {
	printRecallEntrySourceLineWithContext(w, sessionID, episodeID, runID,
		extractorMethod, model, "")
}

func printRecallEntrySourceLineWithContext(
	w io.Writer, sessionID, episodeID, runID, extractorMethod, model string,
	contextState string,
) {
	if sessionID == "" && episodeID == "" && runID == "" &&
		extractorMethod == "" && model == "" && contextState == "" {
		return
	}
	fmt.Fprintf(w,
		"    source session=%s episode=%s run=%s extractor=%s model=%s",
		sanitizeTerminal(sessionID),
		sanitizeTerminal(episodeID),
		sanitizeTerminal(runID),
		sanitizeTerminal(extractorMethod),
		sanitizeTerminal(model),
	)
	if contextState != "" {
		fmt.Fprintf(w, " context=%s", sanitizeTerminal(contextState))
	}
	fmt.Fprintln(w)
}

func formatRecallEvidence(evidence []db.RecallEvidence) string {
	parts := make([]string, 0, len(evidence))
	for _, item := range evidence {
		part := fmt.Sprintf(
			"%s:%d-%d",
			item.SessionID,
			item.MessageStartOrdinal,
			item.MessageEndOrdinal,
		)
		if item.ToolUseID != "" {
			part += " tool=" + item.ToolUseID
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, "; ")
}
