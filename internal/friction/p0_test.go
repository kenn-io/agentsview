package friction

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func p0Error(subject, tool string) Signal {
	return Signal{Kind: KindError, SubjectID: subject, SubjectKind: SubjectSession, ToolName: tool}
}

// The prefix retains the upstream sub-agent fixture's meaning. Callers use
// their own parent-session relationship to provide this predicate.
func p0FixtureSubAgent(id string) bool { return strings.HasPrefix(id, "0000000000000000") }

// Ports the threshold, deduplication, and sub-agent cases from the upstream
// P0 detector tests.
func TestDetectP0Alerts(t *testing.T) {
	tests := []struct {
		name   string
		errors []Signal
		want   map[string][]string
	}{
		{
			name:   "three distinct subjects sorted",
			errors: []Signal{p0Error("c", "shell"), p0Error("a", "shell"), p0Error("b", "shell")},
			want:   map[string][]string{"shell": {"a", "b", "c"}},
		},
		{
			name:   "two distinct subjects skipped",
			errors: []Signal{p0Error("a", "shell"), p0Error("b", "shell")},
			want:   map[string][]string{},
		},
		{
			name:   "repeated subject counted once",
			errors: []Signal{p0Error("a", "shell"), p0Error("a", "shell"), p0Error("a", "shell")},
			want:   map[string][]string{},
		},
		{
			name: "sub-agent subjects excluded",
			errors: []Signal{
				p0Error("0000000000000000-a", "shell"),
				p0Error("0000000000000000-b", "shell"),
				p0Error("0000000000000000-c", "shell"),
			},
			want: map[string][]string{},
		},
		{
			name: "non-error signals ignored",
			errors: []Signal{
				{Kind: KindCorrection, SubjectID: "a", SubjectKind: SubjectSession, ToolName: "shell"},
				p0Error("b", "shell"), p0Error("c", "shell"),
			},
			want: map[string][]string{},
		},
		{
			name: "tools grouped independently",
			errors: []Signal{
				p0Error("c", "shell"), p0Error("a", "shell"), p0Error("b", "shell"),
				p0Error("b", "reader"), p0Error("a", "reader"),
			},
			want: map[string][]string{"shell": {"a", "b", "c"}},
		},
		{name: "empty input returns empty non-nil map", want: map[string][]string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, DetectP0Alerts(tt.errors, p0FixtureSubAgent))
		})
	}
}

// Four diagnostic subjects never raise P0; the same tool names on session
// subjects do. This checks subject kind rather than a private tool-name list.
func TestWorkerDiagnosticsDoNotRaiseP0(t *testing.T) {
	for _, tool := range []string{"status_check", "review_check", "fallback_check", "hook_check", "shell"} {
		t.Run(tool, func(t *testing.T) {
			diagnostics := []Signal{
				p0Error("diag-a", tool), p0Error("diag-b", tool),
				p0Error("diag-c", tool), p0Error("diag-d", tool),
			}
			for i := range diagnostics {
				diagnostics[i].SubjectKind = SubjectDiagnostic
			}
			assert.Equal(t, map[string][]string{}, DetectP0Alerts(diagnostics, p0FixtureSubAgent))

			sessions := []Signal{
				p0Error("session-d", tool), p0Error("session-b", tool),
				p0Error("session-a", tool), p0Error("session-c", tool),
			}
			assert.Equal(t, map[string][]string{tool: {"session-a", "session-b", "session-c", "session-d"}},
				DetectP0Alerts(sessions, p0FixtureSubAgent))
		})
	}
}

func TestDetectP0AlertsMixedSubjectsAndNilPredicate(t *testing.T) {
	errors := []Signal{
		p0Error("0000000000000000-a", "shell"),
		p0Error("b", "shell"),
		p0Error("c", "shell"),
		{Kind: KindError, SubjectID: "diag", SubjectKind: SubjectDiagnostic, ToolName: "shell"},
	}
	assert.Equal(t, map[string][]string{}, DetectP0Alerts(errors, p0FixtureSubAgent))
	assert.Equal(t, map[string][]string{"shell": {"0000000000000000-a", "b", "c"}},
		DetectP0Alerts(errors, nil))
}
