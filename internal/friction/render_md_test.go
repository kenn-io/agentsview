package friction

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRenderMarkdownKeepsArchivedTextLiteral(t *testing.T) {
	const hostile = "value\n`"
	const literal = "`value '`"
	for _, tt := range []struct {
		name   string
		signal Signal
		p0     map[string][]string
		want   string
	}{
		{"correction", Signal{Kind: KindCorrection, SubjectID: "s1", Text: hostile}, nil, "`s1` — " + literal},
		{"error tool", Signal{Kind: KindError, SubjectID: "s1", ToolName: hostile, Text: "error"}, nil, "`s1` / " + literal + ": `error`"},
		{"workaround label", Signal{Kind: KindWorkaround, SubjectID: "s1", Label: hostile, Text: "text"}, nil, "`s1` pattern=" + literal + ": `text`"},
		{"deferral label", Signal{Kind: KindDeferral, SubjectID: "s1", Label: hostile}, nil, "`s1` pattern=" + literal},
		{"pattern evidence", Signal{Kind: KindPattern, SubjectID: "s1", Label: "retry_loop", Evidence: hostile}, nil, "`s1` kind=`retry_loop`: " + literal},
		{"pattern label", Signal{Kind: KindPattern, SubjectID: "s1", Label: hostile, Evidence: "evidence"}, nil, "`s1` kind=" + literal + ": `evidence`"},
		{"interruption subject", Signal{Kind: KindInterruption, SubjectID: hostile}, nil, literal + " interruptions=1"},
		{"seat", Signal{Kind: KindInterruption, SubjectID: "s1", Dims: Dims{Seat: hostile}}, nil, "`seat:value '` `s1` interruptions=1"},
		{"P0 tool", Signal{}, map[string][]string{hostile: {"s1", "s2", "s3"}}, "**P0 ALERT**: " + literal + " failed in 3 distinct sessions: `s1`, `s2`, `s3`"},
		{"P0 sessions", Signal{}, map[string][]string{"Read": {hostile, hostile + "2", hostile + "3"}}, "**P0 ALERT**: `Read` failed in 3 distinct sessions: " + literal + ", `value '2`, `value '3`"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			body := string(RenderMarkdown(DigestSnapshot{
				Date: "2026-09-16", Signals: []Signal{tt.signal}, P0Alerts: tt.p0,
			}))
			assert.Contains(t, body, "- "+tt.want+"\n")
		})
	}
}
