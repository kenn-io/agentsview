package parser

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// commandPRLinks derives links from a fresh shell and reports whether its
// context changed.
func commandPRLinks(ctx context.Context, command, cwd, remote string) ([]PRLink, bool, error) {
	links, s, err := CommandPRLinksWithContext(ctx, command, cwd, remote, PRShellContext{})
	return links, s.CheckoutUncertain || s.HostUncertain || s.RepoUncertain, err
}

func TestCommandPRLinksCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _, err := commandPRLinks(ctx, "gh pr view 42 -R example/repo", "", "")
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
		{`git remote set-url origin https://github.com/example/other.git; gh pr view 42`, "", remote, "", true},
		{`gh repo set-default example/other; gh pr view 42`, "", remote, "", true},
		{`git remote set-url origin https://github.com/example/other.git; gh pr view 42 -R example/other`, "", remote, "https://github.com/example/other/pull/42", true},
		{`(git remote set-url origin https://github.com/example/other.git); gh pr view 42`, "", remote, "", true},
		{`bash -c 'git remote set-url origin https://github.com/example/other.git'; gh pr view 42`, "", remote, "", true},
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
		{`unset GH_REPO`, "", remote, "", false},
		{`export GH_HOST=example.com`, "", remote, "", true},
		{`echo GH_REPO`, "", remote, "", false},
		{`GH_REPO=other/repo gh pr view 42`, "", remote, "https://github.com/other/repo/pull/42", false},
		{`export GH_REPO=example/other; gh pr view 42`, "", remote, "https://github.com/example/other/pull/42", true},
		{`export GH_REPO=example/other && gh pr view 42`, "", remote, "https://github.com/example/other/pull/42", true},
		{`GH_REPO=example/other; export GH_REPO; gh pr view 42`, "", remote, "https://github.com/example/other/pull/42", true},
		{`GH_REPO=example/other; gh pr view 42`, "", remote, "", true},
		{`export GH_REPO=example/other; GH_REPO=example/fork; gh pr view 42`, "", remote, "https://github.com/example/fork/pull/42", true},
		{`export GH_REPO="$REPO"; gh pr view 42`, "", remote, "", true},
		{`export GH_REPO=example/other; unset GH_REPO; gh pr view 42`, "", remote, "", true},
		{`(export GH_REPO=example/other); gh pr view 42`, "", remote, "https://github.com/example/repo/pull/42", false},
		{`GH_REPO=other/repo gh pr view 42 -R example/repo`, "", remote, "https://github.com/example/repo/pull/42", false},
		{`GH_REPO="$REPO" gh pr view 42`, "", remote, "", false},
		{`GH_REPO="$REPO" gh pr view 42 -R example/repo`, "", remote, "https://github.com/example/repo/pull/42", false},
		{`GH_HOST=host.example GH_REPO=other/repo gh pr view 42`, "", remote, "", false},
		{`GH_REPO=host.example/other/repo gh pr view 42`, "", remote, "https://host.example/other/repo/pull/42", false},
		{`other() { cd /other; }; gh pr view 42`, "", remote, "https://github.com/example/repo/pull/42", false},
		{`other() { cd /other; }; other; gh pr view 42`, "", remote, "", true},
		{`other() { gh pr view 41 -R example/repo; }; gh pr view 42`, "", remote, "https://github.com/example/repo/pull/42", false},
		{`export GH_REPO=example/other; bash -c 'gh pr view 42'`, "", remote, "https://github.com/example/other/pull/42", true},
		{`export GH_REPO=example/other; (export GH_REPO=example/fork); gh pr view 42`, "", remote, "https://github.com/example/other/pull/42", true},
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
		{`bash -c 'export GH_HOST=git.example'; gh pr view 42`, "", remote, "https://github.com/example/repo/pull/42", false},
		{`GH_REPO=other/repo bash -c 'gh pr view 42'`, "", remote, "https://github.com/other/repo/pull/42", false},
		{`GH_REPO="$REPO" bash -c 'gh pr view 42'`, "", remote, "", false},
		{`GH_REPO=other/repo bash -c 'gh pr view 42 -R example/repo'`, "", remote, "https://github.com/example/repo/pull/42", false},
		{`GH_REPO=other/repo bash -c 'GH_REPO=example/repo gh pr view 42'`, "", remote, "https://github.com/example/repo/pull/42", false},
		{`GH_REPO=other/repo bash -c true; gh pr view 42`, "", remote, "https://github.com/example/repo/pull/42", false},
		{`GH_HOST=enterprise bash -c 'gh pr view 42 -R example/repo'`, "", remote, "", false},
		{`export GH_HOST=git.example; gh pr view 42 -R example/repo`, "", remote, "", true},
		{`export GH_HOST=git.example; gh pr view 42 -R git.example/example/repo`, "", remote, "https://git.example/example/repo/pull/42", true},
		{`(export GH_HOST=git.example); gh pr view 42`, "", remote, "https://github.com/example/repo/pull/42", false},
		{`echo $(export GH_HOST=git.example); gh pr view 42`, "", remote, "https://github.com/example/repo/pull/42", false},
		{`cat <(export GH_HOST=git.example); gh pr view 42`, "", remote, "https://github.com/example/repo/pull/42", false},
		{`export GH_HOST=git.example; bash -c 'gh pr view 42 -R example/repo'`, "", remote, "", true},
		{`cd /other && cd /work/repo && gh pr view 42`, "/work/repo", remote, "", true},
		{`env gh pr view 42 -R example/repo`, "/work/repo", remote, "https://github.com/example/repo/pull/42", false},
		{`env GH_REPO=example/repo gh pr view 42`, "/work/repo", remote, "https://github.com/example/repo/pull/42", false},
		{`env GH_HOST=git.example gh pr view 42 -R example/repo`, "/work/repo", remote, "", false},
		{`env GH_HOST=git.example gh pr view 42 -R git.example/example/repo`, "/work/repo", remote, "https://git.example/example/repo/pull/42", false},
		{`gh.exe pr view 42 -R example/repo`, "/work/repo", remote, "https://github.com/example/repo/pull/42", false},
		{`GH.EXE pr view 42 -R example/repo`, "/work/repo", remote, "https://github.com/example/repo/pull/42", false},
		{`gh pr view 42 -R https://github.com/example/repo`, "/work/repo", remote, "https://github.com/example/repo/pull/42", false},
		{`c''d /other; gh pr view 42`, "/work/repo", remote, "", true},
		{`gh pr view 42`, "", "git@github.com:Example/Repo.git", "https://github.com/Example/Repo/pull/42", false},
	} {
		t.Run(tc.command, func(t *testing.T) {
			links, changed, err := commandPRLinks(t.Context(), tc.command, tc.cwd, tc.remote)
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
		{`claude --allowedTools Bash -p 'Review https://github.com/example/repo/pull/42'`, []string{"https://github.com/example/repo/pull/42"}},
		{"cat <<'EOF' | claude -p\nReview https://github.com/example/repo/pull/42\nEOF", []string{"https://github.com/example/repo/pull/42"}},
	}
	for _, tt := range launcherTests {
		t.Run(tt.command, func(t *testing.T) {
			links, _, err := commandPRLinks(t.Context(), tt.command, "", "")
			require.NoError(t, err)
			var urls []string
			for _, link := range links {
				urls = append(urls, link.URL)
			}
			assert.Equal(t, tt.want, urls)
		})
	}
}

func TestConditionalRepoAssignmentsDoNotSelectBranch(t *testing.T) {
	for _, command := range []string{
		`if true; then export GH_REPO=example/one; else export GH_REPO=example/two; fi; gh pr view 42`,
		`condition && export GH_REPO=example/one; gh pr view 42`,
		`export GH_REPO=example/one; if condition; then unset GH_REPO; fi; gh pr view 42`,
	} {
		t.Run(command, func(t *testing.T) {
			links, _, err := commandPRLinks(t.Context(), command, "/repo", "https://github.com/example/repo.git")
			require.NoError(t, err)
			assert.Empty(t, links)
		})
	}
}
