package parser

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCommandPRLinks(t *testing.T) {
	remote := "https://github.com/example/repo.git"
	for _, tc := range []struct{ command, remote, want string }{
		{`gh pr view 42; cd /other`, remote, "https://github.com/example/repo/pull/42"},
		{`gh pr view 42; cd /other; gh pr view 43`, remote, "https://github.com/example/repo/pull/42"},
		{`gh pr view 42; git remote set-url origin other`, remote, "https://github.com/example/repo/pull/42"},
		{`env GH_HOST=other.example gh pr view 42 -R example/repo`, remote, ""},
		{`gh pr view 42`, remote, "https://github.com/example/repo/pull/42"},
		{`gh pr merge 12 -R example/repo --squash`, "", "https://github.com/example/repo/pull/12"},
		{`gh pr view --json title 42 --web`, remote, "https://github.com/example/repo/pull/42"},
		{`gh pr comment https://github.com/example/repo/pull/42 --body 'hello'`, "", "https://github.com/example/repo/pull/42"},
		{`gh pr comment --body "$BODY" 42 -R example/repo`, "", "https://github.com/example/repo/pull/42"},
		{`gh pr comment 42 -R example/repo --body "$BODY"`, "", "https://github.com/example/repo/pull/42"},
		{`gh pr comment --body="$BODY" 42 -R example/repo`, "", "https://github.com/example/repo/pull/42"},
		{`gh pr view 42 -R example/repo --future value`, "", "https://github.com/example/repo/pull/42"},
		{`gh pr view 42 --future -R example/repo`, "", ""},
		{`gh pr view 42 --future --json -R example/repo`, remote, ""},
		{`gh pr comment --body 42 -R example/repo`, "", ""},
		{`gh pr comment --body -R example/repo 42`, "", ""},
		{`gh pr view --json=42 -R example/repo`, "", ""},
		{`gh pr view --unknown 42 -R example/repo`, "", ""},
		{`gh pr view 42 $OPTIONS`, remote, ""},
		{`gh pr comment 42 --body=$OPTIONS`, remote, ""},
		{`gh pr comment 42 -b$OPTIONS`, remote, ""},
		{`gh pr comment 42 --body $OPTIONS`, remote, ""},
		{`gh pr comment 42 --body "$@"`, remote, ""},
		{`gh pr comment 42 --body "${OPTIONS[@]}"`, remote, ""},
		{`gh pr view 42 "--repo=$OTHER"`, remote, ""},
		{`gh pr view 42 '-R'"$OTHER"`, remote, ""},
		{`gh pr view --repo="$OTHER" 42`, remote, ""},
		{`command cd /other && gh pr view 42`, remote, ""},
		{`command -- cd /other && gh pr view 42`, remote, ""},
		{`command -p cd /other && gh pr view 42`, remote, ""},
		{`command -v cd && gh pr view 42`, remote, "https://github.com/example/repo/pull/42"},
		{`builtin cd /other && gh pr view 42`, remote, ""},
		{`gh pr co 42 -R example/repo`, "", "https://github.com/example/repo/pull/42"},
		{`gh pr view '#42' -R example/repo`, "", "https://github.com/example/repo/pull/42"},
		{`gh pr update-branch 42 -R example/repo`, "", "https://github.com/example/repo/pull/42"},
		{`gh pr lock 42 -R example/repo`, "", "https://github.com/example/repo/pull/42"},
		{`gh pr unlock 42 -R example/repo`, "", "https://github.com/example/repo/pull/42"},
		{`gh pr revert 42 -R example/repo`, "", "https://github.com/example/repo/pull/42"},
		{`review() { gh pr view 42 -R github.com/example/repo; }; review`, remote, ""},
		{`review() { echo ready; }; review; gh pr view 42 -R github.com/example/repo`, remote, "https://github.com/example/repo/pull/42"},
		{`git -C . remote set-url origin https://github.com/example/other.git; gh pr view 42`, remote, ""},
		{`git -c a=b remote set-url origin https://github.com/example/other.git; gh pr view 42`, remote, ""},
		{`git --git-dir=.git remote set-url origin https://github.com/example/other.git; gh pr view 42`, remote, ""},
		{`git config remote.origin.url https://github.com/example/other.git; gh pr view 42`, remote, ""},
		{`git config --global user.email test@example.com; gh pr view 42`, remote, "https://github.com/example/repo/pull/42"},
		{`gh pr comment 42 -R example/repo --body "$(export GH_HOST=git.example)"`, "", ""},
		{`gh pr comment 42 --body "$(git remote set-url origin other)"`, remote, ""},
		{`git push && gh pr view 42`, remote, "https://github.com/example/repo/pull/42"},
		{`echo $GH_REPO; gh pr view 42`, remote, "https://github.com/example/repo/pull/42"},
		{`change_repo() { export GH_REPO=example/other; }; gh pr view 42`, remote, "https://github.com/example/repo/pull/42"},
		{`change_repo() { export GH_REPO=example/other; }; change_repo; gh pr view 42`, remote, ""},
		{`gh repo set-default --view; gh pr view 42`, remote, "https://github.com/example/repo/pull/42"},
		{`gh repo set-default example/other; gh pr view 42`, remote, ""},
		{`cd repo && gh pr view 42`, remote, ""},
		{`popd; gh pr view 42`, remote, ""},
		{`echo hi | gh pr diff https://github.com/example/repo/pull/42`, remote, "https://github.com/example/repo/pull/42"},
		{`export GH_REPO=example/other; gh pr view 42`, remote, ""},
		{`gh pr view $(echo 42) -R example/repo`, remote, ""},
		{`gh pr create 42 -R example/repo`, remote, ""},
		{`gh pr view branch -R example/repo`, remote, ""},
		{`g''h pr view 43`, remote, "https://github.com/example/repo/pull/43"},
		{`if gh pr view 42 -R github.com/example/repo; then true; fi`, "", "https://github.com/example/repo/pull/42"},
		{`if gh pr checks 44; then gh pr merge 44; fi`, "https://github.com/example/repo.git", "https://github.com/example/repo/pull/44"},
		{`cd /work/other; gh pr view 45 -R example/repo`, "https://github.com/example/repo.git", "https://github.com/example/repo/pull/45"},
		{`gh pr view 42 -R=github.com/example/repo`, "", "https://github.com/example/repo/pull/42"},
		{`gh pr view 42 -R github.com/example/repo`, "", "https://github.com/example/repo/pull/42"},
		{`gh pr view 42 -R git.example/example/repo`, "", "https://git.example/example/repo/pull/42"},
		{`gh pr view 42 -R example/other`, "https://github.com/example/repo.git", "https://github.com/example/other/pull/42"},
		{`GH_HOST=$HOST gh pr view 42 -R github.com/example/repo`, "", "https://github.com/example/repo/pull/42"},
		{`gh pr view -R example/repo 42`, "https://github.com/example/repo.git", "https://github.com/example/repo/pull/42"},
		{`gh pr view 42 -R example/repo`, "", "https://github.com/example/repo/pull/42"},
		{`gh pr view 42 -R example/repo; export GH_HOST=git.example`, "", "https://github.com/example/repo/pull/42"},
		{`GH_HOST=git.example gh pr view 42 -R example/repo`, "https://github.com/example/repo.git", ""},
		{`export GH_HOST=git.example >/bad; gh pr view https://github.com/example/repo/pull/42`, "", "https://github.com/example/repo/pull/42"},
		{`(cd /work/other); gh pr view 42`, remote, ""},
		{`unset GH_REPO; gh pr view 42`, remote, ""},
		{`git config remote.origin.url; gh pr view 42`, remote, "https://github.com/example/repo/pull/42"},
		{`git config --add branch.main.remote upstream; gh pr view 42`, remote, ""},
		{`git config --unset remote.origin.url; gh pr view 42`, remote, ""},
		{`git config set url.https://example.com.insteadOf git@example.com:; gh pr view 42`, remote, ""},
		{`pushd /work/other; gh pr view 42`, remote, ""},
	} {
		t.Run(tc.command, func(t *testing.T) {
			links, err := commandPRLinks(t.Context(), tc.command, tc.remote, true)
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

func commandPRLinks(ctx context.Context, command, remote string, allowBare bool) ([]PRLink, error) {
	links, _, _, _, _, err := AnalyzePRCommand(ctx, command, "", remote, allowBare, false)
	return links, err
}
