package friction

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCode(t *testing.T) {
	for _, tt := range []struct {
		name string
		text string
		want string
	}{
		{"empty", "", ""},
		{"embedded backticks", "use `pwd` now", "``use `pwd` now``"},
		{"boundary backticks", "`Bash` x3 identical arguments", "`` `Bash` x3 identical arguments ``"},
		{"trailing backtick", "run `pwd`", "`` run `pwd` ``"},
		{"longest run", "a``b```c", "````a``b```c````"},
		{"boundary spaces", "  text  ", "`   text   `"},
		{"leading space", " text", "` text`"},
		{"only spaces", "   ", "`   `"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, code(tt.text))
		})
	}
}

func TestRenderMarkdownEmptyErrorText(t *testing.T) {
	signals := DetectErrors([]Message{failedTool("Edit", "")}, "s1")
	require.Len(t, signals, 1)
	body := RenderMarkdown(DigestSnapshot{Date: "2026-09-16", Signals: signals})
	assert.Contains(t, string(body), "## Errors\n\n- `s1` / `Edit`: \n\n## Workarounds\n")
}
