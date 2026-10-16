package friction

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRenderMarkdownKeepsArchivedTextLiteral(t *testing.T) {
	for _, tt := range []struct {
		name string
		text string
		want string
	}{
		{"embedded backticks", "use `pwd` now", "``use `pwd` now``"},
		{"boundary backticks", "`Bash` x3 identical arguments", "`` `Bash` x3 identical arguments ``"},
		{"trailing backtick", "run `pwd`", "`` run `pwd` ``"},
		{"longest run", "a``b```c", "````a``b```c````"},
		{"only backticks", "```", "```` ``` ````"},
		{"boundary spaces", "  text  ", "`   text   `"},
		{"leading space", " text", "` text`"},
		{"only spaces", "   ", "`   `"},
		{"controls and markup", "<b>**text**</b>\n\t", "`<b>**text**</b>  `"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			body := string(RenderMarkdown(DigestSnapshot{
				Date:    "2026-09-16",
				Signals: []Signal{{Kind: KindCorrection, SubjectID: "s1", Text: tt.text}},
			}))
			assert.Contains(t, body, "- `s1` — "+tt.want+"\n")
		})
	}
}
