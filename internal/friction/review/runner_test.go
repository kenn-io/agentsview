package review_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/friction/review"
	"go.kenn.io/agentsview/internal/money"
)

var reviewNow = time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)

type subject struct {
	id, ended, machine, agent string
	parent                    string
	dims                      *db.FrictionSessionDims
	findings                  []db.FrictionFinding
	rules                     string
	usage                     []db.UsageEvent
}

func seed(t *testing.T, d *db.DB, subjects ...subject) {
	t.Helper()
	for _, s := range subjects {
		ended := s.ended
		machine, agent := s.machine, s.agent
		if machine == "" {
			machine = "machine-a"
		}
		if agent == "" {
			agent = "claude"
		}
		dbtest.SeedSession(t, d, s.id, "proj", func(sess *db.Session) {
			sess.EndedAt = &ended
			sess.Machine = machine
			sess.Agent = agent
			if s.parent != "" {
				sess.ParentSessionID = dbtest.Ptr(s.parent)
				sess.RelationshipType = "subagent"
			}
		})
		rules := s.rules
		if rules == "" {
			rules = friction.RulesVersion
		}
		dims := s.dims
		if dims != nil {
			dims.SessionID = s.id
		}
		require.NoError(t, d.ReplaceSessionFriction(t.Context(), s.id, s.findings, dims, rules, "h-"+s.id))
		if len(s.usage) > 0 {
			require.NoError(t, d.ReplaceSessionUsageEvents(t.Context(), s.id, s.usage))
		}
	}
}

func finding(sid string, kind friction.Kind, detector, text, label, tool string, ord, seq int) db.FrictionFinding {
	sig := friction.Signal{
		Kind: kind, SubjectID: sid, Detector: detector, Text: text,
		Label: label, ToolName: tool, Ordinal: dbtest.Ptr(ord), Seq: seq,
	}
	return db.FrictionFinding{
		SessionID: sid, Kind: string(kind), Detector: detector,
		MessageOrdinal: dbtest.Ptr(ord), ToolName: tool, Label: label, Text: text,
		Title: sig.Title(), Fingerprint: sig.Fingerprint(), Seq: seq, RulesVersion: friction.RulesVersion,
	}
}

func correction(sid, text string) db.FrictionFinding {
	return finding(sid, friction.KindCorrection, "correction.coding", text, "", "", 1, 0)
}

func tokens(in, out int, cost string) []db.UsageEvent {
	e := db.UsageEvent{
		Source: "shutdown", Model: "unpriced-test-model", InputTokens: in,
		OutputTokens: out, OccurredAt: "2026-09-15T01:00:00Z", DedupKey: "k",
	}
	if cost != "" {
		c := money.MustParseDollars(cost)
		e.Cost, e.CostStatus = &c, "exact"
	}
	return []db.UsageEvent{e}
}

func newRunner(d *db.DB) *review.Runner {
	return &review.Runner{Store: d, Loc: time.UTC, Now: func() time.Time { return reviewNow }, BackfillDays: 7}
}

func rowCount(t *testing.T, d *db.DB, table string) int {
	t.Helper()
	raw, err := sql.Open("sqlite3", d.Path())
	require.NoError(t, err)
	defer raw.Close()
	var n int
	require.NoError(t, raw.QueryRowContext(t.Context(), "SELECT count(*) FROM "+table).Scan(&n))
	return n
}

func stored(t *testing.T, d *db.DB, date string) *db.FrictionDigest {
	t.Helper()
	got, err := d.GetFrictionDigest(t.Context(), date)
	require.NoError(t, err)
	return got
}

