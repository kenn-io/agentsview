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
	_, _, err := CommandPRLinks(ctx, "gh pr view 42 -R example/repo", "", "")
	assert.ErrorIs(t, err, context.Canceled)
}

func TestSamePRWorkingDirectory(t *testing.T) {
	for _, tc := range []struct {
		name, cwd, dir string
		want           bool
	}{
		{"posix", "/work/repo", "/work/./repo", true},
		{"backslash", `C:\work\repo`, `C:/work/repo`, true},
		{"relative", "/work/repo", "../repo", false},
		{"empty", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, SamePRWorkingDirectory(tc.cwd, tc.dir))
		})
	}
}

func TestGHPRCommandLinks(t *testing.T) {
	remote := "https://github.com/example/repo.git"
	for _, tc := range []struct {
		command, cwd, remote, want string
		changed                    bool
	}{
		{`gh pr view 42`, "", remote, "https://github.com/example/repo/pull/42", false},
		{`gh pr merge 42 -R example/other`, "", remote, "https://github.com/example/other/pull/42", false},
		{`gh pr co 42 -R example/repo`, "", remote, "https://github.com/example/repo/pull/42", false},
		{`gh --repo=example/other pr view 42`, "", remote, "https://github.com/example/other/pull/42", false},
		{`cd /repo && gh pr merge 42`, "/repo", remote, "https://github.com/example/repo/pull/42", false},
		{`cd 'C:\work\repo' && gh pr view 42`, `C:\work\repo`, remote, "https://github.com/example/repo/pull/42", false},
		{`cd '..\repo' && gh pr view 42`, `C:\work\repo`, remote, "", true},
		{`cd 'C:\work\other' && gh pr view 42`, `C:\work\repo`, remote, "", true},
		{`gh pr view 42; cd /other; gh pr view 43`, "", remote, "https://github.com/example/repo/pull/42", true},
		{`gh pr diff https://github.com/example/repo/pull/42`, "", "", "https://github.com/example/repo/pull/42", false},
		{`echo 'gh pr view 42 -R example/repo'`, "", remote, "", false},
		{`cat <<EOF
gh pr view 42 -R example/repo
EOF`, "", remote, "", false},
		{`gh pr view 42; GH_REPO=other/repo`, "", remote, "https://github.com/example/repo/pull/42", true},
		{`cd /other; gh pr view 42`, "", remote, "", true},
		{`pushd /other`, "", remote, "", true},
		{`popd`, "", remote, "", true},
		{`eval true; gh pr view 42`, "", remote, "", true},
		{`source script`, "", remote, "", true},
		{`. script`, "", remote, "", true},
		{`unset GH_REPO`, "", remote, "", false},
		{`export GH_HOST=example.com`, "", remote, "", true},
		{`echo GH_REPO`, "", remote, "", false},
		{`GH_REPO=other/repo gh pr view 42`, "", remote, "", false},
		{`f() { cd /other; }; gh pr view 42`, "", remote, "", true},
		{`cd /other; gh pr view 42 -R example/repo`, "", remote, "https://github.com/example/repo/pull/42", true},
		{`gh pr comment --body 41 42 -R example/repo`, "", remote, "https://github.com/example/repo/pull/42", false},
		{`gh pr view --json title 42`, "", remote, "https://github.com/example/repo/pull/42", false},
		{`gh pr create -R example/repo`, "", remote, "", false},
		{`gh pr view 42 --repo=`, "", remote, "", false},
		{`gh pr view 42 --repo="$REPO"`, "", remote, "", false},
		{`gh pr view 42 -R"$REPO"`, "", remote, "", false},
		{`gh pr comment 42 --body "$MSG" -R example/repo`, "", remote, "https://github.com/example/repo/pull/42", false},
		{`GH_HOST=enterprise gh pr view 42 -R example/repo`, "", remote, "", false},
		{`GH_HOST=enterprise gh pr view 42 -R git.example/example/repo`, "", remote, "https://git.example/example/repo/pull/42", false},
		{`GH_HOST=enterprise gh pr view https://github.com/example/repo/pull/42`, "", remote, "https://github.com/example/repo/pull/42", false},
		{`GH_HOST=enterprise true; gh pr view 42`, "", remote, "https://github.com/example/repo/pull/42", false},
		{`(cd /other); gh pr view 42`, "", remote, "https://github.com/example/repo/pull/42", false},
		{`echo $(cd /other && pwd); gh pr view 42`, "", remote, "https://github.com/example/repo/pull/42", false},
		{`cat <(cd /other && pwd); gh pr view 42`, "", remote, "https://github.com/example/repo/pull/42", false},
		{`/bin/bash -lc 'gh pr view 42 -R example/repo'`, "", remote, "https://github.com/example/repo/pull/42", false},
		{`bash -c 'cd /other'; gh pr view 42`, "", remote, "https://github.com/example/repo/pull/42", false},
		{`GH_HOST=enterprise bash -c 'gh pr view 42 -R example/repo'`, "", remote, "", false},
	} {
		t.Run(tc.command, func(t *testing.T) {
			links, changed, err := CommandPRLinks(t.Context(), tc.command, tc.cwd, tc.remote)
			require.NoError(t, err)
			assert.Equal(t, tc.changed, changed)
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
		{`claude --help https://github.com/example/repo/pull/1`, []string{"https://github.com/example/repo/pull/1"}},
		{`claude --model "$MODEL" -p 'review https://github.com/example/repo/pull/11'`, []string{"https://github.com/example/repo/pull/11"}},
		{`claude -p "$PROMPT"`, nil},
		{`codex -m "$MODEL" exec 'review https://github.com/example/repo/pull/12'`, []string{"https://github.com/example/repo/pull/12"}},
		{`claude --model https://github.com/example/repo/pull/2 -p 'hello'`, []string{"https://github.com/example/repo/pull/2"}},
		{`claude --print 'review https://github.com/example/repo/pull/3'`, []string{"https://github.com/example/repo/pull/3"}},
		{`claude --resume https://github.com/example/repo/pull/10 -p hello`, []string{"https://github.com/example/repo/pull/10"}},
		{`claude -p 'review https://github.com/example/repo/pull/4; report'`, []string{"https://github.com/example/repo/pull/4"}},
		{`codex --help https://github.com/example/repo/pull/5`, []string{"https://github.com/example/repo/pull/5"}},
		{`codex exec --help https://github.com/example/repo/pull/6`, []string{"https://github.com/example/repo/pull/6"}},
		{`codex exec --output-schema https://github.com/example/repo/pull/7 'hello'`, []string{"https://github.com/example/repo/pull/7"}},
		{`codex -m model exec -C /tmp 'review https://github.com/example/repo/pull/8'`, []string{"https://github.com/example/repo/pull/8"}},
		{`codex exec resume https://github.com/example/repo/pull/9`, []string{"https://github.com/example/repo/pull/9"}},
	}
	for _, tt := range launcherTests {
		t.Run(tt.command, func(t *testing.T) {
			links, _, err := CommandPRLinks(t.Context(), tt.command, "", "")
			require.NoError(t, err)
			var urls []string
			for _, link := range links {
				urls = append(urls, link.URL)
			}
			assert.Equal(t, tt.want, urls)
		})
	}
}
