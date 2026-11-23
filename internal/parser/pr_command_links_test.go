package parser

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCommandPRLinks(t *testing.T) {
	remote := "https://github.com/example/repo.git"
	for _, tc := range []struct{ command, want string }{
		{`gh pr view 42`, "https://github.com/example/repo/pull/42"},
		{`cd repo && gh pr view 42`, ""},
		{`export GH_HOST=github.com; gh pr view 42 -R example/other`, "https://github.com/example/other/pull/42"},
		{`echo hi | gh pr diff https://github.com/example/repo/pull/42`, "https://github.com/example/repo/pull/42"},
		{`gh pr view 42 --body 'a;b'`, "https://github.com/example/repo/pull/42"},
		{`gh pr view 42 --body "$VAR"`, "https://github.com/example/repo/pull/42"},
		{`gh pr comment 42 --body "$(git config remote.origin.url https://github.com/example/other.git)"`, ""},
		{`gh pr comment 42 -R example/repo --body "$(git config remote.origin.url https://github.com/example/other.git)"`, "https://github.com/example/repo/pull/42"},
		{`export GH_REPO=example/other; gh pr view 42`, ""},
		{`gh pr view $(echo 42) -R example/repo`, ""},
		{`gh pr create 42 -R example/repo`, ""},
		{`gh pr view branch -R example/repo`, ""},
	} {
		t.Run(tc.command, func(t *testing.T) {
			links, err := CommandPRLinks(t.Context(), tc.command, remote, true)
			require.NoError(t, err)
			if tc.want == "" {
				assert.Empty(t, links)
				return
			}
			require.Len(t, links, 1)
			assert.Equal(t, tc.want, links[0].URL)
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := CommandPRLinks(ctx, "gh pr view 42", remote, true)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestCommandPRLinksHostAndFlags(t *testing.T) {
	for _, tc := range []struct{ command, remote, want string }{
		{`gh pr merge 44 --squash --subject "feat(x): y"`, "https://github.com/example/repo.git", "https://github.com/example/repo/pull/44"},
		{`gh pr comment 44 --body "Done (see notes)"`, "https://github.com/example/repo.git", "https://github.com/example/repo/pull/44"},
		{`if gh pr view 42 -R github.com/example/repo; then true; fi`, "", "https://github.com/example/repo/pull/42"},
		{`if gh pr checks 44; then gh pr merge 44; fi`, "https://github.com/example/repo.git", "https://github.com/example/repo/pull/44"},
		{`if gh pr checks 44; then gh pr merge 45 -R example/repo; fi`, "https://github.com/example/repo.git", "https://github.com/example/repo/pull/44"},
		{`if false; then gh pr view 45 -R example/repo; fi`, "https://github.com/example/repo.git", ""},
		{`if true; then gh pr view 45 -R example/repo; else gh pr view 46 -R example/repo; fi`, "https://github.com/example/repo.git", "https://github.com/example/repo/pull/45"},
		{`if false; then true; else gh pr view 46 -R example/repo; fi`, "https://github.com/example/repo.git", "https://github.com/example/repo/pull/46"},
		{`cd /work/other; gh pr view 45 -R example/repo`, "https://github.com/example/repo.git", ""},
		{`cd /work/other; GH_HOST=git.example gh pr view 45 -R example/repo`, "https://github.com/example/repo.git", "https://git.example/example/repo/pull/45"},
		{`gh pr view 42 -R=github.com/example/repo`, "", "https://github.com/example/repo/pull/42"},
		{`export GH_HOST=git.example; gh pr view 42 -R example/repo`, "", "https://git.example/example/repo/pull/42"},
		{`gh pr edit --add-label 99 42`, "https://github.com/example/repo.git", "https://github.com/example/repo/pull/42"},
		{`gh pr view 42 -R github.com/example/repo`, "", "https://github.com/example/repo/pull/42"},
		{`gh pr view 42 -R example/other`, "https://github.com/example/repo.git", "https://github.com/example/other/pull/42"},
		{`GH_HOST=$HOST gh pr view 42 -R github.com/example/repo`, "", "https://github.com/example/repo/pull/42"},
		{`gh pr view -R example/repo 42`, "https://github.com/example/repo.git", "https://github.com/example/repo/pull/42"},
		{`echo hello && gh pr view 42`, "https://github.com/example/repo.git", "https://github.com/example/repo/pull/42"},
		{`gh pr view 42 -R example/repo`, "", ""},
		{`gh pr view 42 -R example/repo; export GH_HOST=git.example`, "", ""},
		{`gh pr view 42 -R example/repo; export GH_HOST=git.example`, "https://github.com/example/repo.git", "https://github.com/example/repo/pull/42"},
		{`GH_HOST=git.example gh pr view 42 -R example/repo`, "https://github.com/example/repo.git", ""},
	} {
		t.Run(tc.command, func(t *testing.T) {
			links, err := CommandPRLinks(t.Context(), tc.command, tc.remote, true)
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

func TestCommandPRLinksInlineHostDoesNotPersist(t *testing.T) {
	links, err := CommandPRLinks(t.Context(), `GH_HOST=git.example gh pr view 42 -R example/repo; gh pr view 43 -R example/repo`, "", true)
	require.NoError(t, err)
	require.Len(t, links, 1)
	assert.Equal(t, "https://git.example/example/repo/pull/42", links[0].URL)
}
