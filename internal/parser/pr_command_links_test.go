package parser

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCommandPRLinksCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _, err := CommandPRLinks(ctx, "gh pr view 42 -R example/repo", "")
	assert.ErrorIs(t, err, context.Canceled)
}

func TestGHPRCommandLinks(t *testing.T) {
	remote := "https://github.com/example/repo.git"
	for _, tc := range []struct{ command, remote, want string }{
		{`gh pr view 42`, remote, "https://github.com/example/repo/pull/42"},
		{`gh pr merge 42 -R example/other`, remote, "https://github.com/example/other/pull/42"},
		{`gh pr diff https://github.com/example/repo/pull/42`, "", "https://github.com/example/repo/pull/42"},
		{`echo 'gh pr view 42 -R example/repo'`, remote, ""},
		{`cat <<EOF
gh pr view 42 -R example/repo
EOF`, remote, ""},
		{`cd /other; gh pr view 42`, remote, ""},
		{`eval true; gh pr view 42`, remote, ""},
		{`GH_REPO=other/repo gh pr view 42`, remote, ""},
		{`f() { cd /other; }; gh pr view 42`, remote, ""},
		{`cd /other; gh pr view 42 -R example/repo`, remote, "https://github.com/example/repo/pull/42"},
		{`gh pr comment --body 41 42 -R example/repo`, remote, "https://github.com/example/repo/pull/42"},
		{`gh pr view --json title 42`, remote, "https://github.com/example/repo/pull/42"},
		{`gh pr create -R example/repo`, remote, ""},
		{`gh pr view 42 --repo=`, remote, ""},
	} {
		t.Run(tc.command, func(t *testing.T) {
			links, _, err := CommandPRLinks(t.Context(), tc.command, tc.remote)
			require.NoError(t, err)
			if tc.want == "" {
				assert.Empty(t, links)
				return
			}
			require.Len(t, links, 1)
			assert.Equal(t, tc.want, links[0].URL)
		})
	}
}

func TestLauncherPromptPRLinks(t *testing.T) {
	launcherTests := []struct {
		command string
		want    []string
	}{
		{`claude --help https://github.com/example/repo/pull/1`, nil},
		{`claude --model "$MODEL" -p 'review https://github.com/example/repo/pull/11'`, []string{"https://github.com/example/repo/pull/11"}},
		{`claude -p "$PROMPT"`, nil},
		{`codex -m "$MODEL" exec 'review https://github.com/example/repo/pull/12'`, []string{"https://github.com/example/repo/pull/12"}},
		{`claude --model https://github.com/example/repo/pull/2 -p 'hello'`, nil},
		{`claude --print 'review https://github.com/example/repo/pull/3'`, []string{"https://github.com/example/repo/pull/3"}},
		{`claude --resume https://github.com/example/repo/pull/10 -p hello`, nil},
		{`claude -p 'review https://github.com/example/repo/pull/4; report'`, []string{"https://github.com/example/repo/pull/4"}},
		{`codex --help https://github.com/example/repo/pull/5`, nil},
		{`codex exec --help https://github.com/example/repo/pull/6`, nil},
		{`codex exec --output-schema https://github.com/example/repo/pull/7 'hello'`, nil},
		{`codex -m model exec -C /tmp 'review https://github.com/example/repo/pull/8'`, []string{"https://github.com/example/repo/pull/8"}},
		{`codex exec resume https://github.com/example/repo/pull/9`, nil},
	}
	for _, tt := range launcherTests {
		t.Run(tt.command, func(t *testing.T) {
			links, _, err := CommandPRLinks(t.Context(), tt.command, "")
			require.NoError(t, err)
			var urls []string
			for _, link := range links {
				urls = append(urls, link.URL)
			}
			assert.Equal(t, tt.want, urls)
		})
	}
}
