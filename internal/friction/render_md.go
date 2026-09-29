package friction

import (
	"bytes"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// IssueRef is a Kata issue a digest line links to. ID is "#<short_id>",
// Backend "kata"; URL may be empty.
type IssueRef struct{ ID, Backend, URL, Title string }

// DigestSnapshot is everything a digest renders, frozen at build time
// (spec §5.4 snapshot_json). Signals keep detector run order; the
// renderer groups them by kind without sorting (digest.rs:540).
type DigestSnapshot struct {
	Date, Timezone, RulesVersion string
	Signals                      []Signal
	P0Alerts                     map[string][]string
	Personas                     map[PersonaKey]*PersonaCounts
	Spend                        *SpendSummary
	ArchiveSpend                 *ArchiveSpend
	SessionsScanned              int
	RecurrenceCosts              map[string]USD // by fingerprint
}

// RenderLinks carries the current Kata linkage; it changes after the
// build, so it is kept out of the snapshot (spec §8.6).
type RenderLinks struct {
	IssueIndex map[string]IssueRef // by fingerprint
}

// RenderMarkdown ports render_digest (digest.rs:723-1031), with the D8 heading
// and D36 frustration/interruption additions.
func RenderMarkdown(s DigestSnapshot, l RenderLinks) []byte {
	var corrections, errs, workarounds, deferrals, frustrations, interruptions, patterns []Signal
	for _, sig := range s.Signals {
		switch sig.Kind {
		case KindCorrection:
			corrections = append(corrections, sig)
		case KindError:
			errs = append(errs, sig)
		case KindWorkaround:
			workarounds = append(workarounds, sig)
		case KindDeferral:
			deferrals = append(deferrals, sig)
		case KindFrustration:
			frustrations = append(frustrations, sig)
		case KindInterruption:
			interruptions = append(interruptions, sig)
		case KindPattern:
			patterns = append(patterns, sig)
		}
	}
	total := len(corrections) + len(errs) + len(workarounds) + len(deferrals) +
		len(frustrations) + len(interruptions) + len(patterns)

	var b bytes.Buffer
	b.WriteString("---\n")
	fmt.Fprintf(&b, "date: %s\n", s.Date)
	fmt.Fprintf(&b, "signals_captured: %d\n", total)
	fmt.Fprintf(&b, "p0_count: %d\n", len(s.P0Alerts))
	fmt.Fprintf(&b, "corrections: %d\n", len(corrections))
	fmt.Fprintf(&b, "errors: %d\n", len(errs))
	fmt.Fprintf(&b, "workarounds: %d\n", len(workarounds))
	fmt.Fprintf(&b, "deferrals: %d\n", len(deferrals))
	fmt.Fprintf(&b, "patterns: %d\n", len(patterns))
	// D36: always present, after jilog's keys.
	fmt.Fprintf(&b, "frustrations: %d\n", len(frustrations))
	fmt.Fprintf(&b, "interruptions: %d\n", len(interruptions))
	b.WriteString("---\n\n")
	fmt.Fprintf(&b, "# Friction Log — %s\n\n", s.Date)

	b.WriteString("## P0 Alerts\n\n")
	if len(s.P0Alerts) == 0 {
		b.WriteString("_No P0 alerts._\n\n")
	} else {
		for _, tool := range sortedKeys(s.P0Alerts) {
			sessions := sortedUniqueSessions(s.P0Alerts[tool])
			fmt.Fprintf(&b, "- **P0 ALERT**: `%s` failed in %d distinct sessions: %s\n",
				tool, len(sessions), strings.Join(sessions, ", "))
		}
		b.WriteByte('\n')
	}

	section(&b, "Corrections", "_No corrections detected._", corrections, func(sig Signal) string {
		return fmt.Sprintf("- %s`%s` — %s%s\n",
			dimsPrefix(sig.Dims), sig.SubjectID, PythonRepr(sig.Text), lineAnnotations(sig, s, l))
	})
	section(&b, "Errors", "_No errors detected._", errs, func(sig Signal) string {
		return fmt.Sprintf("- %s`%s` / `%s`: %s%s\n",
			dimsPrefix(sig.Dims), sig.SubjectID, sig.ToolName,
			TruncateWithMarker(sig.Text, MaxErrorMessageLength), lineAnnotations(sig, s, l))
	})
	section(&b, "Workarounds", "_No workarounds detected._", workarounds, func(sig Signal) string {
		return fmt.Sprintf("- %s`%s` pattern=`%s`: %s%s\n",
			dimsPrefix(sig.Dims), sig.SubjectID, sig.Label, PythonRepr(sig.Text), lineAnnotations(sig, s, l))
	})
	section(&b, "Deferrals", "_No deferrals detected._", deferrals, func(sig Signal) string {
		return fmt.Sprintf("- %s`%s` pattern=`%s`\n", dimsPrefix(sig.Dims), sig.SubjectID, sig.Label)
	})
	section(&b, "Patterns", "_No patterns detected._", patterns, func(sig Signal) string {
		return fmt.Sprintf("- %s`%s` kind=`%s`: %s%s\n",
			dimsPrefix(sig.Dims), sig.SubjectID, sig.Label, sig.Evidence, lineAnnotations(sig, s, l))
	})
	// D36 sections: always rendered, after Patterns and before Personas.
	section(&b, "Frustration", "_No frustration detected._", frustrations, func(sig Signal) string {
		return fmt.Sprintf("- %s`%s` — %s%s\n",
			dimsPrefix(sig.Dims), sig.SubjectID, PythonRepr(sig.Text), lineAnnotations(sig, s, l))
	})
	b.WriteString("## Interruptions\n\n")
	if groups := groupInterruptions(interruptions); len(groups) == 0 {
		b.WriteString("_No interruptions detected._\n\n")
	} else {
		for _, g := range groups {
			fmt.Fprintf(&b, "- %s`%s` interruptions=%d\n", dimsPrefix(g.first.Dims), g.first.SubjectID, g.n)
		}
		b.WriteByte('\n')
	}

	renderPersonas(&b, s.Personas)

	if s.Spend != nil || s.ArchiveSpend != nil {
		b.WriteString("## Spend\n\n")
	}
	if sp := s.Spend; sp != nil {
		if sp.Total != nil {
			fmt.Fprintf(&b, "- **Total**: %s across %d of %d session(s) with usage data\n",
				FormatUSD(*sp.Total), sp.SessionsWithCost, sp.SessionsWithStats)
		} else {
			fmt.Fprintf(&b, "- **Total**: no cost data (%d session(s) with usage; unpriced models)\n",
				sp.SessionsWithStats)
		}
		fmt.Fprintf(&b, "- **Tokens**: %d in / %d out\n", sp.InputTokens, sp.OutputTokens)
		b.WriteByte('\n')
		costTable(&b, "### Spend by role\n\n", sp.RoleCosts)
		costTable(&b, "### Spend by model\n\n", sp.ModelCosts)
	}
	if a := s.ArchiveSpend; a != nil {
		renderArchiveSpend(&b, a)
	}
	return b.Bytes()
}

func section(b *bytes.Buffer, title, placeholder string, sigs []Signal, line func(Signal) string) {
	b.WriteString("## " + title + "\n\n")
	if len(sigs) == 0 {
		b.WriteString(placeholder + "\n\n")
		return
	}
	for _, sig := range sigs {
		b.WriteString(line(sig))
	}
	b.WriteByte('\n')
}

type interruptionGroup struct {
	first Signal // dims and subject come from the session's first signal
	n     int
}

// groupInterruptions folds interruption signals into one entry per
// subject, in order of first appearance (run order).
func groupInterruptions(sigs []Signal) []interruptionGroup {
	groups := make([]interruptionGroup, 0)
	index := map[string]int{}
	for _, sig := range sigs {
		if i, ok := index[sig.SubjectID]; ok {
			groups[i].n++
			continue
		}
		index[sig.SubjectID] = len(groups)
		groups = append(groups, interruptionGroup{first: sig, n: 1})
	}
	return groups
}

func costTable(b *bytes.Buffer, header string, costs map[string]USD) {
	if len(costs) == 0 {
		return
	}
	b.WriteString(header)
	for _, k := range sortedKeys(costs) {
		fmt.Fprintf(b, "- `%s`: %s\n", SanitizeDisplay(k), FormatUSD(costs[k]))
	}
	b.WriteByte('\n')
}

func renderPersonas(b *bytes.Buffer, personas map[PersonaKey]*PersonaCounts) {
	if len(personas) == 0 {
		return
	}
	b.WriteString("## Personas\n\n")
	for _, e := range DisplayKeyedPersonas(personas) {
		c := e.Counts
		usage := ""
		if c.hasUsage() {
			usage = fmt.Sprintf(" — %d in / %d out tokens", c.InputTokens, c.OutputTokens)
			if c.CostUSD != nil {
				usage += ", " + FormatUSD(*c.CostUSD)
			}
		}
		if c.signalTotal() == 0 {
			fmt.Fprintf(b, "- `%s`: no signals (%d session(s))%s\n", e.Key, c.Sessions, usage)
			continue
		}
		fmt.Fprintf(b, "- `%s`: %d corrections, %d errors, %d workarounds, %d deferrals, %d patterns (%d session(s))%s\n",
			e.Key, c.Corrections, c.Errors, c.Workarounds, c.Deferrals, c.Patterns, c.Sessions, usage)
	}
	b.WriteByte('\n')
}

// renderArchiveSpend ports render_archive_spend (digest.rs:975-1024).
func renderArchiveSpend(b *bytes.Buffer, a *ArchiveSpend) {
	agents := func(p PeriodSpend) string {
		ranked := p.AgentsByCost()
		if len(ranked) == 0 {
			return ""
		}
		parts := make([]string, 0, len(ranked))
		for _, r := range ranked {
			parts = append(parts, SanitizeDisplay(r.Name)+" "+FormatUSD(r.USD))
		}
		return " — " + strings.Join(parts, ", ")
	}
	b.WriteString("### Archive spend (agentsview)\n\n")
	if y := a.Yesterday; y != nil {
		fmt.Fprintf(b, "- **Yesterday (%s)**: %s%s\n", a.WeekTo, FormatUSD(y.Total), agents(*y))
	} else {
		fmt.Fprintf(b, "- **Yesterday (%s)**: no archive rows\n", a.WeekTo)
	}
	fmt.Fprintf(b, "- **Trailing 7d (%s – %s, days in %s)**: %s across %d day(s)%s\n",
		a.WeekFrom, a.WeekTo, SanitizeDisplay(a.Timezone),
		FormatUSD(a.Week.Total), a.Week.Days, agents(a.Week))
	models := a.Week.TopModels(5)
	if len(models) > 0 {
		parts := make([]string, 0, len(models))
		for _, m := range models {
			parts = append(parts, "`"+SanitizeDisplay(m.Name)+"` "+FormatUSD(m.USD))
		}
		fmt.Fprintf(b, "- **Top models (7d)**: %s\n", strings.Join(parts, ", "))
	}
	b.WriteByte('\n')
}

// dimsPrefix ports dims_prefix (digest.rs:1064-1085). An empty field is
// jilog's None.
func dimsPrefix(d Dims) string {
	var b strings.Builder
	if d.Persona != "" {
		b.WriteString("`" + PersonaDisplayKey(PersonaKey{Persona: d.Persona, Channel: d.Channel}) + "` ")
	}
	if d.Seat != "" {
		b.WriteString("`seat:" + SanitizeDisplay(d.Seat) + "` ")
	}
	if d.Agent != "" {
		b.WriteString("`agent:" + SanitizeDisplay(d.Agent) + "` ")
	}
	if d.Machine != "" {
		b.WriteString("`machine:" + SanitizeDisplay(d.Machine) + "` ")
	}
	return b.String()
}

// lineAnnotations ports line_annotations/issue_annotation
// (digest.rs:1087-1117), keyed by fingerprint.
func lineAnnotations(sig Signal, s DigestSnapshot, l RenderLinks) string {
	if len(l.IssueIndex) == 0 && len(s.RecurrenceCosts) == 0 {
		return ""
	}
	fp := sig.Fingerprint()
	out := ""
	if ref, ok := l.IssueIndex[fp]; ok {
		out = " (→ " + ref.Backend + "#" + strings.TrimLeft(ref.ID, "#") + ")"
	}
	if c, ok := s.RecurrenceCosts[fp]; ok {
		out += " (recurred in sessions totaling " + FormatUSD(c) + ")"
	}
	return out
}

func sortedUniqueSessions(ids []string) []string {
	out := append([]string(nil), ids...)
	sort.Strings(out)
	return slices.Compact(out)
}

// kindCounts holds per-kind signal counts.
type kindCounts struct {
	Corrections, Errors, Workarounds, Deferrals, Patterns int
	Frustrations, Interruptions                           int // D36
}

func countKinds(sigs []Signal) kindCounts {
	var k kindCounts
	for _, s := range sigs {
		switch s.Kind {
		case KindCorrection:
			k.Corrections++
		case KindError:
			k.Errors++
		case KindWorkaround:
			k.Workarounds++
		case KindDeferral:
			k.Deferrals++
		case KindPattern:
			k.Patterns++
		case KindFrustration:
			k.Frustrations++
		case KindInterruption:
			k.Interruptions++
		}
	}
	return k
}
