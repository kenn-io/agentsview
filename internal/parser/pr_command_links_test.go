package parser

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGHPRCommandLinks(t *testing.T) {
	tests := []struct{ command, remote, want string }{
		{`GH_REPO=other/repo gh pr view 1; gh pr view 42`, "https://github.com/example/repo.git", "https://github.com/example/repo/pull/42"},
		{`env GH_HOST=other.example gh pr view 1; gh pr view 42`, "https://github.com/example/repo.git", "https://github.com/example/repo/pull/42"},
		{`declare -x GH_REPO=a/b; gh pr view 42`, "https://github.com/example/repo.git", ""},
		{`declare -x FOO=1; gh pr view 42`, "https://github.com/example/repo.git", "https://github.com/example/repo/pull/42"},
		{`typeset GH_HOST=other.example; gh pr view 42`, "https://github.com/example/repo.git", ""},
		{`readonly GH_REPO=a/b; gh pr view 42`, "https://github.com/example/repo.git", ""},
		{`local GH_HOST=other.example; gh pr view 42`, "https://github.com/example/repo.git", ""},
		{`builtin cd /other; gh pr view 42`, "https://github.com/example/repo.git", ""},
		{`command cd /other; gh pr view 42`, "https://github.com/example/repo.git", ""},
		{`builtin pushd /other; gh pr view 42`, "https://github.com/example/repo.git", ""},
		{`command popd; gh pr view 42`, "https://github.com/example/repo.git", ""},
		{`builtin eval 'GH_REPO=a/b'; gh pr view 42`, "https://github.com/example/repo.git", ""},
		{`command source config; gh pr view 42`, "https://github.com/example/repo.git", ""},
		{`GH_HOST=github.com gh pr view 42 -R example/repo`, "", "https://github.com/example/repo/pull/42"},
		{`env GH_HOST=github.com gh pr view 42 -R example/repo`, "", "https://github.com/example/repo/pull/42"},
		{`GH_HOST=ghe.example.com gh pr view 42 -R example/repo`, "", ""},
		{`GH_HOST=github.com; gh pr view 42 -R example/repo`, "", ""},
		{`export GH_HOST=github.com; gh pr view 42 -R example/repo`, "", ""},
		{`export FOO=1; gh pr view 42 -R example/repo`, "", "https://github.com/example/repo/pull/42"},
		{`unset FOO; gh pr view 42 -R example/repo`, "", "https://github.com/example/repo/pull/42"},
		{`export GH_REPO=a/b; gh pr view 42 -R example/repo`, "", ""},
		{`export "$N=x"; gh pr view 42 -R example/repo`, "", ""},
		{"gh pr view 42 -R example/repo | cat", "", "https://github.com/example/repo/pull/42"},
		{"{ cd /other; }; gh pr view 42", "https://github.com/example/repo.git", ""},
		{"{ gh pr view 42 -R example/repo; }", "", "https://github.com/example/repo/pull/42"},
		{"cat | gh pr view 42 -R example/repo", "", "https://github.com/example/repo/pull/42"},
		{"echo $(gh pr view 42 -R example/repo)", "", "https://github.com/example/repo/pull/42"},
		{"gh pr co 42 -R example/repo", "", "https://github.com/example/repo/pull/42"},
		{"gh pr view https://github.com/example/repo/pull/42", "", "https://github.com/example/repo/pull/42"},
		{"gh pr merge 42 -R forge.example/example/repo", "", "https://forge.example/example/repo/pull/42"},
		{"gh pr view 42 --json title", "git@github.com:example/repo.git", "https://github.com/example/repo/pull/42"},
		{"gh pr view --repo=example/repo 42", "", "https://github.com/example/repo/pull/42"},
		{"GH_REPO=$OTHER gh pr view https://github.com/example/repo/pull/42", "", "https://github.com/example/repo/pull/42"},
		{"GH_REPO=$OTHER gh pr view 42", "https://github.com/example/repo.git", ""},
		{"cd $OTHER && gh pr view 7", "https://github.com/example/repo.git", ""},
		{"cd /tmp; gh pr view 7 -R example/repo", "https://github.com/other/repo.git", "https://github.com/example/repo/pull/7"},
		{`GH_HOST=$HOST gh pr view 42 -R example/alpha`, "", ""},
		{`gh pr comment 42 -R example/alpha --body "$(cat /tmp/pr-comment)"`, "", "https://github.com/example/alpha/pull/42"},
		{`gh pr comment --body "$BODY" 42 -R example/alpha`, "", "https://github.com/example/alpha/pull/42"},
		{`gh pr comment --body="$BODY" 42 -R example/alpha`, "", "https://github.com/example/alpha/pull/42"},
		{`gh pr comment -b"$BODY" 42 -R example/alpha`, "", "https://github.com/example/alpha/pull/42"},
		{`gh pr view 42 --repo="$REPO"`, "https://github.com/example/repo.git", ""},
		{`/bin/bash -lc 'gh pr view 42 -R example/alpha'`, "", "https://github.com/example/alpha/pull/42"},
		{`bash -c 'cd /other; gh pr view 42'`, "https://github.com/example/repo.git", ""},
		{`gh pr view "$SELECTOR" -R example/alpha`, "", ""},
		{`gh pr view 42 "$FLAGS" -R example/alpha`, "", ""},
		{`gh pr view 42 -R "$REPO"`, "", ""},
		{"gh pr merge --squash 42 -R example/repo", "", "https://github.com/example/repo/pull/42"},
		{"gh pr merge --unknown 42 -R example/repo", "", ""},
		{"gh pr view 42", "", ""},
		{"gh pr view 42", "https://forge.example/group/sub/repo.git", ""},
		{"gh pr list -R example/repo", "", ""},
		{"gh pr create -R example/repo", "", ""},
		{`gh pr comment --body 43 42 -R example/repo`, "", "https://github.com/example/repo/pull/42"},
		{"gh pr comment 42 -R example/repo --body 'done; thanks\nnext'", "", "https://github.com/example/repo/pull/42"},
		{"gh -Rexample/repo pr view 42", "", "https://github.com/example/repo/pull/42"},
		{"/usr/bin/gh -R example/repo pr view 42", "", "https://github.com/example/repo/pull/42"},
		{"gh pr view 42 -R example/repo#5", "", ""},
		{"gh pr view 42 --repo=", "https://github.com/example/repo.git", ""},
		{`eval "export GH_REPO=a/b"; gh pr view 42`, "https://github.com/example/repo.git", ""},
		{`eval "export GH_REPO=a/b"; gh pr view 42 -R example/repo`, "https://github.com/example/repo.git", ""},
		{`eval "export GH_REPO=a/b"; gh pr view 42 -R github.com/example/repo`, "https://github.com/example/repo.git", "https://github.com/example/repo/pull/42"},
		{`eval "export GH_REPO=a/b"; gh pr view https://github.com/example/repo/pull/42`, "https://github.com/example/repo.git", "https://github.com/example/repo/pull/42"},
		{`GH_TOKEN=x gh pr view 42`, "https://github.com/example/repo.git", "https://github.com/example/repo/pull/42"},
		{`false && gh pr view 42 -R example/repo`, "", "https://github.com/example/repo/pull/42"},
		{`gh pr view 42; eval "export GH_REPO=a/b"; gh pr view 43`, "https://github.com/example/repo.git", "https://github.com/example/repo/pull/42"},
		{`gh pr view 42; cd /other; gh pr view 43`, "https://github.com/example/repo.git", "https://github.com/example/repo/pull/42"},
	}
	for _, tt := range tests {
		t.Run(tt.command, func(t *testing.T) {
			links, _ := CommandPRLinks(tt.command, tt.remote)
			if tt.want == "" {
				assert.Empty(t, links)
				return
			}
			if assert.Len(t, links, 1) {
				assert.Equal(t, tt.want, links[0].URL)
			}
		})
	}
	t.Run("child shell directory", func(t *testing.T) {
		links, changed := CommandPRLinks(`bash -c 'gh pr view 41; cd /other; gh pr view 99'; gh pr view 42`, "https://github.com/example/repo.git")
		assert.False(t, changed)
		if assert.Len(t, links, 2) {
			assert.Equal(t, "https://github.com/example/repo/pull/41", links[0].URL)
			assert.Equal(t, "https://github.com/example/repo/pull/42", links[1].URL)
		}
	})
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
		{`timeout 60 claude -p 'review https://github.com/example/repo/pull/43'`, []string{"https://github.com/example/repo/pull/43"}},
		{`env FOO=bar nohup codex exec 'review https://github.com/example/repo/pull/44'`, []string{"https://github.com/example/repo/pull/44"}},
	}
	for _, tt := range launcherTests {
		t.Run(tt.command, func(t *testing.T) {
			links, _ := CommandPRLinks(tt.command, "")
			var urls []string
			for _, link := range links {
				urls = append(urls, link.URL)
			}
			assert.Equal(t, tt.want, urls)
		})
	}
}
