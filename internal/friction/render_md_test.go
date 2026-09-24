package friction

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func render(s DigestSnapshot, l RenderLinks) string { return string(RenderMarkdown(s, l)) }

func snap(date string, sigs ...Signal) DigestSnapshot {
	return DigestSnapshot{Date: date, Signals: sigs}
}

func lineWith(t *testing.T, body, needle string) string {
	t.Helper()
	for l := range strings.SplitSeq(body, "\n") {
		if strings.Contains(l, needle) {
			return l
		}
	}
	require.FailNowf(t, "line not found", "%q in:\n%s", needle, body)
	return ""
}

// Ports every render test in digest.rs:1266-1376 and :1790-1985 that
// calls render_digest directly.
func TestRenderMarkdown(t *testing.T) {
	t.Run("digest_frontmatter_has_counts", func(t *testing.T) {
		body := render(snap("2026-04-30", Signal{Kind: KindCorrection, SubjectID: "a", Text: "fix it"}), RenderLinks{})
		assert.True(t, strings.HasPrefix(body, "---\n"))
		for _, want := range []string{"date: 2026-04-30", "signals_captured: 1", "corrections: 1", "errors: 0", "deferrals: 0", "frustrations: 0", "interruptions: 0"} {
			assert.Contains(t, body, want)
		}
	})
	t.Run("digest_empty_sections_use_placeholder", func(t *testing.T) {
		body := render(snap("2026-04-30"), RenderLinks{})
		for _, want := range []string{
			"_No P0 alerts._", "_No corrections detected._", "_No errors detected._",
			"_No workarounds detected._", "_No deferrals detected._", "_No patterns detected._",
			"_No frustration detected._", "_No interruptions detected._",
		} {
			assert.Contains(t, body, want)
		}
	})
	t.Run("heading_is_friction_log", func(t *testing.T) {
		body := render(snap("2026-04-30"), RenderLinks{})
		assert.Contains(t, body, "---\n\n# Friction Log — 2026-04-30\n\n## P0 Alerts\n\n")
		assert.NotContains(t, body, "Learning Digest")
	})
	t.Run("digest_p0_includes_session_list", func(t *testing.T) {
		s := snap("2026-04-30")
		s.P0Alerts = map[string][]string{"bash": {"aaa", "bbb", "ccc"}}
		body := render(s, RenderLinks{})
		assert.Contains(t, body, "`bash` failed in 3 distinct sessions")
		assert.Contains(t, body, "aaa, bbb, ccc")
		assert.Contains(t, body, "p0_count: 1\n")
	})
	t.Run("digest_corrections_use_python_repr", func(t *testing.T) {
		body := render(snap("2026-04-30", Signal{Kind: KindCorrection, SubjectID: "abc", Text: "don't do that"}), RenderLinks{})
		assert.Contains(t, body, `'don\'t do that'`)
	})
	t.Run("digest_errors_truncated_with_marker", func(t *testing.T) {
		body := render(snap("2026-04-30", Signal{Kind: KindError, SubjectID: "s1", ToolName: "bash", Text: strings.Repeat("x", 600)}), RenderLinks{})
		assert.Contains(t, body, "[truncated]")
		line := lineWith(t, body, "`s1` / `bash`: ")
		assert.Equal(t, "- `s1` / `bash`: "+strings.Repeat("x", 500)+" … [truncated]", line)
	})
	t.Run("errors_truncate_runes_not_bytes_and_skip_repr", func(t *testing.T) {
		msg := strings.Repeat("é", 600)
		body := render(snap("2026-04-30", Signal{Kind: KindError, SubjectID: "s1", ToolName: "bash", Text: msg}), RenderLinks{})
		line := lineWith(t, body, "`s1` / `bash`: ")
		assert.Equal(t, "- `s1` / `bash`: "+strings.Repeat("é", 500)+" … [truncated]", line)
	})
	t.Run("digest_patterns_render_kind_and_evidence", func(t *testing.T) {
		body := render(snap("2026-07-05", Signal{
			Kind: KindPattern, SubjectID: "s1", Label: "compaction_storm",
			Text:     "compaction storm: 4 compactions within 10 minutes",
			Evidence: "4 compactions 09:01-09:08",
		}), RenderLinks{})
		assert.Contains(t, body, "signals_captured: 1")
		assert.Contains(t, body, "patterns: 1")
		assert.Contains(t, body, "## Patterns")
		assert.Contains(t, body, "- `s1` kind=`compaction_storm`: 4 compactions 09:01-09:08")
	})
	t.Run("digest_patterns_annotated_when_issue_filed", func(t *testing.T) {
		p := Signal{
			Kind: KindPattern, SubjectID: "s1", Label: "stuck_loop",
			Text:     "stuck loop: `bash` called 5 times with identical arguments",
			Evidence: "`bash` x5 identical arguments 09:00-09:04",
		}
		idx := map[string]IssueRef{p.Fingerprint(): {ID: "#9", Backend: "kata", Title: p.Title()}}
		body := render(snap("2026-07-05", p), RenderLinks{IssueIndex: idx})
		assert.Contains(t, body, "(→ kata#9)")
	})
	t.Run("digest_deferrals_render_item", func(t *testing.T) {
		body := render(snap("2026-04-30", Signal{Kind: KindDeferral, SubjectID: "s1", Label: "next session"}), RenderLinks{})
		assert.Contains(t, body, "signals_captured: 1")
		assert.Contains(t, body, "- `s1` pattern=`next session`")
	})
	t.Run("workaround_line_format", func(t *testing.T) {
		body := render(snap("2026-04-30", Signal{Kind: KindWorkaround, SubjectID: "s1", Label: "for now", Text: "Using it for now\nthen"}), RenderLinks{})
		assert.Contains(t, body, "- `s1` pattern=`for now`: 'Using it for now\\nthen'\n")
	})
	t.Run("digest_spend_section_renders_totals_roles_models", func(t *testing.T) {
		s := snap("2026-07-05")
		s.Spend = &SpendSummary{
			Total:             new(mustUSD(t, "4.2")),
			SessionsWithStats: 3, SessionsWithCost: 2,
			InputTokens: 12345, OutputTokens: 678,
			RoleCosts:  map[string]USD{"(root)": mustUSD(t, "3.1"), "explore": mustUSD(t, "1.1")},
			ModelCosts: map[string]USD{"claude-opus-4-8": mustUSD(t, "4.2")},
		}
		body := render(s, RenderLinks{})
		for _, want := range []string{
			"## Spend",
			"- **Total**: $4.20 across 2 of 3 session(s) with usage data",
			"- **Tokens**: 12345 in / 678 out",
			"### Spend by role",
			"- `(root)`: $3.10",
			"- `explore`: $1.10",
			"### Spend by model",
			"- `claude-opus-4-8`: $4.20",
		} {
			assert.Contains(t, body, want)
		}
	})
	t.Run("digest_spend_section_absent_without_stats", func(t *testing.T) {
		assert.NotContains(t, render(snap("2026-07-05"), RenderLinks{}), "## Spend")
	})
	t.Run("digest_spend_all_unpriced_says_no_cost_data", func(t *testing.T) {
		s := snap("2026-07-05")
		s.Spend = &SpendSummary{SessionsWithStats: 2, InputTokens: 10, OutputTokens: 1}
		body := render(s, RenderLinks{})
		assert.Contains(t, body, "- **Total**: no cost data (2 session(s) with usage; unpriced models)")
		assert.NotContains(t, body, "### Spend by role", "no costs → no role table")
	})
	t.Run("digest_dims_sanitize_hostile_channel_names", func(t *testing.T) {
		c := Signal{
			Kind: KindCorrection, SubjectID: "s1", Text: "no, wrong channel entirely",
			Dims: Dims{Persona: "helper", Channel: "general`\ninjected"},
		}
		s := snap("2026-07-13", c)
		s.Personas = map[PersonaKey]*PersonaCounts{{"helper", "general`\ninjected"}: {Sessions: 1, Corrections: 1}}
		body := render(s, RenderLinks{})
		assert.Contains(t, body, "- `helper@general' injected` `s1` — ")
		assert.Contains(t, body, "- `helper@general' injected`: 1 corrections")
		assert.NotContains(t, body, "general`", "raw backtick leaked into digest")
	})
	t.Run("digest_personas_section_absent_for_coding_only_runs", func(t *testing.T) {
		assert.NotContains(t, render(snap("2026-07-13"), RenderLinks{}), "## Personas")
	})
	t.Run("personas_rollup_carries_tokens_and_optional_cost", func(t *testing.T) {
		s := snap("2026-08-26")
		s.Personas = map[PersonaKey]*PersonaCounts{
			{"helper", "ops"}:   {Sessions: 1, Corrections: 1, InputTokens: 12000, OutputTokens: 300},
			{"reviewer", ""}:    {Sessions: 1, InputTokens: 800, OutputTokens: 40, CostUSD: new(mustUSD(t, "1.5"))},
			{"quiet", "lounge"}: {Sessions: 2},
		}
		body := render(s, RenderLinks{})
		assert.Contains(t, body, "## Personas\n\n"+
			"- `helper@ops`: 1 corrections, 0 errors, 0 workarounds, 0 deferrals, 0 patterns (1 session(s)) — 12000 in / 300 out tokens\n"+
			"- `quiet@lounge`: no signals (2 session(s))\n"+
			"- `reviewer`: no signals (1 session(s)) — 800 in / 40 out tokens, $1.50\n\n")
	})
	t.Run("digest_annotation_appended_to_correction_bullet", func(t *testing.T) {
		c := Signal{Kind: KindCorrection, SubjectID: "0e91a2b4", Text: "no, use the gh cli for calendar"}
		idx := map[string]IssueRef{c.Fingerprint(): {ID: "#7", Backend: "kata", Title: c.Title()}}
		body := render(snap("2026-05-11", c), RenderLinks{IssueIndex: idx})
		assert.Contains(t, body, "(→ kata#7)")
		assert.NotContains(t, body, "kata##")
	})
	t.Run("digest_no_annotation_when_issue_index_empty", func(t *testing.T) {
		body := render(snap("2026-05-11", Signal{Kind: KindCorrection, SubjectID: "abc", Text: "fix it"}), RenderLinks{})
		assert.NotContains(t, body, "(→")
	})
	t.Run("digest_annotation_byte_stable_for_unaffected_lines", func(t *testing.T) {
		ann := Signal{Kind: KindCorrection, SubjectID: "ann", Text: "do this"}
		plain := Signal{Kind: KindCorrection, SubjectID: "pla", Text: "plain line"}
		idx := map[string]IssueRef{ann.Fingerprint(): {ID: "#3", Backend: "kata", Title: ann.Title()}}
		with := render(snap("2026-05-11", ann, plain), RenderLinks{IssueIndex: idx})
		without := render(snap("2026-05-11", ann, plain), RenderLinks{})
		assert.Equal(t, lineWith(t, without, "plain line"), lineWith(t, with, "plain line"), "unaffected line changed")
		assert.NotEqual(t, lineWith(t, without, "do this"), lineWith(t, with, "do this"))
		assert.True(t, strings.HasSuffix(lineWith(t, with, "do this"), "(→ kata#3)"))
	})
	t.Run("both_annotations_in_order", func(t *testing.T) {
		c := Signal{Kind: KindCorrection, SubjectID: "s", Text: "no, other branch"}
		s := snap("2026-05-11", c)
		s.RecurrenceCosts = map[string]USD{c.Fingerprint(): mustUSD(t, "4.2")}
		idx := map[string]IssueRef{c.Fingerprint(): {ID: "7", Backend: "kata"}}
		body := render(s, RenderLinks{IssueIndex: idx})
		assert.Contains(t, body, "- `s` — 'no, other branch' (→ kata#7) (recurred in sessions totaling $4.20)\n")
	})
	t.Run("seat_prefix_preserves_fleet_dimensions_and_sanitizes", func(t *testing.T) {
		assert.Equal(t, "`bot@chat` `seat:seat'  01` ",
			dimsPrefix(Dims{Persona: "bot", Channel: "chat", Seat: "seat`\t\n01"}))
		assert.Equal(t, "`agent:cowork` `machine:mac'1 ` ",
			dimsPrefix(Dims{Agent: "cowork", Machine: "mac`1\n"}))
		assert.Empty(t, dimsPrefix(Dims{Channel: "orphan"}), "channel renders only with a persona")
	})
	t.Run("agent_and_machine_tags_are_stamped_and_rendered", func(t *testing.T) {
		c := Signal{
			Kind: KindCorrection, SubjectID: "a37ffc87-2799-4a09-830b-a92fde71d768",
			Text: "no, use the other path", Dims: Dims{Agent: "claude", Machine: "mac-01"},
		}
		body := render(snap("2026-09-16", c), RenderLinks{})
		assert.Contains(t, body, "- `agent:claude` `machine:mac-01` `a37ffc87-2799-4a09-830b-a92fde71d768` — 'no, use the other path'")
	})
	t.Run("digest_without_archive_spend_is_unchanged", func(t *testing.T) {
		body := render(snap("2026-09-16"), RenderLinks{})
		assert.NotContains(t, body, "## Spend")
		// D36 moves jilog's end-of-file rule to the Interruptions placeholder.
		assert.True(t, strings.HasSuffix(body, "## Interruptions\n\n_No interruptions detected._\n\n"))
	})
}