func TestBuildDate(t *testing.T) {
	const day = "2026-09-15"
	tests := []struct {
		name string
		run  func(t *testing.T, d *db.DB, r *review.Runner)
	}{
		{name: "run_review_writes_digest_when_file_absent_even_with_no_sessions", run: func(t *testing.T, d *db.DB, r *review.Runner) {
			rep, err := r.BuildDate(t.Context(), day, review.BuildOptions{})
			require.NoError(t, err)
			assert.True(t, rep.Written)
			assert.Equal(t, 0, rep.Snapshot.SessionsScanned)
			got := stored(t, d, day)
			require.NotNil(t, got, "first build of a date writes even an empty digest")
			assert.Contains(t, string(got.Markdown), "signals_captured: 0")
			assert.True(t, strings.HasSuffix(string(got.Markdown), "## Interruptions\n\n_No interruptions detected._\n\n"),
				"with no spend the digest ends with the Interruptions placeholder (spec §9.1)")
			assert.Equal(t, 1, got.Revision)
			assert.NotEmpty(t, got.RunID)
		}},
		{name: "run_review_preserves_existing_digest_when_nothing_scanned", run: func(t *testing.T, d *db.DB, r *review.Runner) {
			seeded := db.FrictionDigest{
				Date: day, Timezone: "UTC", RulesVersion: friction.RulesVersion,
				BuiltAt: reviewNow, Revision: 1, SnapshotJSON: []byte(`{"date":"` + day + `"}`),
				SummaryJSON: []byte("{}\n"), Markdown: []byte("SEEDED-FROM-EARLIER-RUN\n"),
				MarkdownSHA256: "x", RunID: "r",
			}
			require.NoError(t, d.SaveFrictionDigest(t.Context(), seeded, nil, nil))
			seed(t, d, subject{id: "late", ended: "2026-09-15T12:00:00Z", findings: []db.FrictionFinding{correction("late", "no, use the other file please")}})
			rep, err := r.BuildDate(t.Context(), day, review.BuildOptions{})
			require.NoError(t, err)
			assert.False(t, rep.Written, "a plain build of an existing date is a no-op")
			assert.Equal(t, "SEEDED-FROM-EARLIER-RUN\n", string(stored(t, d, day).Markdown))
		}},
		{name: "processed_sessions_persist_across_load", run: func(t *testing.T, d *db.DB, r *review.Runner) {
			seed(t, d, subject{id: "s1", ended: "2026-09-15T12:00:00Z", findings: []db.FrictionFinding{correction("s1", "no, use the other file please")}})
			rep, err := r.BuildDate(t.Context(), day, review.BuildOptions{})
			require.NoError(t, err)
			assert.Equal(t, 1, rep.Snapshot.SessionsScanned)
			// The session is resumed; its last activity moves to the next date.
			seed(t, d, subject{id: "s1", ended: "2026-09-16T12:00:00Z", findings: []db.FrictionFinding{correction("s1", "no, use the other file please")}})
			next, err := r.BuildDate(t.Context(), "2026-09-16", review.BuildOptions{})
			require.NoError(t, err)
			assert.Equal(t, 0, next.Snapshot.SessionsScanned, "a subject enters one digest only")
		}},
		{name: "rebuild_keeps_membership", run: func(t *testing.T, d *db.DB, r *review.Runner) {
			seed(t, d, subject{id: "a", ended: "2026-09-15T10:00:00Z", findings: []db.FrictionFinding{correction("a", "no, use the other file please")}})
			_, err := r.BuildDate(t.Context(), day, review.BuildOptions{})
			require.NoError(t, err)
			seed(t, d,
				subject{id: "a", ended: "2026-09-17T10:00:00Z", findings: []db.FrictionFinding{correction("a", "no, use the other file please")}},
				subject{id: "b", ended: "2026-09-15T11:00:00Z", findings: []db.FrictionFinding{correction("b", "that is wrong, revert it now")}})
			rep, err := r.BuildDate(t.Context(), day, review.BuildOptions{Rebuild: true})
			require.NoError(t, err)
			assert.True(t, rep.Written)
			assert.Equal(t, 2, rep.Snapshot.SessionsScanned)
			got := stored(t, d, day)
			assert.Equal(t, 2, got.Revision)
			assert.Contains(t, string(got.Markdown), "`a`")
			assert.Contains(t, string(got.Markdown), "`b`")
			// Pattern counts were not double-counted for the kept member.
			var occ int
			raw, err := sql.Open("sqlite3", d.Path())
			require.NoError(t, err)
			defer raw.Close()
			fp := correction("a", "no, use the other file please").Fingerprint
			require.NoError(t, raw.QueryRowContext(t.Context(),
				`SELECT occurrence_count FROM friction_patterns WHERE fingerprint = ?`, fp).Scan(&occ))
			assert.Equal(t, 1, occ)
		}},
		{name: "rebuild_does_not_count_new_findings_on_old_member", run: func(t *testing.T, d *db.DB, r *review.Runner) {
			seed(t, d, subject{id: "quiet", ended: "2026-09-15T10:00:00Z"})
			_, err := r.BuildDate(t.Context(), day, review.BuildOptions{})
			require.NoError(t, err)
			assert.Equal(t, 0, rowCount(t, d, "friction_patterns"))
			require.NoError(t, d.ReplaceSessionFriction(t.Context(), "quiet",
				[]db.FrictionFinding{correction("quiet", "no, use the other file please")}, nil,
				friction.RulesVersion, "h-quiet-new"))
			rep, err := r.BuildDate(t.Context(), day, review.BuildOptions{Rebuild: true})
			require.NoError(t, err)
			require.Len(t, rep.Snapshot.Signals, 1)
			assert.Equal(t, 0, rowCount(t, d, "friction_patterns"), "old member's new findings never change recurrence")
		}},
		{name: "dry_run_writes_nothing", run: func(t *testing.T, d *db.DB, r *review.Runner) {
			seed(t, d, subject{id: "s1", ended: "2026-09-15T12:00:00Z", findings: []db.FrictionFinding{correction("s1", "no, use the other file please")}})
			rep, err := r.BuildDate(t.Context(), day, review.BuildOptions{DryRun: true})
			require.NoError(t, err)
			assert.False(t, rep.Written)
			assert.Nil(t, rep.Meta.DigestPath, "digest_path is null on dry run")
			assert.Len(t, rep.Snapshot.Signals, 1)
			assert.Nil(t, stored(t, d, day))
			assert.Equal(t, 0, rowCount(t, d, "friction_digest_sessions"))
			assert.Equal(t, 0, rowCount(t, d, "friction_patterns"))
			real, err := r.BuildDate(t.Context(), day, review.BuildOptions{})
			require.NoError(t, err)
			assert.Equal(t, 1, real.Snapshot.SessionsScanned, "dry run did not consume the session")
		}},
		{name: "run_review_stamps_dims_and_renders_personas_section", run: func(t *testing.T, d *db.DB, r *review.Runner) {
			seed(t, d,
				subject{
					id: "sess-helper", ended: "2026-09-15T01:00:00Z",
					dims:     &db.FrictionSessionDims{Persona: "helper", Channel: "general", DimsSource: "nanoclaw"},
					findings: []db.FrictionFinding{finding("sess-helper", friction.KindCorrection, "correction.chat", "no, use the gh cli for calendar", "", "", 1, 0)},
				},
				subject{
					id: "sess-director", ended: "2026-09-15T02:00:00Z",
					dims: &db.FrictionSessionDims{Persona: "director", Channel: "Event Group", DimsSource: "nanoclaw"},
				},
				subject{
					id: "sess-coding", ended: "2026-09-15T03:00:00Z",
					findings: []db.FrictionFinding{correction("sess-coding", "no, use the gh cli for calendar")},
				})
			rep, err := r.BuildDate(t.Context(), day, review.BuildOptions{})
			require.NoError(t, err)
			var fleet, code friction.Signal
			for _, s := range rep.Snapshot.Signals {
				switch s.SubjectID {
				case "sess-helper":
					fleet = s
				case "sess-coding":
					code = s
				}
			}
			assert.Equal(t, "helper", fleet.Dims.Persona)
			assert.Equal(t, "general", fleet.Dims.Channel)
			assert.Empty(t, code.Dims.Persona)
			require.Len(t, rep.Snapshot.Personas, 2)
			h := rep.Snapshot.Personas[friction.PersonaKey{Persona: "helper", Channel: "general"}]
			assert.Equal(t, [2]int{1, 1}, [2]int{h.Sessions, h.Corrections})
			dir := rep.Snapshot.Personas[friction.PersonaKey{Persona: "director", Channel: "Event Group"}]
			assert.Equal(t, [2]int{1, 0}, [2]int{dir.Sessions, dir.Corrections})
			body := string(stored(t, d, day).Markdown)
			assert.Contains(t, body, "## Personas")
			assert.Contains(t, body, "- `helper@general`: 1 corrections, 0 errors, 0 workarounds, 0 deferrals, 0 patterns (1 session(s))")
			assert.Contains(t, body, "- `director@Event Group`: no signals (1 session(s))")
			assert.Contains(t, body, "- `helper@general` `agent:claude` `machine:machine-a` `sess-helper` — ")
			assert.Contains(t, body, "- `agent:claude` `machine:machine-a` `sess-coding` — ")
		}},
		{name: "personas_rollup_carries_tokens_and_optional_cost", run: func(t *testing.T, d *db.DB, r *review.Runner) {
			seed(t, d,
				subject{
					id: "sess-cell", ended: "2026-09-15T01:00:00Z",
					dims:     &db.FrictionSessionDims{Persona: "helper", Channel: "general", DimsSource: "nanoclaw"},
					findings: []db.FrictionFinding{finding("sess-cell", friction.KindCorrection, "correction.chat", "helper no, answer in the thread", "", "", 1, 0)},
					usage:    tokens(12000, 300, ""),
				},
				subject{
					id: "sess-priced", ended: "2026-09-15T02:00:00Z",
					dims:  &db.FrictionSessionDims{Persona: "concierge", DimsSource: "nanoclaw"},
					usage: tokens(800, 40, "1.5"),
				})
			rep, err := r.BuildDate(t.Context(), day, review.BuildOptions{})
			require.NoError(t, err)
			h := rep.Snapshot.Personas[friction.PersonaKey{Persona: "helper", Channel: "general"}]
			require.NotNil(t, h)
			assert.Equal(t, uint64(12000), h.InputTokens)
			assert.Equal(t, uint64(300), h.OutputTokens)
			assert.Nil(t, h.CostUSD, "unpriced sessions stay tokens-only")
			c := rep.Snapshot.Personas[friction.PersonaKey{Persona: "concierge"}]
			require.NotNil(t, c)
			assert.Equal(t, 1, c.Sessions)
			require.NotNil(t, c.CostUSD)
			assert.Equal(t, "1.500000", c.CostUSD.String())
			body := string(stored(t, d, day).Markdown)
			assert.Contains(t, body, "12000 in / 300 out tokens")
			// Archive costs are microdollars (scale 6); jilog printed $1.50
			// for a transcript-reported "1.5". Documented delta.
			assert.Contains(t, body, "800 in / 40 out tokens, $1.500000")
		}},
		{name: "stats_only_session_counts_toward_spend_and_personas", run: func(t *testing.T, d *db.DB, r *review.Runner) {
			seed(t, d, subject{
				id: "sess-counters", ended: "2026-09-15T01:00:00Z",
				dims: &db.FrictionSessionDims{Persona: "meter", DimsSource: "nanoclaw"}, usage: tokens(500, 25, ""),
			})
			rep, err := r.BuildDate(t.Context(), day, review.BuildOptions{})
			require.NoError(t, err)
			assert.Equal(t, 1, rep.Snapshot.SessionsScanned)
			require.NotNil(t, rep.Snapshot.Spend)
			assert.Equal(t, uint64(500), rep.Snapshot.Spend.InputTokens)
			m := rep.Snapshot.Personas[friction.PersonaKey{Persona: "meter"}]
			require.NotNil(t, m)
			assert.Equal(t, 1, m.Sessions)
			assert.Equal(t, uint64(500), m.InputTokens)
		}},
		{name: "agent_and_machine_tags_are_stamped_and_rendered", run: func(t *testing.T, d *db.DB, r *review.Runner) {
			const id = "a37ffc87-2799-4a09-830b-a92fde71d768"
			seed(t, d, subject{
				id: id, ended: "2026-09-15T01:00:00Z", machine: "laptop-01",
				findings: []db.FrictionFinding{correction(id, "no, use the other path")},
			})
			rep, err := r.BuildDate(t.Context(), day, review.BuildOptions{})
			require.NoError(t, err)
			require.Len(t, rep.Snapshot.Signals, 1)
			assert.Equal(t, "claude", rep.Snapshot.Signals[0].Dims.Agent)
			assert.Equal(t, "laptop-01", rep.Snapshot.Signals[0].Dims.Machine)
			assert.Contains(t, string(stored(t, d, day).Markdown),
				"- `agent:claude` `machine:laptop-01` `"+id+"` — 'no, use the other path'")
		}},
		{name: "subagent_cost_lands_under_subagent_role", run: func(t *testing.T, d *db.DB, r *review.Runner) {
			seed(t, d,
				subject{id: "root", ended: "2026-09-15T01:00:00Z", usage: tokens(10, 1, "3.1")},
				subject{id: "child", ended: "2026-09-15T02:00:00Z", parent: "root", usage: tokens(10, 1, "1.1")})
			rep, err := r.BuildDate(t.Context(), day, review.BuildOptions{})
			require.NoError(t, err)
			require.NotNil(t, rep.Snapshot.Spend)
			assert.Equal(t, "3.100000", rep.Snapshot.Spend.RoleCosts["(root)"].String())
			assert.Equal(t, "1.100000", rep.Snapshot.Spend.RoleCosts["subagent"].String())
		}},
		{name: "p0_alert_needs_three_top_level_subjects", run: func(t *testing.T, d *db.DB, r *review.Runner) {
			errFinding := func(sid string) db.FrictionFinding {
				return finding(sid, friction.KindError, "error", "exit 2: boom", "", "Bash", 2, 0)
			}
			seed(t, d,
				subject{id: "e1", ended: "2026-09-15T01:00:00Z", findings: []db.FrictionFinding{errFinding("e1")}},
				subject{id: "e2", ended: "2026-09-15T02:00:00Z", findings: []db.FrictionFinding{errFinding("e2")}},
				subject{id: "e3", ended: "2026-09-15T03:00:00Z", parent: "e1", findings: []db.FrictionFinding{errFinding("e3")}})
			rep, err := r.BuildDate(t.Context(), day, review.BuildOptions{})
			require.NoError(t, err)
			assert.Empty(t, rep.Snapshot.P0Alerts, "sub-agent subjects never count toward P0")
		}},
		{name: "review_excluded_sessions_are_skipped_and_not_recorded", run: func(t *testing.T, d *db.DB, r *review.Runner) {
			seed(t, d, subject{
				id: "x", ended: "2026-09-15T01:00:00Z",
				dims: &db.FrictionSessionDims{ReviewExcluded: true, DimsSource: "nanoclaw"},
			})
			rep, err := r.BuildDate(t.Context(), day, review.BuildOptions{})
			require.NoError(t, err)
			assert.Equal(t, 0, rep.Snapshot.SessionsScanned)
			assert.Equal(t, 0, rowCount(t, d, "friction_digest_sessions"))
		}},
		{name: "stale_rules_defer_then_skip_after_grace", run: func(t *testing.T, d *db.DB, r *review.Runner) {
			seed(t, d, subject{id: "stale", ended: "2026-09-15T01:00:00Z", rules: "friction-old"})
			r.Now = func() time.Time { return time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC) }
			_, err := r.BuildDate(t.Context(), day, review.BuildOptions{})
			require.ErrorIs(t, err, review.ErrStaleSubjects)
			assert.Nil(t, stored(t, d, day))
			r.Now = func() time.Time { return time.Date(2026, 9, 18, 0, 1, 0, 0, time.UTC) }
			rep, err := r.BuildDate(t.Context(), day, review.BuildOptions{})
			require.NoError(t, err)
			assert.True(t, rep.Written)
			assert.Equal(t, 0, rep.Snapshot.SessionsScanned)
			assert.Equal(t, 0, rowCount(t, d, "friction_digest_sessions"))
		}},
		{name: "today_is_refused_unless_dry_run", run: func(t *testing.T, d *db.DB, r *review.Runner) {
			_, err := r.BuildDate(t.Context(), "2026-09-22", review.BuildOptions{})
			require.ErrorIs(t, err, review.ErrIncompleteDate)
			_, err = r.BuildDate(t.Context(), "2026-09-22", review.BuildOptions{DryRun: true})
			require.NoError(t, err)
			_, err = r.BuildDate(t.Context(), "2026-9-1", review.BuildOptions{})
			require.Error(t, err)
		}},
		{name: "digest_path_uses_public_url", run: func(t *testing.T, d *db.DB, r *review.Runner) {
			r.PublicURL = "https://av.example/"
			rep, err := r.BuildDate(t.Context(), day, review.BuildOptions{})
			require.NoError(t, err)
			require.NotNil(t, rep.Meta.DigestPath)
			assert.Equal(t, "https://av.example/friction/"+day, *rep.Meta.DigestPath)
			assert.Contains(t, string(stored(t, d, day).SummaryJSON), `"digest_path": "https://av.example/friction/`+day+`"`)
			assert.Equal(t, "friction:"+day, review.DigestURL("", day))
		}},
		{name: "frustration_and_interruption_kinds_flow_through", run: func(t *testing.T, d *db.DB, r *review.Runner) {
			seed(t, d, subject{id: "s1", ended: "2026-09-15T12:00:00Z", findings: []db.FrictionFinding{
				correction("s1", "no, use the other file please"),
				finding("s1", friction.KindFrustration, "frustration", "this is still broken", "", "", 3, 1),
				finding("s1", friction.KindInterruption, "interruption", "", "", "", 4, 2),
			}})
			rep, err := r.BuildDate(t.Context(), day, review.BuildOptions{})
			require.NoError(t, err)
			kinds := []friction.Kind{}
			for _, s := range rep.Snapshot.Signals {
				kinds = append(kinds, s.Kind)
				assert.Equal(t, friction.SubjectSession, s.SubjectKind)
			}
			assert.Equal(t, []friction.Kind{friction.KindCorrection, friction.KindFrustration, friction.KindInterruption}, kinds)
			got := stored(t, d, day)
			md, summary := string(got.Markdown), string(got.SummaryJSON)
			assert.Contains(t, md, "frustrations: 1")
			assert.Contains(t, md, "interruptions: 1")
			assert.Contains(t, md, "signals_captured: 3")
			assert.Contains(t, md, "## Frustration")
			assert.Contains(t, md, "- `agent:claude` `machine:machine-a` `s1` interruptions=1")
			assert.Contains(t, summary, `"frustrations": 1`)
			assert.Contains(t, summary, `"interruptions": 1`)
			assert.Contains(t, summary, `"schema_version": 3`)
			assert.Equal(t, 3, rowCount(t, d, "friction_patterns"), "every kind feeds local recurrence")
		}},
		{name: "diagnostics_come_last_are_recorded_and_never_raise_p0", run: func(t *testing.T, d *db.DB, r *review.Runner) {
			seed(t, d, subject{id: "s1", ended: "2026-09-15T12:00:00Z", findings: []db.FrictionFinding{correction("s1", "no, use the other file please")}})
			diag := func(id string) friction.Signal {
				return friction.Signal{
					Kind: friction.KindError, SubjectID: id, Detector: "error",
					ToolName: "nightly_check", Text: id + ": check failed", Dims: friction.Dims{Machine: "ci-runner"},
				}
			}
			r.Diagnostics = fakeDiagnostics{sigs: []friction.Signal{diag("run-9:nightly_check"), diag("run-1:nightly_check"), diag("run-5:nightly_check")}}
			rep, err := r.BuildDate(t.Context(), day, review.BuildOptions{})
			require.NoError(t, err)
			ids := []string{}
			for _, s := range rep.Snapshot.Signals {
				ids = append(ids, s.SubjectID)
			}
			assert.Equal(t, []string{"s1", "run-1:nightly_check", "run-5:nightly_check", "run-9:nightly_check"}, ids)
			assert.Equal(t, friction.SubjectDiagnostic, rep.Snapshot.Signals[1].SubjectKind)
			assert.Empty(t, rep.Snapshot.P0Alerts, "diagnostics never raise P0 (spec §6.5)")
			assert.Equal(t, 4, rep.Snapshot.SessionsScanned)
			assert.Equal(t, 4, stored(t, d, day).SessionsScanned)
			raw, err := sql.Open("sqlite3", d.Path())
			require.NoError(t, err)
			defer raw.Close()
			var n int
			require.NoError(t, raw.QueryRowContext(t.Context(),
				`SELECT count(*) FROM friction_digest_sessions WHERE subject_kind = 'diagnostic'`).Scan(&n))
			assert.Equal(t, 3, n)

			r.Diagnostics = fakeDiagnostics{}
			rebuilt, err := r.BuildDate(t.Context(), day, review.BuildOptions{Rebuild: true})
			require.NoError(t, err)
			assert.Len(t, rebuilt.Snapshot.Signals, 4, "rebuild keeps diagnostic members")
		}},
		{name: "diagnostic_source_error_fails_the_build", run: func(t *testing.T, d *db.DB, r *review.Runner) {
			r.Diagnostics = fakeDiagnostics{err: assert.AnError}
			_, err := r.BuildDate(t.Context(), day, review.BuildOptions{})
			require.ErrorIs(t, err, assert.AnError)
			assert.Nil(t, stored(t, d, day), "no partial digest")
		}},
		{name: "concurrent_builder_loses_cleanly", run: func(t *testing.T, d *db.DB, r *review.Runner) {
			seed(t, d, subject{id: "s1", ended: "2026-09-15T12:00:00Z", findings: []db.FrictionFinding{correction("s1", "no, use the other file please")}})
			racing := &raceStore{Store: d, beforeSave: func() {
				require.NoError(t, d.SaveFrictionDigest(t.Context(), db.FrictionDigest{
					Date: day, Timezone: "UTC",
					RulesVersion: friction.RulesVersion, BuiltAt: reviewNow, Revision: 1,
					SnapshotJSON: []byte("{}"), SummaryJSON: []byte("{}\n"), Markdown: []byte("OTHER\n"),
					MarkdownSHA256: "x", RunID: "other",
				}, nil, nil))
			}}
			r.Store = racing
			rep, err := r.BuildDate(t.Context(), day, review.BuildOptions{})
			require.NoError(t, err)
			assert.False(t, rep.Written)
			assert.Equal(t, "OTHER\n", string(stored(t, d, day).Markdown))
			assert.Equal(t, 0, rowCount(t, d, "friction_patterns"), "losing save rolled back")
		}},
		{name: "archive_spend_failure_hides_block", run: func(t *testing.T, d *db.DB, r *review.Runner) {
			r.Store = &raceStore{Store: d, archiveErr: assert.AnError}
			rep, err := r.BuildDate(t.Context(), day, review.BuildOptions{})
			require.NoError(t, err)
			assert.True(t, rep.Written)
			assert.Nil(t, rep.Snapshot.ArchiveSpend)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := dbtest.OpenTestDB(t)
			tt.run(t, d, newRunner(d))
		})
	}
}

