package friction

import (
	"fmt"
	"strconv"
	"strings"

	"go.kenn.io/agentsview/internal/serdejson"
)

// SummarySchemaVersion is 3: jilog's schema 2 plus the D36
// frustrations/interruptions count keys (spec §9.2).
const SummarySchemaVersion = 3

// SummaryMeta is the per-render context the snapshot does not hold.
// DigestPath is nil on a dry run.
type SummaryMeta struct {
	DigestPath      *string
	CreatedIssues   []IssueRef
	TrackerFailures int
}

func jsonInt(n int) serdejson.Number     { return serdejson.Number(strconv.Itoa(n)) }
func jsonUint(n uint64) serdejson.Number { return serdejson.Number(strconv.FormatUint(n, 10)) }

func usdMap(m map[string]USD) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v.String()
	}
	return out
}

// RenderSummaryJSON ports digest_report_json (commands/review.rs:276-383)
// printed as `to_string_pretty` plus println's newline.
func RenderSummaryJSON(s DigestSnapshot, m SummaryMeta) []byte {
	k := countKinds(s.Signals)

	p0 := make(map[string]any, len(s.P0Alerts))
	for tool, ids := range s.P0Alerts {
		sessions := sortedUniqueSessions(ids)
		arr := make([]any, 0, len(sessions))
		for _, id := range sessions {
			arr = append(arr, id)
		}
		p0[tool] = arr
	}

	issues := make([]any, 0, len(m.CreatedIssues))
	for _, is := range m.CreatedIssues {
		var url any
		if is.URL != "" {
			url = is.URL
		}
		issues = append(issues, map[string]any{
			"id": is.ID, "backend": is.Backend, "title": is.Title, "url": url,
		})
	}

	var digestPath any
	if m.DigestPath != nil {
		digestPath = *m.DigestPath
	}

	var spend any
	if sp := s.Spend; sp != nil {
		var total any
		if sp.Total != nil {
			total = sp.Total.String()
		}
		spend = map[string]any{
			"total_usd":           total,
			"sessions_with_stats": jsonInt(sp.SessionsWithStats),
			"sessions_with_cost":  jsonInt(sp.SessionsWithCost),
			"input_tokens":        jsonUint(sp.InputTokens),
			"output_tokens":       jsonUint(sp.OutputTokens),
			"role_costs_usd":      usdMap(sp.RoleCosts),
			"model_costs_usd":     usdMap(sp.ModelCosts),
		}
	}

	personas := map[string]any{}
	for _, entry := range DisplayKeyedPersonas(s.Personas) {
		pc := entry.Counts
		var channel, cost any
		if entry.PersonaKey.Channel != "" {
			channel = entry.PersonaKey.Channel
		}
		if pc.CostUSD != nil {
			cost = pc.CostUSD.String()
		}
		personas[entry.Key] = map[string]any{
			"persona": entry.PersonaKey.Persona, "channel": channel,
			"sessions": jsonInt(pc.Sessions), "corrections": jsonInt(pc.Corrections),
			"errors": jsonInt(pc.Errors), "workarounds": jsonInt(pc.Workarounds),
			"deferrals": jsonInt(pc.Deferrals), "patterns": jsonInt(pc.Patterns),
			"input_tokens": jsonUint(pc.InputTokens), "output_tokens": jsonUint(pc.OutputTokens),
			"cost_usd": cost,
		}
	}

	value := map[string]any{
		"schema_version":   jsonInt(SummarySchemaVersion),
		"sessions_scanned": jsonInt(s.SessionsScanned),
		"tracker_failures": jsonInt(m.TrackerFailures),
		"corrections":      jsonInt(k.Corrections),
		"errors":           jsonInt(k.Errors),
		"workarounds":      jsonInt(k.Workarounds),
		"deferrals":        jsonInt(k.Deferrals),
		"patterns":         jsonInt(k.Patterns),
		"frustrations":     jsonInt(k.Frustrations),  // D36
		"interruptions":    jsonInt(k.Interruptions), // D36
		"p0_alerts":        p0,
		"personas":         personas,
		"spend":            spend,
		"digest_path":      digestPath,
		"created_issues":   issues,
	}
	if a := s.ArchiveSpend; a != nil {
		period := func(ps PeriodSpend) map[string]any {
			return map[string]any{
				"total_usd":  ps.Total.String(),
				"days":       jsonInt(ps.Days),
				"agents_usd": usdMap(ps.Agents),
				"models_usd": usdMap(ps.Models),
			}
		}
		var yesterday any
		if a.Yesterday != nil {
			yesterday = period(*a.Yesterday)
		}
		value["archive_spend"] = map[string]any{
			"yesterday": yesterday,
			"week":      period(a.Week),
			"week_from": a.WeekFrom,
			"week_to":   a.WeekTo,
			"timezone":  a.Timezone,
		}
	}
	out, err := serdejson.Pretty(value)
	if err != nil {
		// Every value above is a serdejson-supported type; an error here is
		// a programming bug, never a data condition.
		panic(fmt.Sprintf("friction: summary JSON: %v", err))
	}
	return append(out, '\n')
}

// HumanSummary ports the non-JSON `review nightly` lines
// (commands/review.rs:148-195). The Spend and Archive lines print raw
// decimals, not FormatUSD: that is jilog's output and is kept.
func HumanSummary(s DigestSnapshot, m SummaryMeta) string {
	k := countKinds(s.Signals)
	var b strings.Builder
	fmt.Fprintf(&b, "%d corrections, %d errors, %d workarounds, %d deferrals, %d patterns, %d P0 alert(s), %d session(s) scanned",
		k.Corrections, k.Errors, k.Workarounds, k.Deferrals, k.Patterns, len(s.P0Alerts), s.SessionsScanned)
	if k.Frustrations > 0 || k.Interruptions > 0 {
		fmt.Fprintf(&b, ", %d frustrations, %d interruptions", k.Frustrations, k.Interruptions)
	}
	b.WriteByte('\n')
	if sp := s.Spend; sp != nil && sp.Total != nil {
		fmt.Fprintf(&b, "Spend: $%s across %d of %d session(s) with usage data\n",
			sp.Total.String(), sp.SessionsWithCost, sp.SessionsWithStats)
	}
	if a := s.ArchiveSpend; a != nil {
		y := "n/a"
		if a.Yesterday != nil {
			y = "$" + a.Yesterday.Total.String()
		}
		fmt.Fprintf(&b, "Archive spend: %s yesterday, $%s trailing 7d (%d day(s))\n",
			y, a.Week.Total.String(), a.Week.Days)
	}
	if m.DigestPath != nil {
		fmt.Fprintf(&b, "Digest: %s\n", *m.DigestPath)
	}
	if len(m.CreatedIssues) > 0 {
		fmt.Fprintf(&b, "Created %d issue(s)\n", len(m.CreatedIssues))
	}
	if m.TrackerFailures > 0 {
		fmt.Fprintf(&b, "Tracker failures: %d (affected sessions retry next run)\n", m.TrackerFailures)
	}
	return b.String()
}