// sampleArchiveSpend ports digest.rs sample_archive_spend (:2506-2535),
// with the zone scrubbed to Asia/Dhaka.
func sampleArchiveSpend(t *testing.T) *ArchiveSpend {
	t.Helper()
	d := func(s string) USD { return mustUSD(t, s) }
	week := PeriodSpend{
		Total: d("2101.5"), Days: 7,
		Agents: map[string]USD{"codex": d("1300.25"), "claude": d("800.25"), "cowork": d("1")},
		Models: map[string]USD{
			"gpt-6-astra": d("900"), "claude-opus-5": d("700.5"), "gpt-5.6-sol": d("400"),
			"claude-haiku-4-5-20251001": d("60"), "m5": d("30"), "m6": d("11"),
		},
	}
	yesterday := PeriodSpend{
		Total: d("332.138392"), Days: 1,
		Agents: map[string]USD{"codex": d("224.55406"), "claude": d("104.943456"), "cowork": d("2.640876")},
	}
	return &ArchiveSpend{
		Yesterday: &yesterday, Week: week,
		WeekFrom: "2026-09-09", WeekTo: "2026-09-15", Timezone: "Asia/Dhaka",
	}
}

func TestRenderMarkdownArchiveSpend(t *testing.T) {
	t.Run("digest_archive_spend_block_renders_after_observed_spend", func(t *testing.T) {
		s := snap("2026-09-16")
		s.ArchiveSpend = sampleArchiveSpend(t)
		expected := "## Spend\n\n### Archive spend (agentsview)\n\n" +
			"- **Yesterday (2026-09-15)**: $332.138392 — codex $224.55406, claude $104.943456, cowork $2.640876\n" +
			"- **Trailing 7d (2026-09-09 – 2026-09-15, days in Asia/Dhaka)**: $2101.50 across 7 day(s) — codex $1300.25, claude $800.25, cowork $1.00\n" +
			"- **Top models (7d)**: `gpt-6-astra` $900.00, `claude-opus-5` $700.50, `gpt-5.6-sol` $400.00, `claude-haiku-4-5-20251001` $60.00, `m5` $30.00\n\n"
		assert.True(t, strings.HasSuffix(render(s, RenderLinks{}), expected), render(s, RenderLinks{}))
	})
	t.Run("observed_block_comes_first", func(t *testing.T) {
		s := snap("2026-09-16")
		s.ArchiveSpend = sampleArchiveSpend(t)
		s.Spend = &SpendSummary{SessionsWithStats: 2, InputTokens: 10, OutputTokens: 5}
		assert.Contains(t, render(s, RenderLinks{}),
			"## Spend\n\n- **Total**: no cost data (2 session(s) with usage; unpriced models)\n- **Tokens**: 10 in / 5 out\n\n### Archive spend (agentsview)\n")
	})
	t.Run("yesterday_absent_and_no_agent_rows", func(t *testing.T) {
		s := snap("2026-09-16")
		a := sampleArchiveSpend(t)
		a.Yesterday = nil
		a.Week.Agents = nil
		s.ArchiveSpend = a
		body := render(s, RenderLinks{})
		assert.Contains(t, body, "- **Yesterday (2026-09-15)**: no archive rows\n")
		assert.Contains(t, body, "- **Trailing 7d (2026-09-09 – 2026-09-15, days in Asia/Dhaka)**: $2101.50 across 7 day(s)\n")
	})
	t.Run("no_models_omits_top_models_line", func(t *testing.T) {
		s := snap("2026-09-16")
		a := sampleArchiveSpend(t)
		a.Week.Models = nil
		s.ArchiveSpend = a
		assert.NotContains(t, render(s, RenderLinks{}), "Top models")
	})
}