// raceStore wraps a real store to inject a competing writer or a failing
// archive-spend query.
type raceStore struct {
	review.Store
	beforeSave func()
	archiveErr error
}

func (s *raceStore) SaveFrictionDigest(ctx context.Context, d db.FrictionDigest, subj []db.FrictionDigestSubject, p []db.FrictionPatternUpdate) error {
	if s.beforeSave != nil {
		s.beforeSave()
	}
	return s.Store.SaveFrictionDigest(ctx, d, subj, p)
}

func (s *raceStore) FrictionArchiveSpend(ctx context.Context, from, to string, loc *time.Location) (*friction.ArchiveSpend, error) {
	if s.archiveErr != nil {
		return nil, s.archiveErr
	}
	return s.Store.FrictionArchiveSpend(ctx, from, to, loc)
}

type fakeDiagnostics struct {
	sigs []friction.Signal
	err  error
}

func (f fakeDiagnostics) DiagnosticSignalsForDate(context.Context, string, *time.Location) ([]friction.Signal, error) {
	return f.sigs, f.err
}

func TestSignalsKeepSubjectOrderThenSeq(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	seed(t, d,
		subject{id: "b-session", ended: "2026-09-15T01:00:00Z", machine: "m1", findings: []db.FrictionFinding{
			finding("b-session", friction.KindWorkaround, "workaround", "hack two", "hack", "", 5, 1),
			finding("b-session", friction.KindWorkaround, "workaround", "hack one", "hack", "", 2, 0),
		}},
		subject{id: "a-session", ended: "2026-09-15T02:00:00Z", machine: "m2", findings: []db.FrictionFinding{
			finding("a-session", friction.KindWorkaround, "workaround", "for now", "for now", "", 1, 0),
		}})
	rep, err := newRunner(d).BuildDate(t.Context(), "2026-09-15", review.BuildOptions{})
	require.NoError(t, err)
	got := []string{}
	for _, s := range rep.Snapshot.Signals {
		got = append(got, s.Text)
	}
	assert.Equal(t, []string{"hack one", "hack two", "for now"}, got, "(machine, file_path, id) then seq")
	assert.True(t, strings.HasPrefix(string(stored(t, d, "2026-09-15").Markdown), "---\n"))
}
