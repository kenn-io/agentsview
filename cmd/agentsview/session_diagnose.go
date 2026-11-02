// ABOUTME: Read-only, evidence-based session usage diagnostics.
// ABOUTME: Reuses archived usage accounting and does not emit transcript content.
package main

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
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
	Ordinal   *int   `json:"ordinal,omitempty"`
	Tool      string `json:"tool,omitempty"`
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
	SchemaVersion int                         `json:"schema_version"`
	SessionID     string                      `json:"session_id"`
	Scope         string                      `json:"scope"`
	Coverage      diagnosisCoverage           `json:"coverage"`
	Usage         *service.SessionUsageReport `json:"usage"`
	ObservedRows  diagnosisTokens             `json:"observed_usage_rows"`
	Context       []diagnosisContext          `json:"context_timeline"`
	Compactions   []int                       `json:"compaction_ordinals"`
	LargeResults  []diagnosisTool             `json:"largest_tool_results"`
	RetryRuns     []diagnosisRetry            `json:"retry_runs"`
	ChildLinks    []diagnosisChildLink        `json:"child_links"`
}
type diagnosisCoverage struct {
	RequestCount            *int   `json:"request_count"`
	RequestCountStatus      string `json:"request_count_status"`
	CacheMissReasons        string `json:"cache_miss_reasons"`
	ToolEvidenceScope       string `json:"tool_evidence_scope"`
	UsageStatus             string `json:"usage_status"`
	CacheBucketAvailability string `json:"cache_bucket_availability"`
}

func newSessionDiagnoseCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "diagnose <id>",
		Short:        "Diagnose archived session usage and tool activity",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			backend, err := resolveSessionUsageBackend(cmd, false)
			if err != nil {
				return err
			}
			defer closeArchiveQueryBackend(backend.cleanup)
			svc := backend.service
			usage, code, err := backend.read(args[0])
			if err != nil {
				return err
			}
			if code == tokenUseExitNotFound {
				return fmt.Errorf("%w: %s", errSessionNotFound, args[0])
			}
			if usage == nil {
				return fmt.Errorf("session usage unavailable for %s", args[0])
			}
			id := usage.SessionID
			out := buildSessionDiagnosis(id, &usage.SessionUsageReport)
			ownOnly, _ := cmd.Flags().GetBool("own-only")
			if ownOnly {
				out.Scope = "usage, tool and context evidence cover the named session only; child links remain listed"
			}
			if err := out.readMessages(cmd.Context(), svc); err != nil {
				return err
			}
			children, err := svc.ChildSessions(cmd.Context(), id)
			if err != nil {
				return err
			}
			linked := make(map[string]bool)
			for _, link := range out.ChildLinks {
				linked[link.SessionID] = true
			}
			for _, child := range children {
				if !linked[child.ID] {
					out.ChildLinks = append(out.ChildLinks, diagnosisChildLink{SessionID: child.ID})
					linked[child.ID] = true
				}
			}
			if outputFormat(cmd) == "json" {
				return json.MarshalEncode(jsontext.NewEncoder(cmd.OutOrStdout(), jsontext.WithIndent("  ")), out)
			}
			return renderSessionDiagnosis(cmd.OutOrStdout(), out)
		},
	}
	cmd.Flags().Bool("own-only", false, "Exclude subagent usage")
	return cmd
}

func buildSessionDiagnosis(id string, usage *service.SessionUsageReport) *sessionDiagnosis {
	// Project is not needed for diagnosis and may contain private information.
	safeUsage := *usage
	safeUsage.Project = ""
	out := &sessionDiagnosis{
		SchemaVersion: 1,
		SessionID:     id,
		Scope:         "usage includes linked subagents; tool and context evidence is the named session only",
		Usage:         &safeUsage,
		Coverage: diagnosisCoverage{
			RequestCountStatus:      "unavailable: usage rows do not always represent requests",
			CacheMissReasons:        "unavailable: provider diagnostics not captured",
			ToolEvidenceScope:       "named session only; lengths are not billed tokens",
			UsageStatus:             "archived; completeness unknown",
			CacheBucketAvailability: "unknown: zero can mean missing or unreported",
		},
		Context:      []diagnosisContext{},
		Compactions:  []int{},
		LargeResults: []diagnosisTool{},
		RetryRuns:    []diagnosisRetry{},
		ChildLinks:   []diagnosisChildLink{},
	}
	if !usage.HasTokenData {
		out.Coverage.UsageStatus = "no token data reported"
	}
	for _, row := range usage.Breakdown {
		out.ObservedRows.UncachedInput += row.InputTokens
		out.ObservedRows.CacheRead += row.CacheReadInputTokens
		out.ObservedRows.CacheWrite += row.CacheCreationInputTokens
		out.ObservedRows.Output += row.OutputTokens
	}
	return out
}

