package skills

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRemoteArgs(t *testing.T) {
	assert.Empty(t, Remote{}.Args())
	assert.Equal(t, " --server https://example.invalid",
		Remote{Server: "https://example.invalid"}.Args())
	assert.Equal(t,
		" --server https://example.invalid --server-token-file token",
		Remote{Server: "https://example.invalid", TokenFile: "token"}.Args())
}

func TestRenderBakesServerArgsAndRemoteLine(t *testing.T) {
	remote := Remote{Server: "https://example.invalid", TokenFile: "token"}
	rendered, err := Render(HarnessClaude, "dev", remote)
	require.NoError(t, err)

	assert.Contains(t, rendered.Content, "--server https://example.invalid")
	assert.Contains(t, rendered.Content, "--server-token-file token")
	assert.Contains(t, rendered.Content, "--exclude-session <this-session-id>")
	assert.NotContains(t, rendered.Content, "--fts --in")
	assert.Equal(t, remote, ParseRemote(rendered.Content))
	assert.Equal(t, StateCurrent, Classify([]byte(rendered.Content), rendered))
}

func TestRenderWithoutRemoteOmitsServerFlags(t *testing.T) {
	rendered, err := Render(HarnessClaude, "dev", Remote{})
	require.NoError(t, err)
	assert.NotContains(t, rendered.Content, "--limit 8 --server")
	assert.NotContains(t, rendered.Content, "--json --server")
	assert.True(t, ParseRemote(rendered.Content).Empty())
	assert.Contains(t, rendered.Content, "silently searches local SQLite")
}

func TestParseRemoteIgnoresMalformedLine(t *testing.T) {
	assert.True(t, ParseRemote(skillFileWithRemoteLine("not-json")).Empty())
}

// skillFileWithRemoteLine builds the first four lines of an installed skill
// file with an arbitrary install-remote payload, so ParseRemote can be
// exercised on files it did not render itself.
func skillFileWithRemoteLine(payload string) string {
	return "---\n# generated-by: agentsview dev hash:" + strings.Repeat("a", 64) +
		" — do not edit; re-run `agentsview skills install`\n" +
		installRemotePrefix + payload + "\nname: x\n"
}

func TestRemoteArgsQuotesUnsafeValues(t *testing.T) {
	tests := []struct {
		name   string
		remote Remote
		want   string
	}{
		{
			name:   "safe values stay bare",
			remote: Remote{Server: "https://example.invalid", TokenFile: "~/.tok"},
			want:   " --server https://example.invalid --server-token-file ~/.tok",
		},
		{
			name:   "space is quoted",
			remote: Remote{Server: "https://example.invalid", TokenFile: "/My Tokens/tok"},
			want:   " --server https://example.invalid --server-token-file '/My Tokens/tok'",
		},
		{
			name:   "shell metacharacters are quoted",
			remote: Remote{Server: "https://example.invalid;rm -rf /"},
			want:   ` --server 'https://example.invalid;rm -rf /'`,
		},
		{
			name:   "embedded single quote is escaped",
			remote: Remote{Server: `a'b`},
			want:   ` --server 'a'\''b'`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.remote.Args())
		})
	}
}

func TestRemoteValidateRejectsControlCharacters(t *testing.T) {
	assert.NoError(t, Remote{Server: "https://example.invalid"}.Validate())
	assert.Error(t, Remote{Server: "https://example.invalid\nname: evil"}.Validate())
	assert.Error(t, Remote{Server: "ok", TokenFile: "tok\ttab"}.Validate())

	_, err := Render(HarnessClaude, "dev", Remote{Server: "a\nb"})
	assert.Error(t, err, "Render must refuse a remote that would break the file")
}

// TestParseRemoteDropsControlCharacters covers a hand-edited file whose JSON
// is well formed but decodes to a value that would not survive re-rendering.
func TestParseRemoteDropsControlCharacters(t *testing.T) {
	body := skillFileWithRemoteLine(`{"server":"https://example.invalid\nname: evil"}`)
	assert.True(t, ParseRemote(body).Empty())
}
