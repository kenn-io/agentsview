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
	"go.kenn.io/agentsview/internal/service"
)

func newMemoryCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "memory",
		Short:        "Build and inspect project memories",
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
		"Remote daemon URL for memory API requests",
	)

	cmd.AddCommand(newMemoryListCommand())
	cmd.AddCommand(newMemoryGetCommand())
	cmd.AddCommand(newMemoryQueryCommand())
	cmd.AddCommand(newMemoryStatsCommand())
	cmd.AddCommand(newMemoryBriefCommand())
	cmd.AddCommand(newMemoryExtractCommand())
	cmd.AddCommand(newMemoryImportCommand())
	return cmd
}

func resolveMemoryService(
	cmd *cobra.Command,
) (service.SessionService, func(), error) {
	remote, _ := cmd.Flags().GetString("server")
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return resolveService(cmd)
	}
	cfg, err := config.LoadPFlags(cmd.Flags())
	if err != nil {
		return nil, nil, fmt.Errorf("loading config: %w", err)
	}
	return service.NewHTTPBackend(remote, cfg.AuthToken, false), func() {}, nil
}

// resolveWritableMemoryService is the write-capable counterpart of
// resolveMemoryService for `memory import`. Local imports go through
// resolveWritableService so a read-only daemon (pg serve) is refused up front
// with actionable guidance instead of failing at the import endpoint.
func resolveWritableMemoryService(
	cmd *cobra.Command,
) (service.SessionService, func(), error) {
	remote, _ := cmd.Flags().GetString("server")
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return resolveWritableService(cmd)
	}
	cfg, err := config.LoadPFlags(cmd.Flags())
	if err != nil {
		return nil, nil, fmt.Errorf("loading config: %w", err)
	}
	return service.NewHTTPBackend(remote, cfg.AuthToken, false), func() {}, nil
}

func newMemoryListCommand() *cobra.Command {
	var f service.MemoryFilter
	var currentCWD bool
	var currentGitBranch bool
	var currentWorktree bool
	cmd := &cobra.Command{
		Use:          "list",
		Short:        "List accepted memories",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, cleanup, err := resolveMemoryService(cmd)
			if err != nil {
				return err
			}
			defer cleanup()
			if err := applyMemoryCurrentScope(
				&f.CWD, &f.GitBranch, currentCWD, currentGitBranch,
				currentWorktree,
			); err != nil {
				return err
			}
			list, err := svc.ListMemories(cmd.Context(), f)
			if err != nil {
				return err
			}
			if outputFormat(cmd) == "json" {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(list)
			}
			out := cmd.OutOrStdout()
			printMemoryTrustedOnlyHuman(out, list.TrustedOnly)
			return printMemoryResultsHuman(out, list.Memories)
		},
	}
	addMemoryFilterFlags(cmd, &f)
	addMemoryCurrentCWDFlag(cmd, &currentCWD)
	addMemoryCurrentGitBranchFlag(cmd, &currentGitBranch)
	addMemoryCurrentWorktreeFlag(cmd, &currentWorktree)
	return cmd
}

type memoryStatsResult struct {
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

func newMemoryStatsCommand() *cobra.Command {
	var f service.MemoryFilter
	var currentCWD bool
	var currentGitBranch bool
	var currentWorktree bool
	cmd := &cobra.Command{
		Use:          "stats",
		Short:        "Summarize accepted memories",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, cleanup, err := resolveMemoryService(cmd)
			if err != nil {
				return err
			}
			defer cleanup()
			if err := applyMemoryCurrentScope(
				&f.CWD, &f.GitBranch, currentCWD, currentGitBranch,
				currentWorktree,
			); err != nil {
				return err
			}
			if f.Limit <= 0 {
				f.Limit = db.MaxMemoryLimit
			}
			list, err := svc.ListMemories(cmd.Context(), f)
			if err != nil {
				return err
			}
			stats := buildMemoryStats(list.Memories, f.Limit)
			stats.TrustedOnly = list.TrustedOnly
			if outputFormat(cmd) == "json" {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(stats)
			}
			return printMemoryStatsHuman(cmd.OutOrStdout(), stats)
		},
	}
	addMemoryFilterFlags(cmd, &f)
	addMemoryCurrentCWDFlag(cmd, &currentCWD)
	addMemoryCurrentGitBranchFlag(cmd, &currentGitBranch)
	addMemoryCurrentWorktreeFlag(cmd, &currentWorktree)
	return cmd
}