// readMessages scans archived evidence in pages and collects calls for retry detection.
func (out *sessionDiagnosis) readMessages(ctx context.Context, svc service.SessionService) error {
	from := 0
	var calls []signals.ToolCallRow
	linked := make(map[string]bool)
	for {
		page, err := svc.Messages(ctx, out.SessionID, service.MessageFilter{From: &from, Limit: db.MaxMessageLimit, Direction: "asc"})
		if err != nil {
			return err
		}
		if page == nil || len(page.Messages) == 0 {
			break
		}
		for _, m := range page.Messages {
			if m.HasContextTokens {
				out.Context = append(out.Context, diagnosisContext{m.Ordinal, m.Timestamp, m.Model, m.ContextTokens})
			}
			if m.IsCompactBoundary {
				out.Compactions = append(out.Compactions, m.Ordinal)
			}
			for i, c := range m.ToolCalls {
				if c.SubagentSessionID != "" && !linked[c.SubagentSessionID] {
					out.ChildLinks = append(out.ChildLinks, diagnosisChildLink{new(m.Ordinal), c.ToolName, c.SubagentSessionID})
					linked[c.SubagentSessionID] = true
				}
				if c.ResultContentLength > 0 {
					out.LargeResults = append(out.LargeResults, diagnosisTool{m.Ordinal, c.ToolName, c.ResultContentLength, c.SubagentSessionID})
					sort.SliceStable(out.LargeResults, func(i, j int) bool { return out.LargeResults[i].ResultLength > out.LargeResults[j].ResultLength })
					if len(out.LargeResults) > 10 {
						out.LargeResults = out.LargeResults[:10]
					}
				}
				calls = append(calls, signals.ToolCallRow{ToolName: c.ToolName, InputJSON: c.InputJSON, MessageOrdinal: m.Ordinal, CallIndex: i})
			}
		}
		if len(page.Messages) < db.MaxMessageLimit {
			break
		}
		next := page.Messages[len(page.Messages)-1].Ordinal + 1
		if next <= from {
			return errors.New("message pagination did not advance")
		}
		from = next
	}
	for _, run := range signals.RetryRuns(calls) {
		out.RetryRuns = append(out.RetryRuns, diagnosisRetry{run.ToolName, run.Count, run.First.MessageOrdinal, run.Last.MessageOrdinal})
	}
	return nil
}

func renderSessionDiagnosis(w io.Writer, d *sessionDiagnosis) error {
	if _, err := fmt.Fprintf(w, "Session: %s\nScope: %s\n", sanitizeTerminal(d.SessionID), d.Scope); err != nil {
		return err
	}
	if err := renderSessionUsageHuman(w, &sessionUsageOutput{SessionUsageReport: *d.Usage}); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "Observed usage rows: uncached input %d; cache read %d; cache write %d; output %d (coverage may be incomplete)\n", d.ObservedRows.UncachedInput, d.ObservedRows.CacheRead, d.ObservedRows.CacheWrite, d.ObservedRows.Output); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w, "Request count: unavailable (usage rows may not be requests)\nCache miss reasons: unavailable\nContext observations: %d; compactions: %v; retry runs: %d\n", len(d.Context), d.Compactions, len(d.RetryRuns))
	if err != nil {
		return err
	}
	context := d.Context
	if len(context) > 10 {
		context = context[len(context)-10:]
	}
	for _, c := range context {
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
		via := ""
		if link.Ordinal != nil {
			via = fmt.Sprintf(" via %s (#%d)", sanitizeTerminal(link.Tool), *link.Ordinal)
		}
		if _, err := fmt.Fprintf(w, "  Child: %s%s\n", sanitizeTerminal(link.SessionID), via); err != nil {
			return err
		}
	}
	for _, r := range d.RetryRuns {
		if _, err := fmt.Fprintf(w, "  Retry candidate: %s x%d (#%d–#%d; identical recorded arguments)\n", sanitizeTerminal(r.Tool), r.Count, r.FirstOrdinal, r.LastOrdinal); err != nil {
			return err
		}
	}
	return err
}
