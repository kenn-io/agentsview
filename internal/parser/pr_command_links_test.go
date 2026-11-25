package parser

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCommandPRLinks(t *testing.T) {
	remote := "https://github.com/example/repo.git"
	for _, tc := range []struct {
		command, remote, want, cwd   string
		repositoryChanged, uncertain bool
	}{
		{`command gh pr view 41 -R example/repo`, "", "https://github.com/example/repo/pull/41", "", false, false},
		{`gh -R example/repo pr view 42`, "", "https://github.com/example/repo/pull/42", "", false, false},
		{`bash -lc 'gh pr view 44 -R example/repo'`, "", "https://github.com/example/repo/pull/44", "", false, false},
		{`timeout 60 gh pr checks 45 -R example/repo --watch`, "", "https://github.com/example/repo/pull/45", "", false, false},
		{`bash -lc 'git remote set-url origin https://github.com/user-b/project-b.git'; gh pr view 42`, remote, "", "", true, true},
		{`gh pr view 42; git remote set-url origin other`, remote, "https://github.com/example/repo/pull/42", "", true, true},
		{`env GH_HOST=other.example gh pr view 42 -R example/repo`, remote, "", "", false, false},
		{`env GH_HOST=git.example gh pr view https://github.com/example/repo/pull/42`, remote, "https://github.com/example/repo/pull/42", "", false, false},
		{`env GH_HOST=git.example gh pr view 42 -R github.com/example/repo`, remote, "https://github.com/example/repo/pull/42", "", false, false},
		{`env GH_REPO=other/repo gh pr view https://github.com/example/repo/pull/42`, remote, "https://github.com/example/repo/pull/42", "", false, false},
		{`env GH_HOST=git.example bash -c 'gh pr view 42 -R example/repo'`, remote, "", "", false, true},
		{`bash -n -c 'gh pr view 42 -R example/repo'`, remote, "", "", false, false},
		{`command -v gh pr view 42 -R example/repo`, remote, "", "", false, false},
		{`GH_HOST=git.example command gh pr view 42 -R example/repo`, remote, "", "", false, false},
		{`timeout 60 git remote set-url origin other; gh pr view 42`, remote, "", "", true, true},
		{`bash -c 'export GH_HOST=git.example; gh pr view 42 -R example/repo'`, remote, "", "", false, true},
		{`gh pr merge 12 -R example/repo --squash`, "", "https://github.com/example/repo/pull/12", "", false, false},
		{`gh pr view --json title 42 --web`, remote, "https://github.com/example/repo/pull/42", "", false, false},
		{`gh pr comment https://github.com/example/repo/pull/42 --body 'hello'`, "", "https://github.com/example/repo/pull/42", "", false, false},
		{`gh pr comment --body "$BODY" 42 -R example/repo`, "", "https://github.com/example/repo/pull/42", "", false, false},
		{`gh pr comment 42 -R example/repo --body "$BODY"`, "", "https://github.com/example/repo/pull/42", "", false, false},
		{`gh pr comment --body="$BODY" 42 -R example/repo`, "", "https://github.com/example/repo/pull/42", "", false, false},
		{`gh pr view 42 -R example/repo --future value`, "", "https://github.com/example/repo/pull/42", "", false, false},
		{`gh pr view 42 --future -R example/repo`, "", "", "", false, false},
		{`gh pr view 42 --future --json -R example/repo`, remote, "", "", false, false},
		{`gh pr comment --body 42 -R example/repo`, "", "", "", false, false},
		{`gh pr comment --body -R example/repo 42`, "", "", "", false, false},
		{`gh pr view --json=42 -R example/repo`, "", "", "", false, false},
		{`gh pr view --unknown 42 -R example/repo`, "", "", "", false, false},
		{`gh pr view 42 $OPTIONS`, remote, "", "", false, false},
		{`gh pr comment 42 --body=$OPTIONS`, remote, "", "", false, false},
		{`gh pr comment 42 -b$OPTIONS`, remote, "", "", false, false},
		{`gh pr comment 42 --body $OPTIONS`, remote, "", "", false, false},
		{`gh pr comment 42 --body "$@"`, remote, "", "", false, false},
		{`gh pr comment 42 --body "${OPTIONS[@]}"`, remote, "", "", false, false},
		{`gh pr view 42 "--repo=$OTHER"`, remote, "", "", false, false},
		{`gh pr view 42 '-R'"$OTHER"`, remote, "", "", false, false},
		{`gh pr view --repo="$OTHER" 42`, remote, "", "", false, false},
		{`command cd /other && gh pr view 42`, remote, "", "", false, false},
		{`command -- cd /other && gh pr view 42`, remote, "", "", false, false},
		{`command -p cd /other && gh pr view 42`, remote, "", "", false, false},
		{`command -v cd && gh pr view 42`, remote, "https://github.com/example/repo/pull/42", "", false, false},
		{`builtin cd /other && gh pr view 42`, remote, "", "", false, false},
		{`gh pr co 42 -R example/repo`, "", "https://github.com/example/repo/pull/42", "", false, false},
		{`gh pr view '#42' -R example/repo`, "", "https://github.com/example/repo/pull/42", "", false, false},
		{`gh pr update-branch 42 -R example/repo`, "", "https://github.com/example/repo/pull/42", "", false, false},
		{`gh pr lock 42 -R example/repo`, "", "https://github.com/example/repo/pull/42", "", false, false},
		{`gh pr unlock 42 -R example/repo`, "", "https://github.com/example/repo/pull/42", "", false, false},
		{`gh pr revert 42 -R example/repo`, "", "https://github.com/example/repo/pull/42", "", false, false},
		{`review() { gh pr view 42 -R github.com/example/repo; }; review`, remote, "", "", false, true},
		{`review() { echo ready; }; review; gh pr view 42 -R github.com/example/repo`, remote, "https://github.com/example/repo/pull/42", "", false, true},
		{`git -C . remote set-url origin https://github.com/example/other.git; gh pr view 42`, remote, "", "", true, true},
		{`git -c a=b remote set-url origin https://github.com/example/other.git; gh pr view 42`, remote, "", "", true, true},
		{`git --git-dir=.git remote set-url origin https://github.com/example/other.git; gh pr view 42`, remote, "", "", true, true},
		{`git config remote.origin.url https://github.com/example/other.git; gh pr view 42`, remote, "", "", true, true},
		{`git config --global user.email test@example.com; gh pr view 42`, remote, "https://github.com/example/repo/pull/42", "", false, false},
		{`gh pr comment 42 -R example/repo --body "$(export GH_HOST=git.example)"`, "", "", "", false, true},
		{`gh pr comment 42 --body "$(git remote set-url origin other)"`, remote, "", "", true, true},
		{`git push && gh pr view 42`, remote, "https://github.com/example/repo/pull/42", "", false, false},
		{`echo $GH_REPO; gh pr view 42`, remote, "https://github.com/example/repo/pull/42", "", false, false},
		{`change_repo() { export GH_REPO=example/other; }; gh pr view 42`, remote, "https://github.com/example/repo/pull/42", "", false, false},
		{`change_repo() { export GH_REPO=example/other; }; change_repo; gh pr view 42`, remote, "", "", false, true},
		{`gh repo set-default --view; gh pr view 42`, remote, "https://github.com/example/repo/pull/42", "", false, false},
		{`gh repo set-default example/other; gh pr view 42`, remote, "", "", true, true},
		{`cd repo && gh pr view 42`, remote, "", "", false, false},
		{`popd; gh pr view 42`, remote, "", "", false, false},
		{`echo hi | gh pr diff https://github.com/example/repo/pull/42`, remote, "https://github.com/example/repo/pull/42", "", false, false},
		{`export GH_REPO=example/other; gh pr view 42`, remote, "", "", false, true},
		{`gh pr view $(echo 42) -R example/repo`, remote, "", "", false, false},
		{`gh pr create 42 -R example/repo`, remote, "", "", false, false},
		{`gh pr view branch -R example/repo`, remote, "", "", false, false},
		{`g''h pr view 43`, remote, "https://github.com/example/repo/pull/43", "", false, false},
		{`if gh pr checks 44; then gh pr merge 44; fi`, "https://github.com/example/repo.git", "https://github.com/example/repo/pull/44", "", false, false},
		{`cd /work/other; gh pr view 45 -R example/repo`, "https://github.com/example/repo.git", "https://github.com/example/repo/pull/45", "", false, false},
		{`gh pr view 42 -R=github.com/example/repo`, "", "https://github.com/example/repo/pull/42", "", false, false},
		{`gh pr view 42 -R git.example/example/repo`, "", "https://git.example/example/repo/pull/42", "", false, false},
		{`gh pr view 42 -R example/other`, "https://github.com/example/repo.git", "https://github.com/example/other/pull/42", "", false, false},
		{`GH_HOST=$HOST gh pr view 42 -R github.com/example/repo`, "", "https://github.com/example/repo/pull/42", "", false, false},
		{`gh pr view -R example/repo 42`, "https://github.com/example/repo.git", "https://github.com/example/repo/pull/42", "", false, false},
		{`gh pr view 42 -R example/repo; export GH_HOST=git.example`, "", "https://github.com/example/repo/pull/42", "", false, true},
		{`GH_HOST=git.example gh pr view 42 -R example/repo`, "https://github.com/example/repo.git", "", "", false, false},
		{`GH_HOST=git.example gh pr view 41 -R example/repo; gh pr view 42 -R example/repo`, "", "https://github.com/example/repo/pull/42", "", false, false},
		{`export GH_HOST=git.example >/bad; gh pr view https://github.com/example/repo/pull/42`, "", "https://github.com/example/repo/pull/42", "", false, true},
		{`(cd /work/other); gh pr view 42`, remote, "", "", false, false},
		{`unset GH_REPO; gh pr view 42`, remote, "", "", true, true},
		{`git config remote.origin.url; gh pr view 42`, remote, "https://github.com/example/repo/pull/42", "", false, false},
		{`git config --add branch.main.remote upstream; gh pr view 42`, remote, "", "", true, true},
		{`git config --unset remote.origin.url; gh pr view 42`, remote, "", "", true, true},
		{`git config set url.https://example.com.insteadOf git@example.com:; gh pr view 42`, remote, "", "", true, true},
		{`pushd /work/other; gh pr view 42`, remote, "", "", false, false},
		{`bash -lc 'git remote set-url origin https://github.com/user-b/project-b.git'`, remote, "", "", true, true},
		{`cd /work/repo && gh pr view 42`, remote, "https://github.com/example/repo/pull/42", "/work/repo", false, false},
		{`env GH_PAGER=cat gh pr view 42 -R example/repo`, remote, "https://github.com/example/repo/pull/42", "/work/repo", false, false},
		{`env GH_REPO=other/repo gh pr view 42`, remote, "", "/work/repo", false, false},
		{`env GH_REPO=example/other git remote set-url origin https://github.com/example/other.git; gh pr view 42`, remote, "", "/work/repo", true, true},
		{`env GH_HOST=git.example bash -c 'git remote set-url origin other'; gh pr view 42`, remote, "", "/work/repo", true, true},
		{`f(){ git remote set-url origin https://ghe.example/example/repo.git; }; f`, remote, "", "/work/repo", true, true},
	} {
		t.Run(tc.command, func(t *testing.T) {
			links, _, changed, _, err := AnalyzePRCommand(t.Context(), tc.command, tc.cwd, tc.remote, true, false)
			assert.Equal(t, tc.uncertain, changed)
			assert.Equal(t, tc.repositoryChanged, PRCommandChangesRepository(tc.command))
			require.NoError(t, err)
			if tc.want == "" {
				assert.Empty(t, links)
				return
			}
			require.Len(t, links, 1)
			assert.Equal(t, tc.want, links[0].URL)
		})
	}
	branches, err := commandPRLinks(t.Context(), `if true; then gh pr view 45 -R example/repo; else gh pr view 46 -R example/repo; fi`, "", true)
	require.NoError(t, err)
	require.Len(t, branches, 2)
	assert.Equal(t, 45, branches[0].Number)
	assert.Equal(t, 46, branches[1].Number)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = commandPRLinks(ctx, "gh pr view 42", remote, true)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestCommandPRLinksDirectoryChange(t *testing.T) {
	links, directoryChanged, _, depends, err := AnalyzePRCommand(t.Context(), `gh pr view 42; cd /other; gh pr view 43`, "", "https://github.com/example/repo.git", true, false)
	require.NoError(t, err)
	assert.True(t, directoryChanged)
	require.Len(t, links, 1)
	assert.Equal(t, "https://github.com/example/repo/pull/42", links[0].URL)
	assert.Equal(t, links, depends)
}

func commandPRLinks(ctx context.Context, command, remote string, allowBare bool) ([]PRLink, error) {
	links, _, _, _, err := AnalyzePRCommand(ctx, command, "", remote, allowBare, false)
	return links, err
}