func buildMemoryStats(
	memories []db.MemoryResult,
	limit int,
) memoryStatsResult {
	stats := memoryStatsResult{
		Count:           len(memories),
		Limit:           limit,
		Truncated:       limit > 0 && len(memories) >= limit,
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
	for _, memory := range memories {
		countMemoryStat(stats.ByType, memory.Type)
		countMemoryStat(stats.ByScope, memory.Scope)
		countMemoryStat(stats.ByStatus, memory.Status)
		countMemoryStat(stats.ByProject, memory.Project)
		countMemoryStat(stats.ByAgent, memory.Agent)
		countMemoryStat(stats.ByExtractor, memory.ExtractorMethod)
		countMemoryStat(stats.BySourceRun, memory.SourceRunID)
		countMemoryStat(stats.BySourceSession, memory.SourceSessionID)
		countMemoryStat(stats.BySourceEpisode, memory.SourceEpisodeID)
		countMemoryStat(
			stats.ByTransferable,
			memoryStatsBoolLabel(
				memory.Transferable, "transferable", "not_transferable",
			),
		)
		countMemoryStat(
			stats.ByProvenance,
			memoryStatsBoolLabel(
				memory.ProvenanceOK,
				"provenance_ok",
				"provenance_unverified",
			),
		)
		countMemoryStat(
			stats.ByEvidence,
			memoryStatsBoolLabel(
				len(memory.Evidence) > 0,
				"with_evidence",
				"without_evidence",
			),
		)
		countMemoryStat(stats.ByLifecycle, memory.LifecycleBucket())
	}
	return stats
}

func memoryStatsBoolLabel(ok bool, trueLabel, falseLabel string) string {
	if ok {
		return trueLabel
	}
	return falseLabel
}

func countMemoryStat(counts map[string]int, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		value = "(none)"
	}
	counts[value]++
}

func printMemoryStatsHuman(w io.Writer, stats memoryStatsResult) error {
	fmt.Fprintf(w, "Total: %d\n", stats.Count)
	fmt.Fprintf(w, "Limit: %d\n", stats.Limit)
	fmt.Fprintf(w, "Truncated: %t\n", stats.Truncated)
	fmt.Fprintf(w, "Trusted-only: %t\n", stats.TrustedOnly)
	printMemoryStatsSection(w, "By type:", stats.ByType)
	printMemoryStatsSection(w, "By scope:", stats.ByScope)
	printMemoryStatsSection(w, "By status:", stats.ByStatus)
	printMemoryStatsSection(w, "By project:", stats.ByProject)
	printMemoryStatsSection(w, "By agent:", stats.ByAgent)
	printMemoryStatsSection(w, "By extractor:", stats.ByExtractor)
	printMemoryStatsSection(w, "By source run:", stats.BySourceRun)
	printMemoryStatsSection(w, "By source session:", stats.BySourceSession)
	printMemoryStatsSection(w, "By source episode:", stats.BySourceEpisode)
	printMemoryStatsSection(w, "By transferability:", stats.ByTransferable)
	printMemoryStatsSection(w, "By provenance audit:", stats.ByProvenance)
	printMemoryStatsSection(w, "By evidence:", stats.ByEvidence)
	printMemoryStatsSection(w, "By lifecycle:", stats.ByLifecycle)
	return nil
}

