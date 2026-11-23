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
		{`GH_REPO=other/repo gh pr view 42`, "", remote, "https://github.com/other/repo/pull/42", false},
		{`GH_REPO=other/repo gh pr view 42 -R example/repo`, "", remote, "https://github.com/example/repo/pull/42", false},
		{`GH_REPO="$REPO" gh pr view 42`, "", remote, "", false},
		{`GH_REPO="$REPO" gh pr view 42 -R example/repo`, "", remote, "https://github.com/example/repo/pull/42", false},
		{`GH_HOST=host.example GH_REPO=other/repo gh pr view 42`, "", remote, "", false},
		{`GH_REPO=host.example/other/repo gh pr view 42`, "", remote, "https://host.example/other/repo/pull/42", false},
		{`f() { cd /other; }; gh pr view 42`, "", remote, "", true},
		{`f() { export GH_HOST=git.example; }; f; gh pr view 42 -R example/repo`, "", remote, "", true},
		{`f() { export GH_HOST=git.example; }; f; gh pr view 42 -R git.example/example/repo`, "", remote, "https://git.example/example/repo/pull/42", true},
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
		{`export GH_HOST=git.example; gh pr view 42 -R example/repo`, "", remote, "", true},
		{`export GH_HOST=git.example; gh pr view 42 -R git.example/example/repo`, "", remote, "https://git.example/example/repo/pull/42", true},
		{`(export GH_HOST=git.example); gh pr view 42`, "", remote, "https://github.com/example/repo/pull/42", false},
		{`echo $(export GH_HOST=git.example); gh pr view 42`, "", remote, "https://github.com/example/repo/pull/42", false},
		{`cat <(export GH_HOST=git.example); gh pr view 42`, "", remote, "https://github.com/example/repo/pull/42", false},
		{`export GH_HOST=git.example; bash -c 'gh pr view 42 -R example/repo'`, "", remote, "", true},
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

func TestGHPRCommandReviewCases(t *testing.T) {
	const remote = "https://github.com/example/repo.git"
	for _, tc := range []struct {
		name, command, want string
		changed             bool
	}{
		{"remote changed", `git remote set-url origin https://github.com/example/other.git && gh pr view 42`, "", true},
		{"remote changed in subshell", `(git remote set-url origin https://github.com/example/other.git); gh pr view 42`, "", true},
		{"subshell repository environment", `(export GH_REPO=other/repo); gh pr view 42`, "https://github.com/example/repo/pull/42", false},
		{"remote changed explicit repo", `git remote set-url origin https://github.com/example/other.git; gh pr view 42 -R example/repo`, "https://github.com/example/repo/pull/42", true},
		{"return to checkout", `cd /other && cd /work/repo && gh pr view 42`, "https://github.com/example/repo/pull/42", false},
		{"return after remote changed", `git remote set-url origin https://github.com/example/other.git; cd /other; cd /work/repo; gh pr view 42`, "", true},
		{"env wrapper", `env gh pr view 42 -R example/repo`, "https://github.com/example/repo/pull/42", false},
		{"env repo", `env GH_REPO=example/repo gh pr view 42`, "https://github.com/example/repo/pull/42", false},
		{"env host unknown", `env GH_HOST=git.example gh pr view 42 -R example/repo`, "", false},
		{"env host qualified", `env GH_HOST=git.example gh pr view 42 -R git.example/example/repo`, "https://git.example/example/repo/pull/42", false},
		{"default changed", `gh repo set-default example/other && gh pr view 42`, "", true},
		{"default in subshell", `(gh repo set-default example/other); gh pr view 42`, "", true},
		{"default explicit repo", `gh repo set-default example/other; gh pr view 42 -R example/repo`, "https://github.com/example/repo/pull/42", true},
		{"default repo env", `gh repo set-default example/other; GH_REPO=example/repo gh pr view 42`, "https://github.com/example/repo/pull/42", true},
		{"default viewed", `gh repo set-default --view; gh pr view 42`, "https://github.com/example/repo/pull/42", false},
		{"default viewed short flag", `gh repo set-default -v; gh pr view 42`, "https://github.com/example/repo/pull/42", false},
		{"windows executable", `gh.exe pr view 42 -R example/repo`, "https://github.com/example/repo/pull/42", false},
		{"windows executable case", `GH.EXE pr view 42 -R example/repo`, "https://github.com/example/repo/pull/42", false},
		{"repository URL", `gh pr view 42 -R https://github.com/example/repo`, "https://github.com/example/repo/pull/42", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			links, changed, err := CommandPRLinks(t.Context(), tc.command, "/work/repo", remote)
			require.NoError(t, err)
			assert.Equal(t, tc.changed, changed)
			if tc.want == "" {
				assert.Empty(t, links)
			} else if assert.Len(t, links, 1) {
				assert.Equal(t, tc.want, links[0].URL)
			}
		})
	}
}

func TestLauncherPromptReviewCases(t *testing.T) {
	for _, tc := range []struct{ name, command string }{
		{"allowed tools", `claude --allowedTools Bash -p 'Review https://github.com/example/repo/pull/42'`},
		{"piped prompt", "cat <<'EOF' | claude -p\nReview https://github.com/example/repo/pull/42\nEOF"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			links, _, err := CommandPRLinks(t.Context(), tc.command, "", "")
			require.NoError(t, err)
			if assert.Len(t, links, 1) {
				assert.Equal(t, "https://github.com/example/repo/pull/42", links[0].URL)
			}
		})
	}
}
