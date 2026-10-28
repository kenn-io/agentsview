// ABOUTME: Read-only, evidence-based session usage diagnostics.
// ABOUTME: Reuses archived usage accounting and does not emit transcript content.
package main

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"sort"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/service"
	"go.kenn.io/agentsview/internal/signals"
)

type diagnosisTool struct {
	Ordinal           int    `json:"ordinal"`
	Tool              string `json:"tool"`
	ResultLength      int    `json:"result_length"`
	SubagentSessionID string `json:"subagent_session_id,omitempty"`
}
type diagnosisChildLink struct {
	Ordinal   int    `json:"ordinal"`
	Tool      string `json:"tool"`
	SessionID string `json:"session_id"`
}

type diagnosisRetry struct {
	Tool         string `json:"tool"`
	Count        int    `json:"count"`
	FirstOrdinal int    `json:"first_ordinal"`
	LastOrdinal  int    `json:"last_ordinal"`
}
type diagnosisContext struct {
	Ordinal       int    `json:"ordinal"`
	Timestamp     string `json:"timestamp"`
	Model         string `json:"model"`
	ContextTokens int    `json:"context_tokens"`
}
type diagnosisTokens struct {
	UncachedInput int `json:"uncached_input"`
	CacheRead     int `json:"cache_read"`
	CacheWrite    int `json:"cache_write"`
	Output        int `json:"output"`
}

type sessionDiagnosis struct {
	SchemaVersion int                  `json:"schema_version"`
	SessionID     string               `json:"session_id"`
	Scope         string               `json:"scope"`
	Coverage      diagnosisCoverage    `json:"coverage"`
	Usage         *sessionUsageOutput  `json:"usage"`
	ObservedRows  diagnosisTokens      `json:"observed_usage_rows"`
	Context       []diagnosisContext   `json:"context_timeline"`
	Compactions   []int                `json:"compaction_ordinals"`
	LargeResults  []diagnosisTool      `json:"largest_tool_results"`
	RetryRuns     []diagnosisRetry     `json:"retry_runs"`
	ChildLinks    []diagnosisChildLink `json:"child_links"`
}
type diagnosisCoverage struct {
	RequestCount            *int   `json:"request_count"`
	RequestCountStatus      string `json:"request_count_status"`
	CacheMissReasons        string `json:"cache_miss_reasons"`
	ToolEvidenceScope       string `json:"tool_evidence_scope"`
	MessagesComplete        bool   `json:"messages_complete"`
	UsageStatus             string `json:"usage_status"`
	CacheBucketAvailability string `json:"cache_bucket_availability"`
}

func newSessionDiagnoseCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "diagnose <id>", Short: "Diagnose archived session usage and tool activity", Args: cobra.ExactArgs(1), SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, cleanup, err := resolveService(cmd)
			if err != nil {
				return err
			}
			defer cleanup()
			id, err := resolveServiceSessionID(cmd.Context(), svc, args[0])
			if err != nil {
				return err
			}
			if noSync, _ := cmd.Flags().GetBool("no-sync"); !noSync {
				return fmt.Errorf("diagnose only reads archived usage")
			}
			usage, _, err := sessionUsageDataForCommand(cmd, id)
			if err != nil {
				return err
			}
			if usage == nil {
				return fmt.Errorf("session usage unavailable for %s", id)
			}
			messages, complete, err := diagnosisMessages(cmd.Context(), svc, id)
			if err != nil {
				return err
			}
			calls, err := svc.ToolCalls(cmd.Context(), id)
			if err != nil {
				return err
			}
			out := buildSessionDiagnosis(id, usage, messages, calls.ToolCalls, complete)
			if ownOnly, _ := cmd.Flags().GetBool("own-only"); ownOnly {
				out.Scope = "usage, tool and context evidence cover the named session only"
			}
			if outputFormat(cmd) == "json" {
				return json.MarshalEncode(jsontext.NewEncoder(cmd.OutOrStdout(), jsontext.WithIndent("  ")), out)
			}
			return renderSessionDiagnosis(cmd.OutOrStdout(), out)
		},
	}
	// Always query the archive; usage's refresh path can write to it.
	cmd.Flags().Bool("no-sync", true, "Use archived usage without refreshing sources")
	_ = cmd.Flags().MarkHidden("no-sync")
	cmd.Flags().Bool("own-only", false, "Exclude subagent usage")
	return cmd
}

func diagnosisMessages(ctx context.Context, svc service.SessionService, id string) ([]db.Message, bool, error) {
	const maxMessages = 10000
	var all []db.Message
	from := 0
	revision, source := "", ""
	for len(all) < maxMessages {
		page, err := svc.Messages(ctx, id, service.MessageFilter{From: &from, Limit: db.MaxMessageLimit, Direction: "asc", ExpectedRevision: revision, EvidenceSource: source})
		if err != nil {
			return nil, false, err
		}
		if len(page.Messages) == 0 {
			return all, true, nil
		}
		if revision == "" {
			revision, source = page.TranscriptRevision, page.EvidenceSource
		}
		all = append(all, page.Messages...)
		if len(page.Messages) < db.MaxMessageLimit {
			return all, true, nil
		}
		next := page.Messages[len(page.Messages)-1].Ordinal + 1
		if next <= from {
			return nil, false, fmt.Errorf("message pagination did not advance")
		}
		from = next
	}
	return all, false, nil
}

