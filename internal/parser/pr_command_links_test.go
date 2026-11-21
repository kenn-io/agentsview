package parser

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGHPRCommandLinks(t *testing.T) {
	tests := []struct{ command, remote, want string }{
		{"export GH_REPO=example/repo; unset GH_REPO; gh pr view 42", "https://github.com/other/repo.git", "https://github.com/other/repo/pull/42"},
		{"export GH_REPO=$UNKNOWN; gh pr view 42", "https://github.com/other/repo.git", ""},
		{"env GH_REPO=example/repo timeout 10 nohup gh pr view 42", "", "https://github.com/example/repo/pull/42"},
		{"echo $(export GH_REPO=example/repo; gh pr view 42)", "", "https://github.com/example/repo/pull/42"},
		{"gh pr view 42 -R example/repo | cat", "", "https://github.com/example/repo/pull/42"},
		{"export GH_REPO=example/repo; gh pr view 42", "", "https://github.com/example/repo/pull/42"},
		{"export GH_HOST=forge.example; gh pr view 42 -R example/repo", "", "https://forge.example/example/repo/pull/42"},
		{"env GH_REPO=example/repo gh pr view 42", "", "https://github.com/example/repo/pull/42"},
		{"echo $(gh pr view 42 -R example/repo)", "", "https://github.com/example/repo/pull/42"},
		{"false || gh pr view 42 -R example/repo", "", "https://github.com/example/repo/pull/42"},
		{"true || gh pr view 42 -R example/repo", "", ""},
		{"maybe || gh pr view 42 -R example/repo", "", ""},
		{"if true; then gh pr view 42 -R example/repo; fi", "", "https://github.com/example/repo/pull/42"},
		{"if maybe; then gh pr view 42 -R example/repo; fi", "", ""},
		{"gh pr view https://github.com/example/repo/pull/42", "", "https://github.com/example/repo/pull/42"},
		{"gh pr merge 42 -R forge.example/example/repo", "", "https://forge.example/example/repo/pull/42"},
		{"gh pr view 42 --json title", "git@github.com:example/repo.git", "https://github.com/example/repo/pull/42"},
		{"GH_REPO=example/repo gh pr view 42", "", "https://github.com/example/repo/pull/42"},
		{"gh pr view --repo=example/repo 42", "", "https://github.com/example/repo/pull/42"},
		{"GH_REPO=$OTHER gh pr view https://github.com/example/repo/pull/42", "", "https://github.com/example/repo/pull/42"},
		{"GH_REPO=$OTHER gh pr view 42", "https://github.com/example/repo.git", ""},
		{"cd $OTHER && gh pr view 7", "https://github.com/example/repo.git", ""},
		{"cd /tmp && gh pr view 7", "https://github.com/example/repo.git", ""},
		{"cd /tmp; gh pr view 7 -R example/repo", "https://github.com/other/repo.git", "https://github.com/example/repo/pull/7"},
		{"gh pr comment --body 'done; thanks' 42 -R example/repo", "", "https://github.com/example/repo/pull/42"},
		{`GH_HOST=forge.example gh pr view 42 -R example/alpha`, "", "https://forge.example/example/alpha/pull/42"},
		{`GH_HOST=forge.example gh pr view 42 -R other.example/example/alpha`, "", "https://other.example/example/alpha/pull/42"},
		{`GH_HOST=$HOST gh pr view 42 -R example/alpha`, "", ""},
		{`GH_HOST=forge.example gh pr view 42`, "https://github.com/example/repo.git", "https://github.com/example/repo/pull/42"},
		{`gh pr comment 42 -R example/alpha --body "$(cat /tmp/pr-comment)"`, "", "https://github.com/example/alpha/pull/42"},
		{`gh pr comment --body "$BODY" 42 -R example/alpha`, "", "https://github.com/example/alpha/pull/42"},
		{`gh pr view "$SELECTOR" -R example/alpha`, "", ""},
		{`gh pr view 42 "$FLAGS" -R example/alpha`, "", ""},
		{`gh pr view 42 -R "$REPO"`, "", ""},
		{`GH_REPO=$REPO gh pr view 42 -R example/alpha`, "", "https://github.com/example/alpha/pull/42"},
		{"gh pr merge --squash 42 -R example/repo", "", "https://github.com/example/repo/pull/42"},
		{"gh pr merge --unknown 42 -R example/repo", "", ""},
		{"gh pr view 42", "", ""},
		{"gh pr view 42", "https://forge.example/group/sub/repo.git", ""},
		{"gh pr list -R example/repo", "", ""},
		{"gh pr create -R example/repo", "", ""},
		{"cd x; gh pr view 7 -R example/repo", "", "https://github.com/example/repo/pull/7"},
		{`gh pr comment --body 43 42 -R example/repo`, "", "https://github.com/example/repo/pull/42"},
		{"gh pr comment 42 -R example/repo --body 'done; thanks\nnext'", "", "https://github.com/example/repo/pull/42"},
		{"gh -Rexample/repo pr view 42", "", "https://github.com/example/repo/pull/42"},
		{"/usr/bin/gh -R example/repo pr view 42", "", "https://github.com/example/repo/pull/42"},
		{"gh pr view 42 -R example/repo#5", "", ""},
		{"gh pr view 42 --repo=", "https://github.com/example/repo.git", ""},
		{"gh pr view https://github.com/example/repo/pull/42extra", "", ""},
		{"gh pr view https://forge.example/group/sub/repo/pull/42?x=1", "", "https://forge.example/group/sub/repo/pull/42"},
	}
	for _, tt := range tests {
		t.Run(tt.command, func(t *testing.T) {
			links := CommandPRLinks(tt.command, tt.remote)
			if tt.want == "" {
				assert.Empty(t, links)
				return
			}
			if assert.Len(t, links, 1) {
				assert.Equal(t, tt.want, links[0].URL)
			}
		})
	}
}

func TestMergePRLinksIdentity(t *testing.T) {
	a, _ := NewPRLink("https://github.com/Example/Repo/pull/42", "Example/Repo", 42)
	b, _ := NewPRLink("https://github.com/example/repo/pull/42", "example/repo", 42)
	assert.Equal(t, []PRLink{a}, CombinePRLinks([]PRLink{a}, []PRLink{b}))
}