// Review Focus 1: archive agent, model and zone names are sanitized.
func TestRenderMarkdownArchiveNamesSanitized(t *testing.T) {
	s := snap("2026-09-16")
	s.ArchiveSpend = &ArchiveSpend{
		Week: PeriodSpend{
			Total: mustUSD(t, "1"), Days: 1,
			Agents: map[string]USD{"ag`\nent": mustUSD(t, "1")},
			Models: map[string]USD{"mo`del\n## Injected": mustUSD(t, "1")},
		},
		WeekFrom: "2026-09-09", WeekTo: "2026-09-15", Timezone: "Bad`\nZone",
	}
	body := render(s, RenderLinks{})
	assert.Contains(t, body, "days in Bad' Zone)")
	assert.Contains(t, body, " — ag' ent $1.00\n")
	assert.Contains(t, body, "`mo'del ## Injected` $1.00")
	assert.NotContains(t, body, "\n## Injected")
}

// Review Focus 2: Go map order never leaks into the bytes.
func TestRenderIsDeterministic(t *testing.T) {
	s := snap("2026-09-16")
	s.P0Alerts = map[string][]string{"zsh": {"c", "a", "b"}, "bash": {"b", "a", "c"}, "mode": {"x", "y", "z"}}
	s.Personas = map[PersonaKey]*PersonaCounts{}
	for _, p := range []string{"e", "d", "c", "b", "a"} {
		s.Personas[PersonaKey{p, "ch"}] = &PersonaCounts{Sessions: 1}
	}
	s.Spend = &SpendSummary{
		Total: new(mustUSD(t, "5")), SessionsWithStats: 5, SessionsWithCost: 5,
		RoleCosts:  map[string]USD{"(root)": mustUSD(t, "1"), "subagent": mustUSD(t, "2"), "b": mustUSD(t, "2")},
		ModelCosts: map[string]USD{"m1": mustUSD(t, "1"), "m2": mustUSD(t, "1"), "m3": mustUSD(t, "3")},
	}
	s.ArchiveSpend = sampleArchiveSpend(t)
	first := RenderMarkdown(s, RenderLinks{})
	for range 50 {
		require.Equal(t, string(first), string(RenderMarkdown(s, RenderLinks{})))
	}
	assert.Contains(t, string(first), "- **P0 ALERT**: `bash` failed in 3 distinct sessions: a, b, c\n- **P0 ALERT**: `mode`")
}

