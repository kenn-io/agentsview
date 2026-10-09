package friction

import (
	"bytes"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// DigestSnapshot holds one day's findings in detector order.
type DigestSnapshot struct {
	Date            string
	Signals         []Signal
	P0Alerts        map[string][]string
	SessionsScanned int
}

// RenderMarkdown renders the Friction Log with signals grouped by kind.
func RenderMarkdown(s DigestSnapshot) []byte {
	groups := groupByKind(s.Signals)
	corrections, errs := groups[KindCorrection], groups[KindError]
	workarounds, deferrals := groups[KindWorkaround], groups[KindDeferral]
	frustrations, interruptions := groups[KindFrustration], groups[KindInterruption]
	patterns := groups[KindPattern]
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
	fmt.Fprintf(&b, "frustrations: %d\n", len(frustrations))
	fmt.Fprintf(&b, "interruptions: %d\n", len(interruptions))
	b.WriteString("---\n\n")
	fmt.Fprintf(&b, "# Friction Log — %s\n\n", s.Date)

	b.WriteString("## P0 Alerts\n\n")
	if len(s.P0Alerts) == 0 {
		b.WriteString("_No P0 alerts._\n\n")
	} else {
		for _, tool := range slices.Sorted(maps.Keys(s.P0Alerts)) {
			sessions := make([]string, 0, len(s.P0Alerts[tool]))
			for _, id := range s.P0Alerts[tool] {
				sessions = append(sessions, code(id))
			}
			fmt.Fprintf(&b, "- **P0 ALERT**: %s failed in %d distinct sessions: %s\n",
				code(tool), len(sessions), strings.Join(sessions, ", "))
		}
		b.WriteByte('\n')
	}

	section(&b, "Corrections", "_No corrections detected._", corrections, func(sig Signal) string {
		return fmt.Sprintf("- %s%s — %s\n",
			dimsPrefix(sig.Dims), code(sig.SubjectID), code(sig.Text))
	})
	section(&b, "Errors", "_No errors detected._", errs, func(sig Signal) string {
		return fmt.Sprintf("- %s%s / %s: %s\n",
			dimsPrefix(sig.Dims), code(sig.SubjectID), code(sig.ToolName),
			code(TruncateWithMarker(sig.Text, MaxErrorMessageLength)))
	})
	section(&b, "Workarounds", "_No workarounds detected._", workarounds, func(sig Signal) string {
		return fmt.Sprintf("- %s%s pattern=%s: %s\n",
			dimsPrefix(sig.Dims), code(sig.SubjectID), code(sig.Label), code(sig.Text))
	})
	section(&b, "Deferrals", "_No deferrals detected._", deferrals, func(sig Signal) string {
		return fmt.Sprintf("- %s%s pattern=%s\n", dimsPrefix(sig.Dims), code(sig.SubjectID), code(sig.Label))
	})
	section(&b, "Patterns", "_No patterns detected._", patterns, func(sig Signal) string {
		return fmt.Sprintf("- %s%s kind=%s: %s\n",
			dimsPrefix(sig.Dims), code(sig.SubjectID), code(sig.Label), code(sig.Evidence))
	})
	section(&b, "Frustration", "_No frustration detected._", frustrations, func(sig Signal) string {
		return fmt.Sprintf("- %s%s — %s\n",
			dimsPrefix(sig.Dims), code(sig.SubjectID), code(sig.Text))
	})
	b.WriteString("## Interruptions\n\n")
	if groups := groupInterruptions(interruptions); len(groups) == 0 {
		b.WriteString("_No interruptions detected._\n\n")
	} else {
		for _, g := range groups {
			fmt.Fprintf(&b, "- %s%s interruptions=%d\n", dimsPrefix(g.first.Dims), code(g.first.SubjectID), g.n)
		}
		b.WriteByte('\n')
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

// dimsPrefix ports dims_prefix (digest.rs:1064-1085). An empty field is
// jilog's None.
func dimsPrefix(d Dims) string {
	var b strings.Builder
	if d.Seat != "" {
		b.WriteString(code("seat:"+d.Seat) + " ")
	}
	if d.Agent != "" {
		b.WriteString(code("agent:"+d.Agent) + " ")
	}
	if d.Machine != "" {
		b.WriteString(code("machine:"+d.Machine) + " ")
	}
	return b.String()
}

func code(s string) string { return "`" + SanitizeDisplay(s) + "`" }

func groupByKind(sigs []Signal) map[Kind][]Signal {
	groups := make(map[Kind][]Signal)
	for _, sig := range sigs {
		groups[sig.Kind] = append(groups[sig.Kind], sig)
	}
	return groups
}
