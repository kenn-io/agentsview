package friction

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRenderMarkdownKeepsArchivedTextOnOneLine(t *testing.T) {
	s := DigestSnapshot{
		Date: "2026-09-16",
		Signals: []Signal{{
			Kind: KindError, SubjectID: "s\n1", ToolName: "Ba`\nsh",
			Text: "fatal: denied\n## Spend\n- forged",
		}},
		P0Alerts: map[string][]string{"Ba`\nsh": {"s\n1", "s2", "s3"}},
	}
	body := string(RenderMarkdown(s))
	assert.NotContains(t, body, "\n## Spend")
	_, errors, ok := strings.Cut(body, "## Errors\n\n")
	require.True(t, ok)
	errors, _, ok = strings.Cut(errors, "\n\n## Workarounds")
	require.True(t, ok)
	bullets := 0
	for _, line := range strings.Split(errors, "\n") {
		if strings.HasPrefix(line, "- ") {
			bullets++
		}
	}
	assert.Equal(t, 1, bullets)
	assert.Equal(t, 4, strings.Count(errors, "`"))
	assert.Equal(t, "- `s 1` / `Ba' sh`: fatal: denied ## Spend - forged", errors)
	assert.Contains(t, body, "- **P0 ALERT**: `Ba' sh` failed in 3 distinct sessions: s 1, s2, s3\n")
}
