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
		command, remote, want, cwd string
	}{
		{`bash -c 'export GH_HOST=git.example'; gh pr view 42`, remote, "https://github.com/example/repo/pull/42", "/work/repo"},
		{`bash -c 'export GH_HOST=git.example'; gh pr view 42 -R example/repo`, remote, "https://github.com/example/repo/pull/42", ""},
		{`export GH_HOST=$HOST; gh pr view 42 -R example/repo`, remote, "", ""},
		{`setHost() { export GH_HOST=git.example; }; setHost; gh pr view 42 -R example/repo`, remote, "", ""},
		{`setHost() { export GH_HOST=git.example; }; gh pr view 42 -R example/repo`, remote, "https://github.com/example/repo/pull/42", ""},
		{`setHost() { export GH_HOST=git.example; }; setHost; gh pr view 42 -R github.com/example/repo`, remote, "https://github.com/example/repo/pull/42", ""},
		{`gh pr view 42; cd /other; gh pr view 43`, remote, "", ""},
		{`command gh pr view 41 -R example/repo`, "", "https://github.com/example/repo/pull/41", ""},
		{`gh -R example/repo pr view 42`, "", "https://github.com/example/repo/pull/42", ""},
		{`bash -lc 'gh pr view 44 -R example/repo'`, "", "https://github.com/example/repo/pull/44", ""},
		{`timeout 60 gh pr checks 45 -R example/repo --watch`, "", "https://github.com/example/repo/pull/45", ""},
		{`bash -lc 'git remote set-url origin https://github.com/user-b/project-b.git'; gh pr view 42`, remote, "", ""},
		{`gh pr view 42; git remote set-url origin other`, remote, "", ""},
		{`env GH_HOST=other.example gh pr view 42 -R example/repo`, remote, "", ""},
		{`env GH_HOST=git.example gh pr view https://github.com/example/repo/pull/42`, remote, "https://github.com/example/repo/pull/42", ""},
		{`env GH_HOST=git.example gh pr view 42 -R github.com/example/repo`, remote, "https://github.com/example/repo/pull/42", ""},
		{`env GH_REPO=other/repo gh pr view https://github.com/example/repo/pull/42`, remote, "https://github.com/example/repo/pull/42", ""},
		{`env GH_HOST=git.example bash -c 'gh pr view 42 -R example/repo'`, remote, "", ""},
		{`bash -n -c 'gh pr view 42 -R example/repo'`, remote, "", ""},
		{`command -v gh pr view 42 -R example/repo`, remote, "", ""},
		{`GH_HOST=git.example command gh pr view 42 -R example/repo`, remote, "", ""},
		{`timeout 60 git remote set-url origin other; gh pr view 42`, remote, "", ""},
		{`bash -c 'export GH_HOST=git.example; gh pr view 42 -R example/repo'`, remote, "", ""},
		{`gh pr merge 12 -R example/repo --squash`, "", "https://github.com/example/repo/pull/12", ""},
		{`gh pr view --json title 42 --web`, remote, "https://github.com/example/repo/pull/42", ""},
		{`gh pr comment https://github.com/example/repo/pull/42 --body 'hello'`, "", "https://github.com/example/repo/pull/42", ""},
		{`gh pr comment --body "$BODY" 42 -R example/repo`, "", "https://github.com/example/repo/pull/42", ""},
		{`gh pr comment 42 -R example/repo --body "$BODY"`, "", "https://github.com/example/repo/pull/42", ""},
		{`gh pr comment --body="$BODY" 42 -R example/repo`, "", "https://github.com/example/repo/pull/42", ""},
		{`gh pr view 42 -R example/repo --future value`, "", "https://github.com/example/repo/pull/42", ""},
		{`gh pr view 42 --future -R example/repo`, "", "", ""},
		{`gh pr view 42 --future --json -R example/repo`, remote, "", ""},
		{`gh pr comment --body 42 -R example/repo`, "", "", ""},
		{`gh pr comment --body -R example/repo 42`, "", "", ""},
		{`gh pr view --json=42 -R example/repo`, "", "", ""},
		{`gh pr view --unknown 42 -R example/repo`, "", "", ""},
		{`gh pr view 42 $OPTIONS`, remote, "", ""},
		{`gh pr comment 42 --body=$OPTIONS`, remote, "", ""},
		{`gh pr comment 42 -b$OPTIONS`, remote, "", ""},
		{`gh pr comment 42 --body $OPTIONS`, remote, "", ""},
		{`gh pr comment 42 --body "$@"`, remote, "", ""},
		{`gh pr comment 42 --body "${OPTIONS[@]}"`, remote, "", ""},
		{`gh pr view 42 "--repo=$OTHER"`, remote, "", ""},
		{`gh pr view 42 '-R'"$OTHER"`, remote, "", ""},
		{`gh pr view --repo="$OTHER" 42`, remote, "", ""},
		{`command cd /other && gh pr view 42`, remote, "", ""},
		{`command -- cd /other && gh pr view 42`, remote, "", ""},
		{`command -p cd /other && gh pr view 42`, remote, "", ""},
		{`command -v cd && gh pr view 42`, remote, "https://github.com/example/repo/pull/42", ""},
		{`builtin cd /other && gh pr view 42`, remote, "", ""},
		{`gh pr co 42 -R example/repo`, "", "https://github.com/example/repo/pull/42", ""},
		{`gh pr view '#42' -R example/repo`, "", "https://github.com/example/repo/pull/42", ""},
		{`gh pr update-branch 42 -R example/repo`, "", "https://github.com/example/repo/pull/42", ""},
		{`gh pr lock 42 -R example/repo`, "", "https://github.com/example/repo/pull/42", ""},
		{`gh pr unlock 42 -R example/repo`, "", "https://github.com/example/repo/pull/42", ""},
		{`gh pr revert 42 -R example/repo`, "", "https://github.com/example/repo/pull/42", ""},
		{`review() { gh pr view 42 -R github.com/example/repo; }; review`, remote, "", ""},
		{`review() { echo ready; }; review; gh pr view 42 -R github.com/example/repo`, remote, "https://github.com/example/repo/pull/42", ""},
		{`git -C . remote set-url origin https://github.com/example/other.git; gh pr view 42`, remote, "", ""},
		{`git -c a=b remote set-url origin https://github.com/example/other.git; gh pr view 42`, remote, "", ""},
		{`git --git-dir=.git remote set-url origin https://github.com/example/other.git; gh pr view 42`, remote, "", ""},
		{`git config remote.origin.url https://github.com/example/other.git; gh pr view 42`, remote, "", ""},
		{`git config --global core.editor vim; gh pr view 42`, remote, "https://github.com/example/repo/pull/42", ""},
		{`gh pr comment 42 -R example/repo --body "$(export GH_HOST=git.example)"`, "", "", ""},
		{`gh pr comment 42 --body "$(git remote set-url origin other)"`, remote, "", ""},
		{`git push && gh pr view 42`, remote, "https://github.com/example/repo/pull/42", ""},
		{`echo $GH_REPO; gh pr view 42`, remote, "https://github.com/example/repo/pull/42", ""},
		{`change_repo() { export GH_REPO=example/other; }; gh pr view 42`, remote, "https://github.com/example/repo/pull/42", ""},
		{`change_repo() { export GH_REPO=example/other; }; change_repo; gh pr view 42`, remote, "", ""},
		{`gh repo set-default --view; gh pr view 42`, remote, "https://github.com/example/repo/pull/42", ""},
		{`gh repo set-default example/other; gh pr view 42`, remote, "", ""},
		{`cd repo && gh pr view 42`, remote, "", ""},
		{`popd; gh pr view 42`, remote, "", ""},
		{`echo hi | gh pr diff https://github.com/example/repo/pull/42`, remote, "https://github.com/example/repo/pull/42", ""},
		{`export GH_REPO=example/other; gh pr view 42`, remote, "", ""},
		{`gh pr view $(echo 42) -R example/repo`, remote, "", ""},
		{`gh pr create 42 -R example/repo`, remote, "", ""},
		{`gh pr view branch -R example/repo`, remote, "", ""},
		{`g''h pr view 43`, remote, "https://github.com/example/repo/pull/43", ""},
		{`if gh pr checks 44; then gh pr merge 44; fi`, "https://github.com/example/repo.git", "https://github.com/example/repo/pull/44", ""},
		{`cd /work/other; gh pr view 45 -R example/repo`, "https://github.com/example/repo.git", "https://github.com/example/repo/pull/45", ""},
		{`gh pr view 42 -R=github.com/example/repo`, "", "https://github.com/example/repo/pull/42", ""},
		{`gh pr view 42 -R git.example/example/repo`, "", "https://git.example/example/repo/pull/42", ""},
		{`gh pr view 42 -R example/other`, "https://github.com/example/repo.git", "https://github.com/example/other/pull/42", ""},
		{`GH_HOST=$HOST gh pr view 42 -R github.com/example/repo`, "", "https://github.com/example/repo/pull/42", ""},
		{`gh pr view -R example/repo 42`, "https://github.com/example/repo.git", "https://github.com/example/repo/pull/42", ""},
		{`gh pr view 42 -R example/repo; export GH_HOST=git.example`, "", "", ""},
		{`GH_HOST=git.example gh pr view 42 -R example/repo`, "https://github.com/example/repo.git", "", ""},
		{`GH_HOST=git.example gh pr view 41 -R example/repo; gh pr view 42 -R example/repo`, "", "", ""},
		{`export GH_HOST=git.example >/bad; gh pr view https://github.com/example/repo/pull/42`, "", "https://github.com/example/repo/pull/42", ""},
		{`(cd /work/other); gh pr view 42`, remote, "", ""},
		{`unset GH_REPO; gh pr view 42`, remote, "", ""},
		{`git config remote.origin.url; gh pr view 42`, remote, "https://github.com/example/repo/pull/42", ""},
		{`git config --add branch.main.remote upstream; gh pr view 42`, remote, "", ""},
		{`git config --unset remote.origin.url; gh pr view 42`, remote, "", ""},
		{`git config set url.https://example.com.insteadOf git@example.com:; gh pr view 42`, remote, "", ""},
		{`pushd /work/other; gh pr view 42`, remote, "", ""},
		{`cd /work/repo && gh pr view 42`, remote, "", "/work/repo"},
		{`env GH_PAGER=cat gh pr view 42 -R example/repo`, remote, "https://github.com/example/repo/pull/42", "/work/repo"},
		{`env GH_REPO=other/repo gh pr view 42`, remote, "", "/work/repo"},
		{`env GH_REPO=example/other git remote set-url origin https://github.com/example/other.git; gh pr view 42`, remote, "", "/work/repo"},
		{`env GH_HOST=git.example bash -c 'git remote set-url origin other'; gh pr view 42`, remote, "", "/work/repo"},
	} {
		t.Run(tc.command, func(t *testing.T) {
			links, err := AnalyzePRCommand(t.Context(), tc.command, tc.cwd, tc.remote, true)
			require.NoError(t, err)
			if tc.want == "" {
				assert.Empty(t, links)
				return
			}
			require.Len(t, links, 1)
			assert.Equal(t, tc.want, links[0].URL)
		})
	}
	branches, err := AnalyzePRCommand(t.Context(), `if true; then gh pr view 45 -R example/repo; else gh pr view 46 -R example/repo; fi`, "", "", true)
	require.NoError(t, err)
	require.Len(t, branches, 2)
	assert.Equal(t, 45, branches[0].Number)
	assert.Equal(t, 46, branches[1].Number)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = AnalyzePRCommand(ctx, "gh pr view 42", "", remote, true)
	assert.ErrorIs(t, err, context.Canceled)
}
