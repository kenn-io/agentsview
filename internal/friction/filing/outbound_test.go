// internal/friction/filing/outbound_test.go
package filing

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/friction"
)

func TestIdempotencyKey(t *testing.T) {
	tests := []struct {
		name  string
		title string
		check func(t *testing.T, key string)
	}{
		{name: "idempotency_key_is_deterministic", title: "[friction/correction] sess-abc: foo bar baz", check: func(t *testing.T, key string) {
			assert.Equal(t, key, IdempotencyKey("[friction/correction] sess-abc: foo bar baz"))
		}},
		{name: "idempotency_key_strips_whitespace", title: "[friction/correction] sess-abc: foo\tbar\u00a0baz\u2003qux", check: func(t *testing.T, key string) {
			assert.Equal(t, "[friction/correction]-sess-abc:-foo-bar-baz-qux", key)
		}},
		{name: "idempotency_key_clamps_length", title: strings.Repeat("x", 1000), check: func(t *testing.T, key string) {
			assert.Equal(t, 240, utf8.RuneCountInString(key))
		}},
		{name: "idempotency_key_handles_unicode", title: "[friction/error] 失敗: 茶の湯 — em-dash and 日本語 " + strings.Repeat("語", 300), check: func(t *testing.T, key string) {
			assert.True(t, utf8.ValidString(key))
			assert.Equal(t, 240, utf8.RuneCountInString(key))
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { tt.check(t, IdempotencyKey(tt.title)) })
	}
}

func TestPriorityAndLabels(t *testing.T) {
	// signal_priority_all_variants, plus the two agentsview kinds (spec §6.8).
	tests := []struct {
		kind     friction.Kind
		priority int
	}{
		{friction.KindError, 3}, {friction.KindCorrection, 2}, {friction.KindPattern, 2},
		{friction.KindWorkaround, 3}, {friction.KindDeferral, 3},
		{friction.KindFrustration, 2}, {friction.KindInterruption, 3},
	}
	for _, tt := range tests {
		t.Run("signal_priority_all_variants/"+string(tt.kind), func(t *testing.T) {
			assert.Equal(t, tt.priority, Priority(tt.kind))
			assert.Equal(t, []string{"friction", "friction:" + string(tt.kind)}, Labels(tt.kind))
		})
	}
	assert.Equal(t, "friction:recurred", LabelRecurred)
}

func TestForceNew(t *testing.T) {
	// identified_worker_errors_bypass_fuzzy_gate_but_keep_exact_dedup, in its
	// generic form: only diagnostic subjects bypass Kata's look-alike gate.
	tests := []struct {
		name string
		sig  friction.Signal
		want bool
	}{
		{name: "diagnostic_error", sig: friction.Signal{Kind: friction.KindError, SubjectKind: friction.SubjectDiagnostic, SubjectID: "ci:build-17", ToolName: "ci"}, want: true},
		{name: "session_error", sig: friction.Signal{Kind: friction.KindError, SubjectKind: friction.SubjectSession, SubjectID: "claude:abc", ToolName: "Bash"}},
		{name: "session_correction", sig: friction.Signal{Kind: friction.KindCorrection, SubjectKind: friction.SubjectSession}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { assert.Equal(t, tt.want, ForceNew(tt.sig)) })
	}
}

func run(date string) RunContext {
	return RunContext{Date: date, PublicURL: "https://av.example.test", DigestURL: DigestURL("https://av.example.test", date)}
}

func TestBody(t *testing.T) {
	ord := 7
	tests := []struct {
		name     string
		sig      friction.Signal
		run      RunContext
		contains []string
		absent   []string
	}{
		{name: "build_body_correction_contains_context",
			sig: friction.Signal{Kind: friction.KindCorrection, SubjectKind: friction.SubjectSession, SubjectID: "sess-abc", Text: "no, use the other cli for calendar", Ordinal: &ord},
			run: run("2026-05-11"),
			contains: []string{"Detected by agentsview Friction Log on 2026-05-11.\n\n## Source\n- Session: sess-abc\n- Kind: correction\n",
				"- Link: https://av.example.test/sessions/sess-abc?msg=7\n", "- Digest: https://av.example.test/friction/2026-05-11\n",
				"\n\n## Signal\nno, use the other cli for calendar"}},
		{name: "build_body_error_contains_tool_and_message",
			sig: friction.Signal{Kind: friction.KindError, SubjectKind: friction.SubjectSession, SubjectID: "sess-def", ToolName: "bash", Text: "command not found: fzf"},
			run: run("2026-05-11"), contains: []string{"Tool: bash\nMessage: command not found: fzf", "- Kind: error\n"}},
		{name: "build_body_workaround_contains_pattern_and_context",
			sig: friction.Signal{Kind: friction.KindWorkaround, SubjectKind: friction.SubjectSession, SubjectID: "sess-ghi", Label: "for now", Text: "temporarily using osascript"},
			run: run("2026-05-11"), contains: []string{"Pattern: for now\nContext: temporarily using osascript", "- Kind: workaround\n"}},
		{name: "build_body_pattern_contains_description",
			sig: friction.Signal{Kind: friction.KindPattern, SubjectKind: friction.SubjectSession, SubjectID: "sess-jkl", Text: "always asks for confirmation before deleting"},
			run: run("2026-05-11"), contains: []string{"## Signal\nalways asks for confirmation before deleting", "- Kind: pattern\n"}, absent: []string{"?msg="}},
		{name: "build_body_deferral_contains_item",
			sig: friction.Signal{Kind: friction.KindDeferral, SubjectKind: friction.SubjectSession, SubjectID: "sess-mno", Label: "set up the CI pipeline"},
			run: run("2026-05-11"), contains: []string{"## Signal\nset up the CI pipeline", "- Kind: deferral\n"}},
		{name: "frustration_contains_text",
			sig: friction.Signal{Kind: friction.KindFrustration, SubjectKind: friction.SubjectSession, SubjectID: "s", Text: "this is broken again"},
			run: run("2026-05-11"), contains: []string{"## Signal\nthis is broken again"}},
		{name: "interruption_names_the_message",
			sig: friction.Signal{Kind: friction.KindInterruption, SubjectKind: friction.SubjectSession, SubjectID: "s", Ordinal: &ord},
			run: run("2026-05-11"), contains: []string{"## Signal\nUser interrupted the agent at message 7."}},
		{name: "build_body_uses_threaded_digest_path_when_present_becomes_no_url_lines_without_public_url",
			sig: friction.Signal{Kind: friction.KindCorrection, SubjectKind: friction.SubjectSession, SubjectID: "sess-abc", Text: "ctx", Ordinal: &ord},
			run: RunContext{Date: "2026-08-26"}, absent: []string{"- Link:", "- Digest:", "~/.amplifier", "learning-digest"}},
		{name: "diagnostic_has_no_session_link",
			sig: friction.Signal{Kind: friction.KindError, SubjectKind: friction.SubjectDiagnostic, SubjectID: "ci:build-17", ToolName: "ci", Text: "ci: build 17 failed"},
			run: run("2026-05-11"), contains: []string{"- Digest: "}, absent: []string{"- Link:"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := Body(tt.sig, tt.run)
			for _, c := range tt.contains {
				assert.Contains(t, body, c)
			}
			for _, a := range tt.absent {
				assert.NotContains(t, body, a)
			}
		})
	}
}

func TestBodyDimsLines(t *testing.T) {
	// body_carries_agent_and_machine_lines_only_when_set and
	// issue_body_includes_seat_without_multiline_injection.
	base := friction.Signal{Kind: friction.KindCorrection, SubjectKind: friction.SubjectSession, SubjectID: "claude:53c9fd03", Text: "no, the other one"}
	body := Body(base, RunContext{Date: "2026-09-16"})
	assert.NotContains(t, body, "- Agent:")
	assert.NotContains(t, body, "- Machine:")
	assert.NotContains(t, body, "- Seat:")

	withDims := base
	withDims.Dims = friction.Dims{Seat: "seat-01\nextra", Agent: "claude", Machine: "laptop-a\r\nx"}
	body = Body(withDims, RunContext{Date: "2026-09-16"})
	assert.Contains(t, body, "- Kind: correction\n- Seat: seat-01 extra\n- Agent: claude\n- Machine: laptop-a  x\n\n## Signal\n")
	assert.NotContains(t, body, "\nextra")
}

func TestBodyRedactsAndScrubsPaths(t *testing.T) {
	fixtureKey := "AKIA" + "IOSFODNN7EXAMPLE"
	macHome := strings.Join([]string{"", "Users", "example"}, "/")
	linuxHome := strings.Join([]string{"", "home", "example"}, "/")
	windowsHome := strings.Join([]string{"C:", "Users", "example"}, `\`)
	sig := friction.Signal{Kind: friction.KindError, SubjectKind: friction.SubjectSession, SubjectID: "s", ToolName: "Bash",
		Text: "export AWS_KEY=" + fixtureKey + " failed in " + macHome + "/src/app/main.go and " + linuxHome + "/.config/x and " + windowsHome + `\proj`}
	out := DefaultRedact(Body(sig, RunContext{Date: "2026-09-21"}))
	assert.NotContains(t, out, fixtureKey)
	assert.Contains(t, out, "…MPLE")
	assert.NotContains(t, out, macHome)
	assert.NotContains(t, out, linuxHome)
	assert.NotContains(t, out, windowsHome)
	assert.Contains(t, out, "~/src/app/main.go")
	assert.Contains(t, out, "~/.config/x")
}

func TestURLs(t *testing.T) {
	ord := 3
	tests := []struct {
		name string
		got  string
		want string
	}{
		{name: "session_with_agent_prefix", got: SessionURL("https://av.example.test/", "claude:abc-1", &ord), want: "https://av.example.test/sessions/claude/abc-1?msg=3"},
		{name: "session_plain", got: SessionURL("https://av.example.test/base", "abc", nil), want: "https://av.example.test/base/sessions/abc"},
		{name: "session_no_public_url", got: SessionURL("", "abc", &ord), want: ""},
		{name: "session_bad_scheme", got: SessionURL("ftp://x", "abc", nil), want: ""},
		{name: "session_strips_userinfo", got: SessionURL("https://u:p@av.example.test", "abc", nil), want: "https://av.example.test/sessions/abc"},
		{name: "digest", got: DigestURL("https://av.example.test/", "2026-09-21"), want: "https://av.example.test/friction/2026-09-21"},
		{name: "digest_no_public_url", got: DigestURL("", "2026-09-21"), want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { assert.Equal(t, tt.want, tt.got) })
	}
}

func TestMetadata(t *testing.T) {
	ord := 2
	sig := friction.Signal{Kind: friction.KindError, SubjectKind: friction.SubjectSession, SubjectID: "claude:abc", ToolName: "Bash", Text: "boom", Ordinal: &ord}
	got := Metadata(sig, run("2026-09-21"), "inst-1")
	assert.Equal(t, map[string]any{
		"friction.fingerprint":   sig.Fingerprint(),
		"friction.kind":          "error",
		"friction.rules_version": friction.RulesVersion,
		"agentsview.session_id":  "claude:abc",
		"agentsview.session_url": "https://av.example.test/sessions/claude/abc?msg=2",
		"agentsview.instance":    "inst-1",
	}, got)
	noURL := Metadata(sig, RunContext{Date: "2026-09-21"}, "inst-1")
	assert.NotContains(t, noURL, "agentsview.session_url")
}

func TestBackoff(t *testing.T) {
	tests := []struct {
		attempts int
		base     time.Duration
	}{{1, time.Hour}, {2, 2 * time.Hour}, {3, 4 * time.Hour}, {5, 16 * time.Hour}, {6, 24 * time.Hour}, {40, 24 * time.Hour}}
	for _, tt := range tests {
		t.Run(tt.base.String(), func(t *testing.T) {
			got := Backoff(tt.attempts, "fl1:abc")
			require.GreaterOrEqual(t, got, tt.base)
			assert.Less(t, got, tt.base+tt.base/10+time.Nanosecond)
			assert.Equal(t, got, Backoff(tt.attempts, "fl1:abc"), "jitter is deterministic per fingerprint")
		})
	}
}
