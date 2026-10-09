package friction

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRenderMarkdownKeepsArchivedTextLiteral(t *testing.T) {
	const hostile = "value\n## Spend\n<h2>x</h2> __init__.py `"
	const literal = "`value ## Spend <h2>x</h2> __init__.py '`"
	for _, tt := range []struct {
		name   string
		signal Signal
		p0     map[string][]string
		want   string
	}{
		{"correction", Signal{Kind: KindCorrection, SubjectID: "s1", Text: hostile}, nil, "`s1` — " + literal},
		{"correction subject", Signal{Kind: KindCorrection, SubjectID: hostile, Text: "text"}, nil, literal + " — `text`"},
		{"error", Signal{Kind: KindError, SubjectID: "s1", ToolName: "Read", Text: hostile}, nil, "`s1` / `Read`: " + literal},
		{"error tool", Signal{Kind: KindError, SubjectID: "s1", ToolName: hostile, Text: "error"}, nil, "`s1` / " + literal + ": `error`"},
		{"error subject", Signal{Kind: KindError, SubjectID: hostile, ToolName: "Read", Text: "error"}, nil, literal + " / `Read`: `error`"},
		{"workaround", Signal{Kind: KindWorkaround, SubjectID: "s1", Label: "for now", Text: hostile}, nil, "`s1` pattern=`for now`: " + literal},
		{"workaround label", Signal{Kind: KindWorkaround, SubjectID: "s1", Label: hostile, Text: "text"}, nil, "`s1` pattern=" + literal + ": `text`"},
		{"workaround subject", Signal{Kind: KindWorkaround, SubjectID: hostile, Label: "for now", Text: "text"}, nil, literal + " pattern=`for now`: `text`"},
		{"deferral label", Signal{Kind: KindDeferral, SubjectID: "s1", Label: hostile}, nil, "`s1` pattern=" + literal},
		{"deferral subject", Signal{Kind: KindDeferral, SubjectID: hostile, Label: "later"}, nil, literal + " pattern=`later`"},
		{"pattern evidence", Signal{Kind: KindPattern, SubjectID: "s1", Label: "retry_loop", Evidence: hostile}, nil, "`s1` kind=`retry_loop`: " + literal},
		{"pattern label", Signal{Kind: KindPattern, SubjectID: "s1", Label: hostile, Evidence: "evidence"}, nil, "`s1` kind=" + literal + ": `evidence`"},
		{"pattern subject", Signal{Kind: KindPattern, SubjectID: hostile, Label: "retry_loop", Evidence: "evidence"}, nil, literal + " kind=`retry_loop`: `evidence`"},
		{"frustration", Signal{Kind: KindFrustration, SubjectID: "s1", Text: hostile}, nil, "`s1` — " + literal},
		{"frustration subject", Signal{Kind: KindFrustration, SubjectID: hostile, Text: "text"}, nil, literal + " — `text`"},
		{"interruption subject", Signal{Kind: KindInterruption, SubjectID: hostile}, nil, literal + " interruptions=1"},
		{"seat", Signal{Kind: KindInterruption, SubjectID: "s1", Dims: Dims{Seat: hostile}}, nil, "`seat:value ## Spend <h2>x</h2> __init__.py '` `s1` interruptions=1"},
		{"agent", Signal{Kind: KindInterruption, SubjectID: "s1", Dims: Dims{Agent: hostile}}, nil, "`agent:value ## Spend <h2>x</h2> __init__.py '` `s1` interruptions=1"},
		{"machine", Signal{Kind: KindInterruption, SubjectID: "s1", Dims: Dims{Machine: hostile}}, nil, "`machine:value ## Spend <h2>x</h2> __init__.py '` `s1` interruptions=1"},
		{"P0 tool", Signal{}, map[string][]string{hostile: {"s1", "s2", "s3"}}, "**P0 ALERT**: " + literal + " failed in 3 distinct sessions: `s1`, `s2`, `s3`"},
		{"P0 sessions", Signal{}, map[string][]string{"Read": {hostile, hostile + "2", hostile + "3"}}, "**P0 ALERT**: `Read` failed in 3 distinct sessions: " + literal + ", `value ## Spend <h2>x</h2> __init__.py '2`, `value ## Spend <h2>x</h2> __init__.py '3`"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			body := string(RenderMarkdown(DigestSnapshot{
				Date: "2026-09-16", Signals: []Signal{tt.signal}, P0Alerts: tt.p0,
			}))
			assert.NotContains(t, body, "\n## Spend")
			assert.Contains(t, body, "- "+tt.want+"\n")
		})
	}
}