func buildSessionDiagnosis(id string, usage *sessionUsageOutput, messages []db.Message, calls []service.ToolCall, complete bool) *sessionDiagnosis {
	// Project is not needed for diagnosis and may contain private information.
	safeUsage := *usage
	safeUsage.Project = ""
	out := &sessionDiagnosis{SchemaVersion: 1, SessionID: id, Scope: "usage includes linked subagents; tool and context evidence is the named session only", Usage: &safeUsage,
		Coverage: diagnosisCoverage{RequestCountStatus: "unavailable: usage rows do not always represent requests", CacheMissReasons: "unavailable: provider diagnostics not captured", ToolEvidenceScope: "named session only; lengths are not billed tokens", MessagesComplete: complete, UsageStatus: "archived; completeness unknown", CacheBucketAvailability: "unknown: zero can mean missing or unreported"},
		Context:  []diagnosisContext{}, Compactions: []int{}, LargeResults: []diagnosisTool{}, RetryRuns: []diagnosisRetry{}, ChildLinks: []diagnosisChildLink{}}
	if !usage.HasTokenData {
		out.Coverage.UsageStatus = "no token data reported"
	}
	for _, row := range usage.Breakdown {
		out.ObservedRows.UncachedInput += row.InputTokens
		out.ObservedRows.CacheRead += row.CacheReadInputTokens
		out.ObservedRows.CacheWrite += row.CacheCreationInputTokens
		out.ObservedRows.Output += row.OutputTokens
	}
	for _, m := range messages {
		if m.HasContextTokens {
			out.Context = append(out.Context, diagnosisContext{m.Ordinal, m.Timestamp, m.Model, m.ContextTokens})
		}
		if m.IsCompactBoundary {
			out.Compactions = append(out.Compactions, m.Ordinal)
		}
	}
	rows := make([]signals.ToolCallRow, 0, len(calls))
	for i, c := range calls {
		if c.SubagentSessionID != "" {
			out.ChildLinks = append(out.ChildLinks, diagnosisChildLink{c.Ordinal, c.ToolName, c.SubagentSessionID})
		}
		if c.ResultLength > 0 {
			out.LargeResults = append(out.LargeResults, diagnosisTool{c.Ordinal, c.ToolName, c.ResultLength, c.SubagentSessionID})
		}
		// Missing arguments cannot establish identical calls; unique placeholders break runs.
		input := c.InputJSON
		if input == "" {
			input = fmt.Sprintf("\x00missing:%d", i)
		}
		rows = append(rows, signals.ToolCallRow{ToolName: c.ToolName, InputJSON: input, MessageOrdinal: c.Ordinal, CallIndex: i})
	}
	sort.SliceStable(out.LargeResults, func(i, j int) bool { return out.LargeResults[i].ResultLength > out.LargeResults[j].ResultLength })
	if len(out.LargeResults) > 10 {
		out.LargeResults = out.LargeResults[:10]
	}
	for _, run := range signals.RetryRuns(rows) {
		out.RetryRuns = append(out.RetryRuns, diagnosisRetry{run.ToolName, run.Count, run.First.MessageOrdinal, run.Last.MessageOrdinal})
	}
	return out
}

func renderSessionDiagnosis(w io.Writer, d *sessionDiagnosis) error {
	if _, err := fmt.Fprintf(w, "Session: %s\nScope: %s\n", sanitizeTerminal(d.SessionID), d.Scope); err != nil {
		return err
	}
	if err := renderSessionUsageHuman(w, d.Usage); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "Observed usage rows: uncached input %d; cache read %d; cache write %d; output %d (coverage may be incomplete)\n", d.ObservedRows.UncachedInput, d.ObservedRows.CacheRead, d.ObservedRows.CacheWrite, d.ObservedRows.Output); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w, "Request count: unavailable (usage rows may not be requests)\nCache miss reasons: unavailable\nContext observations: %d; compactions: %v; retry runs: %d\n", len(d.Context), d.Compactions, len(d.RetryRuns))
	if err != nil {
		return err
	}
	for _, c := range d.Context {
		if _, err := fmt.Fprintf(w, "  Context #%d: %d tokens (%s)\n", c.Ordinal, c.ContextTokens, sanitizeTerminal(c.Model)); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(w, "Largest tool results (length is not billed tokens):"); err != nil {
		return err
	}
	for _, c := range d.LargeResults {
		if _, err := fmt.Fprintf(w, "  #%d %s: %d\n", c.Ordinal, sanitizeTerminal(c.Tool), c.ResultLength); err != nil {
			return err
		}
	}
	for _, link := range d.ChildLinks {
		if _, err := fmt.Fprintf(w, "  Child: %s via %s (#%d)\n", sanitizeTerminal(link.SessionID), sanitizeTerminal(link.Tool), link.Ordinal); err != nil {
			return err
		}
	}
	for _, r := range d.RetryRuns {
		if _, err := fmt.Fprintf(w, "  Retry candidate: %s x%d (#%d–#%d; identical recorded arguments)\n", sanitizeTerminal(r.Tool), r.Count, r.FirstOrdinal, r.LastOrdinal); err != nil {
			return err
		}
	}
	if !d.Coverage.MessagesComplete {
		_, err = fmt.Fprintln(w, "Message evidence truncated at 10000 rows")
	}
	return err
}