// Review Focus 3: unsorted and duplicated P0 session ids.
func TestRenderMarkdownP0SortsAndDedupes(t *testing.T) {
	s := snap("2026-09-16")
	input := []string{"s3", "s1", "s1"}
	s.P0Alerts = map[string][]string{"bash": input}
	body := render(s, RenderLinks{})
	assert.Contains(t, body, "- **P0 ALERT**: `bash` failed in 2 distinct sessions: s1, s3\n")
	assert.Equal(t, []string{"s3", "s1", "s1"}, input, "renderer must not mutate the snapshot")
}

// D36 (spec §6.8, §9.1): frustration and interruption kinds. jilog has
// no test for these; this pins their bytes. The file-level golden is
// friction-log-extra-kinds.md (Task 7).
func TestRenderMarkdownFrustrationAndInterruptions(t *testing.T) {
	f := Signal{Kind: KindFrustration, SubjectID: "s1", Text: "this is broken, same error again!!!", Dims: Dims{Agent: "claude"}}
	c := Signal{Kind: KindCorrection, SubjectID: "s3", Text: "no, use the other branch"}
	p := Signal{Kind: KindPattern, SubjectID: "s4", Label: "retry_loop", Evidence: "`bash` x3 identical arguments 01:00-01:02"}
	intr := func(subject string, d Dims) Signal {
		return Signal{Kind: KindInterruption, SubjectID: subject, Dims: d}
	}

	t.Run("sections_and_frontmatter_bytes", func(t *testing.T) {
		s := snap("2026-09-16", c, p, f,
			intr("s2", Dims{}), intr("s5", Dims{Seat: "seat-02"}), intr("s2", Dims{}))
		s.RecurrenceCosts = map[string]USD{f.Fingerprint(): mustUSD(t, "1.5")}
		idx := map[string]IssueRef{f.Fingerprint(): {ID: "#11", Backend: "kata"}}
		want := "---\n" +
			"date: 2026-09-16\n" +
			"signals_captured: 6\n" +
			"p0_count: 0\n" +
			"corrections: 1\n" +
			"errors: 0\n" +
			"workarounds: 0\n" +
			"deferrals: 0\n" +
			"patterns: 1\n" +
			"frustrations: 1\n" +
			"interruptions: 3\n" +
			"---\n\n" +
			"# Friction Log — 2026-09-16\n\n" +
			"## P0 Alerts\n\n_No P0 alerts._\n\n" +
			"## Corrections\n\n- `s3` — 'no, use the other branch'\n\n" +
			"## Errors\n\n_No errors detected._\n\n" +
			"## Workarounds\n\n_No workarounds detected._\n\n" +
			"## Deferrals\n\n_No deferrals detected._\n\n" +
			"## Patterns\n\n- `s4` kind=`retry_loop`: `bash` x3 identical arguments 01:00-01:02\n\n" +
			"## Frustration\n\n- `agent:claude` `s1` — 'this is broken, same error again!!!' (→ kata#11) (recurred in sessions totaling $1.50)\n\n" +
			"## Interruptions\n\n- `s2` interruptions=2\n- `seat:seat-02` `s5` interruptions=1\n\n"
		assert.Equal(t, want, render(s, RenderLinks{IssueIndex: idx}))
	})
	t.Run("empty_kinds_render_keys_and_placeholders", func(t *testing.T) {
		body := render(snap("2026-09-16", c), RenderLinks{})
		assert.Contains(t, body, "deferrals: 0\npatterns: 0\nfrustrations: 0\ninterruptions: 0\n---\n")
		assert.Contains(t, body, "## Patterns\n\n_No patterns detected._\n\n"+
			"## Frustration\n\n_No frustration detected._\n\n"+
			"## Interruptions\n\n_No interruptions detected._\n\n")
	})
	t.Run("new_sections_precede_personas", func(t *testing.T) {
		s := snap("2026-09-16")
		s.Personas = map[PersonaKey]*PersonaCounts{{"helper", "general"}: {Sessions: 1}}
		assert.Contains(t, render(s, RenderLinks{}),
			"_No interruptions detected._\n\n## Personas\n\n- `helper@general`: no signals (1 session(s))\n\n")
	})
	t.Run("interruptions_group_by_session_in_run_order", func(t *testing.T) {
		s := snap("2026-09-16", intr("b", Dims{}), intr("a", Dims{}), intr("b", Dims{Agent: "later"}), intr("a", Dims{}))
		body := render(s, RenderLinks{})
		assert.Contains(t, body, "## Interruptions\n\n- `b` interruptions=2\n- `a` interruptions=2\n\n",
			"first-appearance order; dims from the first signal")
		assert.Contains(t, body, "interruptions: 4\n")
	})
	t.Run("interruptions_never_annotated", func(t *testing.T) {
		i := intr("s2", Dims{})
		s := snap("2026-09-16", i)
		s.RecurrenceCosts = map[string]USD{i.Fingerprint(): mustUSD(t, "2")}
		idx := map[string]IssueRef{i.Fingerprint(): {ID: "#12", Backend: "kata"}}
		body := render(s, RenderLinks{IssueIndex: idx})
		assert.Contains(t, body, "- `s2` interruptions=1\n")
		assert.NotContains(t, body, "(→")
		assert.NotContains(t, body, "recurred in sessions")
	})
	t.Run("frustration_repr_escapes", func(t *testing.T) {
		g := Signal{Kind: KindFrustration, SubjectID: "s9", Text: "why won't it\nload???"}
		assert.Contains(t, render(snap("2026-09-16", g), RenderLinks{}), "- `s9` — 'why won\\'t it\\nload???'\n")
	})
	t.Run("persona_rollup_ignores_new_kinds", func(t *testing.T) {
		fp := f
		fp.Dims = Dims{Persona: "helper", Channel: "general"}
		s := snap("2026-09-16", fp, intr("s1", fp.Dims))
		pc := &PersonaCounts{Sessions: 1}
		pc.AddSignals(s.Signals)
		s.Personas = map[PersonaKey]*PersonaCounts{{"helper", "general"}: pc}
		assert.Contains(t, render(s, RenderLinks{}), "- `helper@general`: no signals (1 session(s))\n")
	})
}

// Review Focus 4: deferrals never carry annotations.
func TestRenderMarkdownNeverAnnotatesDeferrals(t *testing.T) {
	d := Signal{Kind: KindDeferral, SubjectID: "s1", Label: "next session"}
	s := snap("2026-09-16", d)
	s.RecurrenceCosts = map[string]USD{d.Fingerprint(): mustUSD(t, "4.2")}
	idx := map[string]IssueRef{d.Fingerprint(): {ID: "#5", Backend: "kata"}}
	body := render(s, RenderLinks{IssueIndex: idx})
	assert.Contains(t, body, "- `s1` pattern=`next session`\n")
	assert.NotContains(t, body, "(→")
	assert.NotContains(t, body, "recurred in sessions")
}
