package parser

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCommandPRLinks(t *testing.T) {
	remote := "https://github.com/example/repo.git"
	for _, tc := range []struct{ command, remote, want string }{
		{`gh pr view 42`, remote, "https://github.com/example/repo/pull/42"},
		{`gh pr view 42; export GH_REPO=example/other`, remote, "https://github.com/example/repo/pull/42"},
		{`gh pr view 42; export GH_REPO=example/other; gh pr view 43`, remote, "https://github.com/example/repo/pull/42"},
		{`gh pr co 42 -R example/repo`, "", "https://github.com/example/repo/pull/42"},
		{`gh pr view '#42' -R example/repo`, "", "https://github.com/example/repo/pull/42"},
		{`gh pr update-branch 42 -R example/repo`, "", "https://github.com/example/repo/pull/42"},
		{`gh pr lock 42 -R example/repo`, "", "https://github.com/example/repo/pull/42"},
		{`gh pr lock --reason resolved 42 -R example/repo`, "", "https://github.com/example/repo/pull/42"},
		{`gh pr unlock 42 -R example/repo`, "", "https://github.com/example/repo/pull/42"},
		{`gh pr revert 42 -R example/repo`, "", "https://github.com/example/repo/pull/42"},
		{`gh pr revert --title revert 42 -R example/repo`, "", "https://github.com/example/repo/pull/42"},
		{`review() { gh pr view 42 -R example/repo; }; echo ready`, "", ""},
		{`review() { gh pr view 42 -R example/repo; }; review`, "", "https://github.com/example/repo/pull/42"},
		{`review() { gh pr view 42; }; review`, remote, "https://github.com/example/repo/pull/42"},
		{`review() { gh pr view 42; }; GH_REPO=example/other review`, remote, "https://github.com/example/other/pull/42"},
		{`review() { gh pr view 42 -R example/repo; }; GH_HOST=git.example review`, remote, "https://git.example/example/repo/pull/42"},
		{`review() { gh pr view 42 -R github.com/example/repo; }; GH_REPO=example/other review`, remote, "https://github.com/example/repo/pull/42"},
		{`GH_REPO=example/other gh pr view 42 -R example/repo`, remote, "https://github.com/example/repo/pull/42"},
		{`GH_REPO=$REPO gh pr view 42`, remote, ""},
		{`GH_REPO=example/other GH_HOST=git.example gh pr view 42`, remote, "https://git.example/example/other/pull/42"},
		{`GH_REPO=example/other /bin/bash -lc 'gh pr view 42'`, remote, "https://github.com/example/other/pull/42"},
		{`GH_REPO=example/repo bash -c 'export GH_REPO=example/other; gh pr view 42'`, remote, ""},
		{`export GH_REPO=old/repo; GH_REPO=example/repo bash -c 'export GH_REPO=example/other; gh pr view 42'`, remote, ""},
		{`GH_REPO=example/repo bash -c 'unset GH_REPO; gh pr view 42'`, remote, ""},
		{`GH_REPO=example/repo bash -c 'export GH_REPO=example/other; gh pr view 42 -R github.com/example/repo'`, remote, "https://github.com/example/repo/pull/42"},
		{`GH_REPO=example/repo bash -c 'GH_REPO=example/other gh pr view 42'`, remote, "https://github.com/example/other/pull/42"},
		{`GH_HOST=github.com bash -c 'export GH_HOST=git.example; gh pr view 42 -R example/repo'`, remote, ""},
		{`GH_HOST=github.com bash -c 'unset GH_HOST; gh pr view 42 -R example/repo'`, remote, ""},
		{`git -C . remote set-url origin https://github.com/example/other.git; gh pr view 42`, remote, ""},
		{`git -c a=b remote set-url origin https://github.com/example/other.git; gh pr view 42`, remote, ""},
		{`git --git-dir=.git remote set-url origin https://github.com/example/other.git; gh pr view 42`, remote, ""},
		{`/bin/bash -lc 'git remote set-url origin https://github.com/example/other.git'; gh pr view 42`, remote, ""},
		{`git config remote.origin.url https://github.com/example/other.git; gh pr view 42`, remote, ""},
		{`git config --global user.email test@example.com; gh pr view 42`, remote, "https://github.com/example/repo/pull/42"},
		{`review() { gh pr view 42; }; cd /other; review`, remote, ""},
		{`review() { git remote set-url origin https://github.com/example/other.git; gh pr view 42; }; review`, remote, ""},
		{`review() { review; gh pr view 42 -R github.com/example/repo; }; review`, "", "https://github.com/example/repo/pull/42"},
		{`gh pr comment 42 -R example/repo --body "$(export GH_HOST=git.example)"`, "", ""},
		{`git push && gh pr view 42`, remote, "https://github.com/example/repo/pull/42"},
		{`ls; gh pr view 42`, remote, "https://github.com/example/repo/pull/42"},
		{`echo $GH_REPO; gh pr view 42`, remote, "https://github.com/example/repo/pull/42"},
		{`change_repo() { export GH_REPO=example/other; }; gh pr view 42`, remote, "https://github.com/example/repo/pull/42"},
		{`if false; then export GH_REPO=example/other; fi; gh pr view 42`, remote, ""},
		{`change_repo() { export GH_REPO=example/other; }; change_repo; gh pr view 42`, remote, ""},
		{`if true; then export GH_REPO=example/other; fi; gh pr view 42`, remote, ""},
		{`gh repo set-default --view; gh pr view 42`, remote, "https://github.com/example/repo/pull/42"},
		{`gh repo set-default example/other; gh pr view 42`, remote, ""},
		{`gh repo set-default example/other; gh pr view 42 -R example/repo`, remote, "https://github.com/example/repo/pull/42"},
		{`cd repo && gh pr view 42`, remote, ""},
		{`popd; gh pr view 42`, remote, ""},
		{`export GH_HOST=github.com; gh pr view 42 -R example/other`, remote, ""},
		{`echo hi | gh pr diff https://github.com/example/repo/pull/42`, remote, "https://github.com/example/repo/pull/42"},
		{`gh pr view 42 --body 'a;b'`, remote, "https://github.com/example/repo/pull/42"},
		{`gh pr view 42 --body "$VAR"`, remote, "https://github.com/example/repo/pull/42"},
		{`gh pr comment 42 --body "$(git config remote.origin.url https://github.com/example/other.git)"`, remote, ""},
		{`gh pr comment 42 -R example/repo --body "$(git config remote.origin.url https://github.com/example/other.git)"`, remote, "https://github.com/example/repo/pull/42"},
		{`export GH_REPO=example/other; gh pr view 42`, remote, ""},
		{`gh pr view $(echo 42) -R example/repo`, remote, ""},
		{`gh pr create 42 -R example/repo`, remote, ""},
		{`gh pr view branch -R example/repo`, remote, ""},
		{`g''h pr view 43`, remote, "https://github.com/example/repo/pull/43"},
		{`gh p'r' view 42`, remote, "https://github.com/example/repo/pull/42"},
		{`gh pr merge 44 --squash --subject "feat(x): y"`, "https://github.com/example/repo.git", "https://github.com/example/repo/pull/44"},
		{`gh pr comment 44 --body "Done (see notes)"`, "https://github.com/example/repo.git", "https://github.com/example/repo/pull/44"},
		{`if gh pr view 42 -R github.com/example/repo; then true; fi`, "", "https://github.com/example/repo/pull/42"},
		{`if gh pr checks 44; then gh pr merge 44; fi`, "https://github.com/example/repo.git", "https://github.com/example/repo/pull/44"},
		{`if false; then gh pr view 45 -R example/repo; fi`, "https://github.com/example/repo.git", "https://github.com/example/repo/pull/45"},
		{`if false; then true; else gh pr view 46 -R example/repo; fi`, "https://github.com/example/repo.git", "https://github.com/example/repo/pull/46"},
		{`cd /work/other; gh pr view 45 -R example/repo`, "https://github.com/example/repo.git", "https://github.com/example/repo/pull/45"},
		{`cd /work/other; GH_HOST=git.example gh pr view 45 -R example/repo`, "https://github.com/example/repo.git", "https://git.example/example/repo/pull/45"},
		{`gh pr view 42 -R=github.com/example/repo`, "", "https://github.com/example/repo/pull/42"},
		{`export GH_HOST=git.example; gh pr view 42 -R example/repo`, "", ""},
		{`gh pr edit --add-label 99 42`, "https://github.com/example/repo.git", "https://github.com/example/repo/pull/42"},
		{`gh pr view 42 -R github.com/example/repo`, "", "https://github.com/example/repo/pull/42"},
		{`gh pr view 42 -R example/other`, "https://github.com/example/repo.git", "https://github.com/example/other/pull/42"},
		{`GH_HOST=$HOST gh pr view 42 -R github.com/example/repo`, "", "https://github.com/example/repo/pull/42"},
		{`gh pr view -R example/repo 42`, "https://github.com/example/repo.git", "https://github.com/example/repo/pull/42"},
		{`echo hello && gh pr view 42`, "https://github.com/example/repo.git", "https://github.com/example/repo/pull/42"},
		{`gh pr view 42 -R example/repo`, "", "https://github.com/example/repo/pull/42"},
		{`gh pr view 42 -R example/repo; export GH_HOST=git.example`, "", "https://github.com/example/repo/pull/42"},
		{`gh pr view 42 -R example/repo; export GH_HOST=git.example`, "https://github.com/example/repo.git", "https://github.com/example/repo/pull/42"},
		{`GH_HOST=git.example gh pr view 42 -R example/repo`, "https://github.com/example/repo.git", "https://git.example/example/repo/pull/42"},
		{`export GH_HOST=git.example >/bad; gh pr view 42 -R example/repo`, "", ""},
		{`export GH_HOST=git.example >/bad; gh pr view 42 -R github.com/example/repo`, "", "https://github.com/example/repo/pull/42"},
		{`export GH_HOST=git.example >/bad; gh pr view https://github.com/example/repo/pull/42`, "", "https://github.com/example/repo/pull/42"},
		{`if true; then export GH_HOST=git.example; fi; gh pr view 42 -R example/repo`, remote, ""},
		{`(cd /work/other); gh pr view 42`, remote, ""},
		{`if false; then cd /work/other; fi; gh pr view 42`, remote, ""},
		{`unset GH_REPO; gh pr view 42`, remote, ""},
		{`GH_HOST=git.example; gh pr view 42 -R example/repo`, remote, ""},
		{`true || export GH_HOST=git.example; gh pr view 42 -R example/repo`, remote, ""},
		{`git config remote.origin.url; gh pr view 42`, remote, "https://github.com/example/repo/pull/42"},
		{`git config --global remote.origin.url; gh pr view 42`, remote, "https://github.com/example/repo/pull/42"},
		{`git config remote.origin.url https://github.com/example/other; gh pr view 42`, remote, ""},
		{`git config --add branch.main.remote upstream; gh pr view 42`, remote, ""},
		{`git config --unset remote.origin.url; gh pr view 42`, remote, ""},
		{`git config set url.https://example.com.insteadOf git@example.com:; gh pr view 42`, remote, ""},
		{`if true; then cd /work/other; fi; gh pr view 42`, remote, ""},
		{`cd $REPO; gh pr view 42`, remote, ""},
		{`cd -P /work/other; gh pr view 42`, remote, ""},
		{`cd -- /work/other; gh pr view 42`, remote, ""},
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
	for _, tc := range []struct {
		command string
		want    int
	}{
		{`cd /work/repo && gh pr view 42`, 1},
		{`cd . && gh pr view 42`, 1},
		{`cd ../repo && gh pr view 42`, 1},
		{`cd ../other && gh pr view 42`, 0},
	} {
		t.Run(tc.command, func(t *testing.T) {
			links, _, _, _, _, err := AnalyzePRCommand(t.Context(), tc.command, "/work/repo", remote, true, false)
			require.NoError(t, err)
			assert.Len(t, links, tc.want)
		})
	}
	branches, err := commandPRLinks(t.Context(), `if true; then gh pr view 45 -R example/repo; else gh pr view 46 -R example/repo; fi`, "", true)
	require.NoError(t, err)
	require.Len(t, branches, 2)
	assert.Equal(t, 45, branches[0].Number)
	assert.Equal(t, 46, branches[1].Number)
	links, err := commandPRLinks(t.Context(), `GH_HOST=git.example gh pr view 42 -R example/repo; gh pr view 43 -R example/repo`, "", true)
	require.NoError(t, err)
	require.Len(t, links, 2)
	assert.Equal(t, "https://git.example/example/repo/pull/42", links[0].URL)
	assert.Equal(t, "https://github.com/example/repo/pull/43", links[1].URL)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = commandPRLinks(ctx, "gh pr view 42", remote, true)
	assert.ErrorIs(t, err, context.Canceled)
}

func commandPRLinks(ctx context.Context, command, remote string, allowBare bool) ([]PRLink, error) {
	links, _, _, _, _, err := AnalyzePRCommand(ctx, command, "", remote, allowBare, false)
	return links, err
}

func TestCommandPRLinksBoundsExpandedShellWork(t *testing.T) {
	command := `heavy() { ` + strings.Repeat(`:; `, 1000) + `}; target() { gh pr view 42 -R github.com/example/repo; }; ` + strings.Repeat(`heavy; `, 100) + `target; gh pr view 44; gh pr view 43 -R github.com/example/repo`
	links, err := commandPRLinks(t.Context(), command, "https://github.com/example/repo.git", true)
	require.NoError(t, err)
	require.Len(t, links, 1)
	assert.Equal(t, 43, links[0].Number)
	command = `heavy() { printf '` + strings.Repeat(`x`, 20000) + `'; }; target() { gh pr view 42 -R github.com/example/repo; }; ` + strings.Repeat(`heavy; `, 10) + `target; gh pr view 43 -R github.com/example/repo`
	links, err = commandPRLinks(t.Context(), command, "", true)
	require.NoError(t, err)
	require.Len(t, links, 1)
	assert.Equal(t, 43, links[0].Number)
	command = `bash -c '` + strings.Repeat(`:;`, 50001) + `git remote set-url origin https://github.com/example/other.git'; gh pr view 42; gh pr view 43 -R github.com/example/repo`
	links, err = commandPRLinks(t.Context(), command, "https://github.com/example/repo.git", true)
	require.NoError(t, err)
	require.Len(t, links, 1)
	assert.Equal(t, 43, links[0].Number)
}