func printMemoryStatsSection(
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

func newMemoryGetCommand() *cobra.Command {
	var showEvidence bool
	cmd := &cobra.Command{
		Use:          "get <id>",
		Short:        "Get one accepted memory",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, cleanup, err := resolveMemoryService(cmd)
			if err != nil {
				return err
			}
			defer cleanup()
			memory, err := svc.GetMemory(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if memory == nil {
				return fmt.Errorf("memory %s not found", args[0])
			}
			if outputFormat(cmd) == "json" {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(memory)
			}
			if err := printMemoryHuman(cmd.OutOrStdout(), memory); err != nil {
				return err
			}
			if showEvidence {
				printMemoryEvidenceDetailsHuman(cmd.OutOrStdout(), memory.Evidence)
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

func newMemoryQueryCommand() *cobra.Command {
	var req service.MemoryQuery
	var showScores bool
	var showEvidence bool
	var showSummary bool
	var currentCWD bool
	var currentGitBranch bool
	var currentWorktree bool
	cmd := &cobra.Command{
		Use:          "query <text>",
		Short:        "Query accepted memories",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, cleanup, err := resolveMemoryService(cmd)
			if err != nil {
				return err
			}
			defer cleanup()
			req.Query = args[0]
			if err := applyMemoryCurrentScope(
				&req.CWD, &req.GitBranch, currentCWD, currentGitBranch,
				currentWorktree,
			); err != nil {
				return err
			}
			result, err := svc.QueryMemories(cmd.Context(), req)
			if err != nil {
				return err
			}
			if outputFormat(cmd) == "json" {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
			}
			out := cmd.OutOrStdout()
			printMemoryTrustedOnlyHuman(out, result.TrustedOnly)
			if req.IncludeContext && (result.Context != "" || result.ContextMeta != nil) {
				if result.Context != "" {
					fmt.Fprintln(out, sanitizeTerminal(result.Context))
				} else {
					fmt.Fprintln(out, "(no memory context fit)")
				}
				printMemoryContextMetaHuman(out, result.ContextMeta)
				if showSummary {
					printMemoryQuerySummaryHuman(out, result.Memories)
					printMemoryQuerySummaryStructHuman(
						out, "Context summary", result.ContextSummary,
					)
				}
				if showScores || showEvidence {
					return printMemoryResultsDetailedHumanWithOptions(
						out, result.Memories, memoryDetailedPrintOptions{
							ShowScores:   showScores,
							ShowEvidence: showEvidence,
							ContextMeta:  result.ContextMeta,
						})
				}
				return nil
			}
			if showSummary {
				printMemoryQuerySummaryHuman(out, result.Memories)
			}
			if showScores || showEvidence {
				return printMemoryResultsDetailedHuman(
					out, result.Memories,
					showScores, showEvidence,
				)
			}
			return printMemoryResultsHuman(out, result.Memories)
		},
	}
	addMemoryQueryFlags(cmd, &req)
	addMemoryCurrentCWDFlag(cmd, &currentCWD)
	addMemoryCurrentGitBranchFlag(cmd, &currentGitBranch)
	addMemoryCurrentWorktreeFlag(cmd, &currentWorktree)
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

func printMemoryTrustedOnlyHuman(w io.Writer, trustedOnly bool) {
	fmt.Fprintf(w, "Trusted-only: %t\n", trustedOnly)
}

func printMemoryQuerySummaryHuman(
	w io.Writer,
	memories []db.MemoryResult,
) {
	summary := service.BuildMemoryQuerySummary(memories)
	printMemoryQuerySummaryStructHuman(w, "Summary", summary)
}

func printMemoryQuerySummaryStructHuman(
	w io.Writer,
	label string,
	summary *service.MemoryQuerySummary,
) {
	if summary == nil {
		return
	}
	fmt.Fprintf(
		w,
		"%s: %d %s\n",
		label,
		summary.Count,
		memoryCountNoun(summary.Count),
	)
	printMemoryStatsSection(w, "By type:", summary.ByType)
	printMemoryStatsSection(w, "By scope:", summary.ByScope)
	printMemoryStatsSection(w, "By status:", summary.ByStatus)
	printMemoryStatsSection(w, "By project:", summary.ByProject)
	printMemoryStatsSection(w, "By agent:", summary.ByAgent)
	printMemoryStatsSection(w, "By cwd:", summary.ByCWD)
	printMemoryStatsSection(w, "By git branch:", summary.ByGitBranch)
	printMemoryStatsSection(w, "By match reason:", summary.ByMatchReason)
	printMemoryStatsSection(w, "By extractor:", summary.ByExtractorMethod)
	printMemoryStatsSection(w, "By model:", summary.ByModel)
	printMemoryStatsSection(w, "By source run:", summary.BySourceRun)
	printMemoryStatsSection(w, "By source session:", summary.BySourceSession)
	printMemoryStatsSection(w, "By source episode:", summary.BySourceEpisode)
	printMemoryStatsSection(w, "By transferability:", summary.ByTransferability)
	printMemoryStatsSection(w, "By provenance audit:", summary.ByProvenanceAudit)
	printMemoryStatsSection(w, "By evidence:", summary.ByEvidence)
	printMemoryStatsSection(w, "By lifecycle:", summary.ByLifecycle)
}

func memoryCountNoun(count int) string {
	if count == 1 {
		return "memory"
	}
	return "memories"
}

type memoryBriefResult struct {
	Task            string                      `json:"task"`
	TrustedOnly     bool                        `json:"trusted_only"`
	Context         string                      `json:"context"`
	ContextMeta     *service.MemoryContextMeta  `json:"context_meta,omitempty"`
	Summary         *service.MemoryQuerySummary `json:"summary,omitempty"`
	ContextSummary  *service.MemoryQuerySummary `json:"context_summary,omitempty"`
	MemoryIDs       []string                    `json:"memory_ids"`
	ContextMemories []db.MemoryResult           `json:"context_memories,omitempty"`
	Memories        []db.MemoryResult           `json:"memories"`
}

func newMemoryBriefCommand() *cobra.Command {
	var req service.MemoryQuery
	var currentCWD bool
	var currentGitBranch bool
	var currentWorktree bool
	var showScores bool
	var showEvidence bool
	var showSummary bool
	req.TrustedOnly = true
	cmd := &cobra.Command{
		Use:          "brief <task>",
		Short:        "Write a task briefing from accepted memories",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, cleanup, err := resolveMemoryService(cmd)
			if err != nil {
				return err
			}
			defer cleanup()
			req.Query = args[0]
			req.IncludeContext = true
			if err := applyMemoryCurrentScope(
				&req.CWD, &req.GitBranch, currentCWD, currentGitBranch,
				currentWorktree,
			); err != nil {
				return err
			}
			result, err := svc.QueryMemories(cmd.Context(), req)
			if err != nil {
				return err
			}
			summary := result.Summary
			if summary == nil {
				summary = service.BuildMemoryQuerySummary(result.Memories)
			}
			brief := memoryBriefResult{
				Task:            req.Query,
				TrustedOnly:     req.TrustedOnly,
				Context:         result.Context,
				ContextMeta:     result.ContextMeta,
				Summary:         summary,
				ContextSummary:  result.ContextSummary,
				MemoryIDs:       memoryBriefIDs(result),
				ContextMemories: result.ContextMemories,
				Memories:        result.Memories,
			}
			if outputFormat(cmd) == "json" {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(brief)
			}
			if err := printMemoryBriefHuman(cmd.OutOrStdout(), brief); err != nil {
				return err
			}
			if showSummary {
				printMemoryQuerySummaryHuman(cmd.OutOrStdout(), result.Memories)
				printMemoryQuerySummaryStructHuman(
					cmd.OutOrStdout(), "Context summary",
					result.ContextSummary,
				)
			}
			if showScores || showEvidence {
				return printMemoryResultsDetailedHumanWithOptions(
					cmd.OutOrStdout(), result.Memories,
					memoryDetailedPrintOptions{
						ShowScores:   showScores,
						ShowEvidence: showEvidence,
						ContextMeta:  result.ContextMeta,
					})
			}
			return nil
		},
	}
	addMemoryQueryFlags(cmd, &req)
	if err := cmd.Flags().MarkHidden("context"); err != nil {
		panic(err)
	}
	if flag := cmd.Flags().Lookup("context-max-bytes"); flag != nil {
		flag.Usage = "Maximum bytes of assembled context"
	}
	addMemoryCurrentCWDFlag(cmd, &currentCWD)
	addMemoryCurrentGitBranchFlag(cmd, &currentGitBranch)
	addMemoryCurrentWorktreeFlag(cmd, &currentWorktree)
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

type memoryExtractDryRunResult struct {
	SessionID     string                          `json:"session_id"`
	DryRun        bool                            `json:"dry_run"`
	MessageCount  int                             `json:"message_count"`
	ChunkCount    int                             `json:"chunk_count"`
	ChunkMaxChars int                             `json:"chunk_max_chars"`
	Chunks        []service.MemoryExtractionChunk `json:"chunks"`
}

func newMemoryExtractCommand() *cobra.Command {
	var sessionID string
	var dryRun bool
	var chunkMaxChars int
	cmd := &cobra.Command{
		Use:          "extract --session <id> --dry-run",
		Short:        "Preview session chunks for memory extraction",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(sessionID) == "" {
				return fmt.Errorf("memory extract requires --session")
			}
			if !dryRun {
				return fmt.Errorf(
					"memory extract currently supports --dry-run only; " +
						"model-backed fact extraction is not wired yet",
				)
			}
			svc, cleanup, err := resolveMemoryService(cmd)
			if err != nil {
				return err
			}
			defer cleanup()
			messages, err := loadMemoryExtractionMessages(
				cmd.Context(), svc, sessionID,
			)
			if err != nil {
				return err
			}
			chunks := service.BuildMemoryExtractionChunks(
				sessionID, messages,
				service.MemoryExtractionChunkOptions{MaxChars: chunkMaxChars},
			)
			result := memoryExtractDryRunResult{
				SessionID:     sessionID,
				DryRun:        true,
				MessageCount:  len(messages),
				ChunkCount:    len(chunks),
				ChunkMaxChars: chunkMaxChars,
				Chunks:        chunks,
			}
			if outputFormat(cmd) == "json" {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
			}
			return printMemoryExtractDryRunHuman(cmd.OutOrStdout(), result)
		},
	}
	cmd.Flags().StringVar(
		&sessionID,
		"session",
		"",
		"Session id to analyze",
	)
	cmd.Flags().BoolVar(
		&dryRun,
		"dry-run",
		false,
		"Print memory extraction chunks without storing facts",
	)
	cmd.Flags().IntVar(
		&chunkMaxChars,
		"chunk-max-chars",
		0,
		"Maximum characters per analysis chunk",
	)
	return cmd
}

func loadMemoryExtractionMessages(
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

func printMemoryExtractDryRunHuman(
	w io.Writer,
	result memoryExtractDryRunResult,
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

func memoryBriefIDs(result *service.MemoryQueryResult) []string {
	// Always return a non-nil slice so memory_ids serializes as [] rather than
	// null when no memory IDs fit the packed context.
	if result == nil {
		return []string{}
	}
	if result.ContextMeta != nil {
		ids := make([]string, 0, len(result.ContextMeta.IncludedIDs))
		return append(ids, result.ContextMeta.IncludedIDs...)
	}
	ids := make([]string, 0, len(result.Memories))
	for _, memory := range result.Memories {
		if memory.ID != "" {
			ids = append(ids, memory.ID)
		}
	}
	return ids
}

func printMemoryBriefHuman(w io.Writer, brief memoryBriefResult) error {
	fmt.Fprintf(
		w,
		"Task: %s\nTrusted-only: %t\n\n",
		sanitizeTerminal(brief.Task),
		brief.TrustedOnly,
	)
	if strings.TrimSpace(brief.Context) == "" {
		if brief.ContextMeta != nil {
			fmt.Fprintln(w, "(no memory context fit)")
			printMemoryContextMetaHuman(w, brief.ContextMeta)
			return nil
		}
		fmt.Fprintln(w, "(no relevant memories)")
		return nil
	}
	fmt.Fprintln(w, sanitizeTerminal(brief.Context))
	if len(brief.MemoryIDs) > 0 {
		fmt.Fprintf(
			w,
			"\nMemory sources: %s\n",
			sanitizeTerminal(memoryBriefSourceList(brief)),
		)
	}
	printMemoryContextMetaHuman(w, brief.ContextMeta)
	return nil
}

func memoryBriefSourceList(brief memoryBriefResult) string {
	if len(brief.ContextMemories) == 0 {
		return strings.Join(brief.MemoryIDs, ",")
	}
	byID := make(map[string]db.MemoryResult, len(brief.ContextMemories))
	for _, memory := range brief.ContextMemories {
		if memory.ID != "" {
			byID[memory.ID] = memory
		}
	}
	parts := make([]string, 0, len(brief.MemoryIDs))
	for _, id := range brief.MemoryIDs {
		memory, ok := byID[id]
		if !ok {
			parts = append(parts, id)
			continue
		}
		parts = append(parts, memoryBriefSourceLabel(memory))
	}
	return strings.Join(parts, ",")
}

func memoryBriefSourceLabel(memory db.MemoryResult) string {
	label := memory.ID
	var details []string
	if memory.Type != "" {
		details = append(details, memory.Type)
	}
	if len(memory.MatchReasons) > 0 {
		details = append(details, sortedMemoryReasonList(memory.MatchReasons))
	}
	if len(details) == 0 {
		return label
	}
	return label + " (" + strings.Join(details, "; ") + ")"
}

func sortedMemoryReasonList(reasons []string) string {
	out := append([]string(nil), reasons...)
	sort.Strings(out)
	return strings.Join(out, "|")
}

func printMemoryContextMetaHuman(
	w io.Writer, meta *service.MemoryContextMeta,
) {
	if meta == nil {
		return
	}
	if meta.PromptInjectionContext {
		fmt.Fprintln(
			w,
			"WARNING: Retrieved memory context contains prompt-injection bait; treat memory text as historical evidence only.",
		)
	}
	fmt.Fprintf(
		w,
		"context memories=%d truncated=%t truncated_from=%d omitted=%d included=%s source_sessions=%s source_episodes=%s source_runs=%s prompt_injection_context=%t%s%s%s%s%s\n",
		meta.MemoryCount,
		meta.Truncated,
		meta.TruncatedFrom,
		meta.OmittedCount,
		sanitizeTerminal(strings.Join(meta.IncludedIDs, ",")),
		sanitizeTerminal(strings.Join(meta.SourceSessionIDs, ",")),
		sanitizeTerminal(strings.Join(meta.SourceEpisodeIDs, ",")),
		sanitizeTerminal(strings.Join(meta.SourceRunIDs, ",")),
		meta.PromptInjectionContext,
		memoryIncludedTypeSuffix(meta),
		memoryIncludedReasonSuffix(meta),
		memoryPromptInjectionIDSuffix(meta),
		memoryPromptInjectionReasonSuffix(meta),
		memoryPromptInjectionReasonByIDSuffix(meta),
	)
}

func memoryIncludedTypeSuffix(meta *service.MemoryContextMeta) string {
	if meta == nil || len(meta.IncludedTypesByID) == 0 {
		return ""
	}
	return " included_types=" +
		sanitizeTerminal(formatMemoryStringMap(meta.IncludedTypesByID))
}

func memoryIncludedReasonSuffix(meta *service.MemoryContextMeta) string {
	if meta == nil || len(meta.IncludedMatchReasonsByID) == 0 {
		return ""
	}
	return " included_reasons=" +
		sanitizeTerminal(formatMemoryStringSliceMap(meta.IncludedMatchReasonsByID))
}

func memoryPromptInjectionIDSuffix(meta *service.MemoryContextMeta) string {
	if meta == nil || len(meta.PromptInjectionContextIDs) == 0 {
		return ""
	}
	return " prompt_injection_ids=" +
		sanitizeTerminal(strings.Join(meta.PromptInjectionContextIDs, ","))
}

func memoryPromptInjectionReasonSuffix(meta *service.MemoryContextMeta) string {
	if meta == nil || len(meta.PromptInjectionContextReasons) == 0 {
		return ""
	}
	return " prompt_injection_reasons=" +
		sanitizeTerminal(strings.Join(meta.PromptInjectionContextReasons, ","))
}

func memoryPromptInjectionReasonByIDSuffix(
	meta *service.MemoryContextMeta,
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

func formatMemoryStringMap(values map[string]string) string {
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

func formatMemoryStringSliceMap(values map[string][]string) string {
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

func newMemoryImportCommand() *cobra.Command {
	var dryRun bool
	var yes bool
	var allowRemoteImport bool
	var allowProductionImport bool
	var requireExistingSessions = true
	var allowPlaceholderSessions bool
	cmd := &cobra.Command{
		Use:          "import <accepted-memories.jsonl>",
		Short:        "Import reviewed accepted memories",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !dryRun && !yes {
				return fmt.Errorf(
					"memory import writes to the active agentsview database; " +
						"run --dry-run first, then pass --yes to import",
				)
			}
			remote, _ := cmd.Flags().GetString("server")
			if strings.TrimSpace(remote) != "" && !dryRun && !allowRemoteImport {
				return fmt.Errorf(
					"memory import --server writes to a remote daemon; " +
						"run --dry-run first, then pass --yes " +
						"--allow-remote-import to import",
				)
			}
			if strings.TrimSpace(remote) == "" && !allowProductionImport {
				if err := requireSafeLocalMemoryImportTarget(); err != nil {
					return err
				}
			}
			f, err := os.Open(args[0])
			if err != nil {
				return fmt.Errorf("opening memory import file: %w", err)
			}
			defer f.Close()
			svc, cleanup, err := resolveWritableMemoryService(cmd)
			if err != nil {
				return err
			}
			defer cleanup()
			result, err := svc.ImportMemories(
				cmd.Context(),
				f,
				db.MemoryImportOptions{
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
			printMemoryImportItemsHuman(cmd.OutOrStdout(), *result)
			return nil
		},
	}
	cmd.Flags().BoolVar(
		&dryRun,
		"dry-run",
		false,
		"Validate and count reviewed memories without inserting",
	)
	cmd.Flags().BoolVar(
		&yes,
		"yes",
		false,
		"Confirm importing reviewed memories into the active agentsview database",
	)
	cmd.Flags().BoolVar(
		&allowRemoteImport,
		"allow-remote-import",
		false,
		"Confirm importing reviewed memories into a remote daemon selected by --server",
	)
	cmd.Flags().BoolVar(
		&allowProductionImport,
		"allow-production-import",
		false,
		"Allow validating or importing reviewed memories against a default agentsview data directory",
	)
	cmd.Flags().BoolVar(
		&requireExistingSessions,
		"require-existing-sessions",
		true,
		"Reject memories whose source session or evidence is not already present",
	)
	cmd.Flags().BoolVar(
		&allowPlaceholderSessions,
		"allow-placeholder-sessions",
		false,
		"Allow importing memories with missing source evidence by creating placeholder sessions",
	)
	return cmd
}

func requireSafeLocalMemoryImportTarget() error {
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
		"memory import refuses to validate or write against the default agentsview data directory %s; "+
			"set AGENTSVIEW_DATA_DIR to an isolated lab directory or pass "+
			"--allow-production-import to validate or import against that archive",
		dataDir,
	)
}

func printMemoryImportItemsHuman(w io.Writer, result db.MemoryImportResult) {
	for _, item := range result.ImportedMemories {
		printMemoryImportItemHuman(w, "imported", item)
	}
	for _, item := range result.WouldImportMemories {
		printMemoryImportItemHuman(w, "would import", item)
	}
	for _, item := range result.SkippedMemories {
		printMemoryImportItemHuman(w, "skipped", item)
	}
}

func printMemoryImportItemHuman(
	w io.Writer, action string, item db.MemoryImportItem,
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
	if item.SupersedesMemoryID != "" {
		fmt.Fprintf(
			w,
			"  supersedes=%s",
			sanitizeTerminal(item.SupersedesMemoryID),
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

func addMemoryFilterFlags(cmd *cobra.Command, f *service.MemoryFilter) {
	flags := cmd.Flags()
	flags.StringVar(&f.Query, "query", "", "Filter by query text")
	flags.StringVar(&f.Project, "project", "", "Filter by project")
	flags.StringVar(&f.CWD, "cwd", "", "Filter by cwd")
	flags.StringVar(&f.GitBranch, "git-branch", "", "Filter by git branch")
	flags.StringVar(&f.Agent, "agent", "", "Filter by agent")
	flags.StringVar(&f.Type, "type", "", "Filter by memory type")
	flags.StringVar(&f.Scope, "scope", "", "Filter by memory scope")
	flags.StringVar(&f.Status, "status", "", "Filter by memory status")
	flags.StringVar(
		&f.ExtractorMethod,
		"extractor-method",
		"",
		"Filter by memory extractor method",
	)
	flags.StringVar(
		&f.SourceSessionID,
		"source-session-id",
		"",
		"Filter by memory source session id",
	)
	flags.StringVar(
		&f.SourceEpisodeID,
		"source-episode-id",
		"",
		"Filter by memory source episode id",
	)
	flags.StringVar(
		&f.SourceRunID,
		"source-run-id",
		"",
		"Filter by memory source run id",
	)
	flags.StringVar(
		&f.SupersedesMemoryID,
		"supersedes-memory-id",
		"",
		"Filter by memory id this memory supersedes",
	)
	flags.StringVar(
		&f.SupersededByMemoryID,
		"superseded-by-memory-id",
		"",
		"Filter by memory id that superseded this memory",
	)
	flags.BoolVar(
		&f.TrustedOnly,
		"trusted-only",
		false,
		"Only include memories marked transferable with verified provenance",
	)
	flags.IntVar(&f.Limit, "limit", 0, "Maximum memories to return")
}

func addMemoryCurrentCWDFlag(cmd *cobra.Command, currentCWD *bool) {
	cmd.Flags().BoolVar(
		currentCWD,
		"current-cwd",
		false,
		"Filter memories to the current working directory",
	)
}

func addMemoryCurrentGitBranchFlag(cmd *cobra.Command, currentGitBranch *bool) {
	cmd.Flags().BoolVar(
		currentGitBranch,
		"current-git-branch",
		false,
		"Filter memories to the current git branch",
	)
}

func addMemoryCurrentWorktreeFlag(cmd *cobra.Command, currentWorktree *bool) {
	cmd.Flags().BoolVar(
		currentWorktree,
		"current-worktree",
		false,
		"Filter memories to the current git worktree root and branch",
	)
}

func applyMemoryCurrentScope(
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
	if err := applyMemoryCurrentCWD(cwd, currentCWD); err != nil {
		return err
	}
	return applyMemoryCurrentGitBranch(gitBranch, currentGitBranch)
}

func applyMemoryCurrentCWD(cwd *string, currentCWD bool) error {
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

func applyMemoryCurrentGitBranch(gitBranch *string, currentGitBranch bool) error {
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

func addMemoryQueryFlags(cmd *cobra.Command, req *service.MemoryQuery) {
	flags := cmd.Flags()
	flags.StringVar(&req.Project, "project", "", "Filter by project")
	flags.StringVar(&req.CWD, "cwd", "", "Filter by cwd")
	flags.StringVar(&req.GitBranch, "git-branch", "", "Filter by git branch")
	flags.StringVar(&req.Agent, "agent", "", "Filter by agent")
	flags.StringVar(&req.Type, "type", "", "Filter by memory type")
	flags.StringVar(&req.Scope, "scope", "", "Filter by memory scope")
	flags.StringVar(&req.Status, "status", "", "Filter by memory status")
	flags.StringVar(
		&req.ExtractorMethod,
		"extractor-method",
		"",
		"Filter by memory extractor method",
	)
	flags.StringVar(
		&req.SourceSessionID,
		"source-session-id",
		"",
		"Filter by memory source session id",
	)
	flags.StringVar(
		&req.SourceEpisodeID,
		"source-episode-id",
		"",
		"Filter by memory source episode id",
	)
	flags.StringVar(
		&req.SourceRunID,
		"source-run-id",
		"",
		"Filter by memory source run id",
	)
	flags.StringVar(
		&req.SupersedesMemoryID,
		"supersedes-memory-id",
		"",
		"Filter by memory id this memory supersedes",
	)
	flags.StringVar(
		&req.SupersededByMemoryID,
		"superseded-by-memory-id",
		"",
		"Filter by memory id that superseded this memory",
	)
	flags.BoolVar(
		&req.TrustedOnly,
		"trusted-only",
		req.TrustedOnly,
		"Only include memories marked transferable with verified provenance",
	)
	flags.IntVar(&req.Limit, "limit", 0, "Maximum memories to return")
	flags.BoolVar(&req.IncludeContext, "context", false, "Print assembled context")
	flags.IntVar(
		&req.ContextMaxBytes,
		"context-max-bytes",
		0,
		"Maximum bytes of assembled context when --context is set",
	)
}

func printMemoryResultsHuman(
	w io.Writer, memories []db.MemoryResult,
) error {
	if len(memories) == 0 {
		fmt.Fprintln(w, "(no memories)")
		return nil
	}
	for _, memory := range memories {
		fmt.Fprintf(w, "%s  %s  %s\n",
			sanitizeTerminal(memory.ID),
			sanitizeTerminal(memory.Type),
			sanitizeTerminal(memory.Title))
		if memory.Project != "" || memory.Agent != "" {
			fmt.Fprintf(w, "    %s %s\n",
				sanitizeTerminal(memory.Project),
				sanitizeTerminal(memory.Agent))
		}
		printMemoryReviewLine(w, memory.Memory)
		printMemoryLifecycleLine(w, memory.Memory)
		printMemorySourceLine(w, memory.SourceSessionID, memory.SourceEpisodeID,
			memory.SourceRunID, memory.ExtractorMethod, memory.Model)
	}
	return nil
}

func printMemoryReviewLine(w io.Writer, memory db.Memory) {
	fmt.Fprintf(
		w,
		"    review transferable=%t provenance_ok=%t evidence=%d\n",
		memory.Transferable,
		memory.ProvenanceOK,
		len(memory.Evidence),
	)
}

func printMemoryResultsDetailedHuman(
	w io.Writer,
	memories []db.MemoryResult,
	showScores bool,
	showEvidence bool,
) error {
	return printMemoryResultsDetailedHumanWithOptions(
		w, memories, memoryDetailedPrintOptions{
			ShowScores:   showScores,
			ShowEvidence: showEvidence,
		})
}

type memoryDetailedPrintOptions struct {
	ShowScores   bool
	ShowEvidence bool
	ContextMeta  *service.MemoryContextMeta
}

func printMemoryResultsDetailedHumanWithOptions(
	w io.Writer,
	memories []db.MemoryResult,
	options memoryDetailedPrintOptions,
) error {
	if len(memories) == 0 {
		fmt.Fprintln(w, "(no memories)")
		return nil
	}
	included := memoryContextIncludedSet(options.ContextMeta)
	for _, memory := range memories {
		fmt.Fprintf(w, "%s  %s  %s\n",
			sanitizeTerminal(memory.ID),
			sanitizeTerminal(memory.Type),
			sanitizeTerminal(memory.Title))
		if memory.Project != "" || memory.Agent != "" {
			fmt.Fprintf(w, "    %s %s\n",
				sanitizeTerminal(memory.Project),
				sanitizeTerminal(memory.Agent))
		}
		printMemoryReviewLine(w, memory.Memory)
		printMemoryLifecycleLine(w, memory.Memory)
		printMemorySourceLineWithContext(w, memory.SourceSessionID,
			memory.SourceEpisodeID, memory.SourceRunID,
			memory.ExtractorMethod, memory.Model,
			memoryContextState(options.ContextMeta, included, memory.ID))
		if options.ShowScores {
			b := memory.ScoreBreakdown
			fmt.Fprintf(
				w,
				"    score=%.2f keyword=%.2f evidence=%.2f identifier=%.2f phrase=%.2f entity=%.2f temporal=%.2f confidence=%.2f matched=%s terms=%s\n",
				memory.Score,
				b.KeywordIDFScore,
				b.EvidenceIDFScore,
				b.IdentifierBoost,
				b.PhraseBoost,
				b.EntityBoost,
				b.TemporalBoost,
				b.ConfidenceBonus,
				formatMemoryScoreReasons(memory),
				formatMemoryMatchedTerms(memory.MatchedTerms),
			)
		}
		if options.ShowEvidence {
			printMemoryEvidenceDetailsHuman(w, memory.Evidence)
		}
	}
	return nil
}

func memoryContextIncludedSet(
	meta *service.MemoryContextMeta,
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

func memoryContextState(
	meta *service.MemoryContextMeta,
	included map[string]bool,
	memoryID string,
) string {
	if meta == nil || memoryID == "" {
		return ""
	}
	if included[memoryID] {
		return "included"
	}
	return "omitted"
}

const memoryEvidenceSnippetMaxChars = 220

func printMemoryEvidenceDetailsHuman(w io.Writer, evidence []db.MemoryEvidence) {
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
		if snippet := memoryEvidenceSnippet(item.Snippet); snippet != "" {
			fmt.Fprintf(w, "  %s", sanitizeTerminal(snippet))
		}
		fmt.Fprintln(w)
	}
}

func memoryEvidenceSnippet(snippet string) string {
	snippet = strings.Join(strings.Fields(snippet), " ")
	if len([]rune(snippet)) <= memoryEvidenceSnippetMaxChars {
		return snippet
	}
	runes := []rune(snippet)
	return string(runes[:memoryEvidenceSnippetMaxChars]) + "..."
}

func formatMemoryMatchedTerms(terms []string) string {
	if len(terms) == 0 {
		return "none"
	}
	return strings.Join(terms, ",")
}

func formatMemoryScoreReasons(memory db.MemoryResult) string {
	if len(memory.MatchReasons) > 0 {
		return strings.Join(memory.MatchReasons, ",")
	}
	b := memory.ScoreBreakdown
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

func printMemoryHuman(w io.Writer, memory *db.Memory) error {
	fmt.Fprintf(w, "ID:       %s\n", sanitizeTerminal(memory.ID))
	fmt.Fprintf(w, "Type:     %s\n", sanitizeTerminal(memory.Type))
	fmt.Fprintf(w, "Scope:    %s\n", sanitizeTerminal(memory.Scope))
	if memory.Status != "" {
		fmt.Fprintf(w, "Status:   %s\n", sanitizeTerminal(memory.Status))
	}
	if memory.SupersedesMemoryID != "" {
		fmt.Fprintf(
			w,
			"Supersedes: %s\n",
			sanitizeTerminal(memory.SupersedesMemoryID),
		)
	}
	if memory.SupersededByMemoryID != "" {
		fmt.Fprintf(
			w,
			"Superseded by: %s\n",
			sanitizeTerminal(memory.SupersededByMemoryID),
		)
	}
	fmt.Fprintf(w, "Title:    %s\n", sanitizeTerminal(memory.Title))
	fmt.Fprintf(w, "Body:     %s\n", sanitizeTerminal(memory.Body))
	if memory.Trigger != "" {
		fmt.Fprintf(w, "Trigger:  %s\n", sanitizeTerminal(memory.Trigger))
	}
	if memory.Confidence != nil {
		fmt.Fprintf(w, "Confidence: %.2f\n", *memory.Confidence)
	}
	if memory.Uncertainty != "" {
		fmt.Fprintf(
			w,
			"Uncertainty: %s\n",
			sanitizeTerminal(memory.Uncertainty),
		)
	}
	if memory.Project != "" || memory.Agent != "" {
		fmt.Fprintf(w, "Context:  %s %s %s %s\n",
			sanitizeTerminal(memory.Project),
			sanitizeTerminal(memory.CWD),
			sanitizeTerminal(memory.GitBranch),
			sanitizeTerminal(memory.Agent))
	}
	if memory.SourceSessionID != "" || memory.SourceEpisodeID != "" ||
		memory.SourceRunID != "" ||
		memory.ExtractorMethod != "" || memory.Model != "" {
		printMemorySourceLine(w, memory.SourceSessionID, memory.SourceEpisodeID,
			memory.SourceRunID, memory.ExtractorMethod, memory.Model)
	}
	if evidence := formatMemoryEvidence(memory.Evidence); evidence != "" {
		fmt.Fprintf(w, "Evidence: %s\n", sanitizeTerminal(evidence))
	}
	return nil
}

func printMemoryLifecycleLine(w io.Writer, memory db.Memory) {
	if !hasMemoryLifecycleMetadata(memory) {
		return
	}
	status := strings.TrimSpace(memory.Status)
	if status == "" {
		status = "accepted"
	}
	fmt.Fprintf(w, "    lifecycle status=%s", sanitizeTerminal(status))
	if memory.SupersedesMemoryID != "" {
		fmt.Fprintf(
			w,
			" supersedes=%s",
			sanitizeTerminal(memory.SupersedesMemoryID),
		)
	}
	if memory.SupersededByMemoryID != "" {
		fmt.Fprintf(
			w,
			" superseded_by=%s",
			sanitizeTerminal(memory.SupersededByMemoryID),
		)
	}
	fmt.Fprintln(w)
}

func hasMemoryLifecycleMetadata(memory db.Memory) bool {
	status := strings.TrimSpace(memory.Status)
	return status != "" && status != "accepted" ||
		memory.SupersedesMemoryID != "" ||
		memory.SupersededByMemoryID != ""
}

func printMemorySourceLine(
	w io.Writer, sessionID, episodeID, runID, extractorMethod, model string,
) {
	printMemorySourceLineWithContext(w, sessionID, episodeID, runID,
		extractorMethod, model, "")
}

func printMemorySourceLineWithContext(
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

func formatMemoryEvidence(evidence []db.MemoryEvidence) string {
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
