package why

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/testutil"
)

func fixtureRepo(t *testing.T) (string, func(...string) string) {
	t.Helper()
	return testutil.GitRepo(t, "")
}

func fixtureRepoAt(t *testing.T, dir string) (string, func(...string) string) {
	t.Helper()
	return testutil.GitRepo(t, dir)
}

func TestInspectCommitPrefixIgnoresHexNamedTag(t *testing.T) {
	dir, git := fixtureRepo(t)
	writeFixture(t, dir, "x.go", "first\n")
	git("add", "--all")
	git("commit", "-m", "First")
	first := git("rev-parse", "HEAD")
	writeFixture(t, dir, "x.go", "second\n")
	git("add", "--all")
	git("commit", "-m", "Second")
	second := git("rev-parse", "HEAD")
	git("tag", first[:8], second)

	got, err := Inspect(t.Context(), dir, Request{Commit: first[:8]})
	require.NoError(t, err)
	assert.Equal(t, first, got.Question.Commit)
}

func TestInspectPreservesCarriageReturnInCheckoutRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows paths cannot contain carriage returns")
	}
	dir := filepath.Join(t.TempDir(), "repo\r")
	dir, git := fixtureRepoAt(t, dir)
	writeFixture(t, dir, "x.go", "first\n")
	git("add", "--all")
	git("commit", "-m", "Initial")

	got, err := Inspect(t.Context(), dir, Request{Path: "x.go"})
	require.NoError(t, err)
	require.Len(t, got.Lines, 1)
	assert.Equal(t, "first", got.Lines[0].Text)
}

func writeFixture(t *testing.T, dir, path, text string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(dir, path)), 0700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, path), []byte(text), 0600))
}

func TestInspectWorkingTreeAndRevision(t *testing.T) {
	dir, git := fixtureRepo(t)
	for _, name := range []string{"x.go", "-notes.md", "notes:12", "子.go", "deleted/file.go"} {
		writeFixture(t, dir, name, "first\nsecond\n")
	}
	writeFixture(t, dir, "empty.txt", "")
	git("add", "--all")
	git("commit", "-m", "Initial\n\nSee docs/guide.md and project#abcd")
	first := git("rev-parse", "HEAD")
	writeFixture(t, dir, "x.go", "first\nchanged\n")
	got, err := Inspect(t.Context(), dir, Request{Path: "x.go", Start: 1, End: 2})
	require.NoError(t, err)
	require.Len(t, got.Lines, 2)
	assert.Equal(t, first, *got.Lines[0].SHA)
	assert.Nil(t, got.Lines[1].SHA)
	assert.Equal(t, 1, got.Commits[0].Covered)
	assert.Equal(t, "WORKTREE", got.Question.Rev)
	got, err = Inspect(t.Context(), dir, Request{Path: "x.go", Rev: "HEAD"})
	require.NoError(t, err)
	require.Len(t, got.Lines, 2)
	assert.Equal(t, "second", got.Lines[1].Text)
	for _, name := range []string{"-notes.md", "notes:12", "子.go", "empty.txt"} {
		got, err := Inspect(t.Context(), dir, Request{Path: name})
		require.NoError(t, err)
		assert.Equal(t, name, got.Question.Path)
	}
	require.NoError(t, os.RemoveAll(filepath.Join(dir, "deleted")))
	_, err = Inspect(t.Context(), dir, Request{Path: "deleted/file.go", Rev: first})
	require.NoError(t, err)
	got, err = Inspect(t.Context(), dir, Request{Commit: first[:8]})
	require.NoError(t, err)
	assert.Empty(t, got.Lines)
	require.Len(t, got.Commits, 1)
	assert.Equal(t, first, got.Question.Commit)
	for _, req := range []Request{{Path: "x.go", Start: 1, End: 3}, {Path: "../escape"}, {Path: "x.go", Rev: "--output=oops"}, {Commit: strings.Repeat("f", 40)}} {
		_, err = Inspect(t.Context(), dir, req)
		assert.Error(t, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = Inspect(ctx, dir, Request{Path: "x.go"})
	assert.ErrorIs(t, err, context.Canceled)
}

func TestInspectLimitsAndHelpers(t *testing.T) {
	dir, git := fixtureRepo(t)
	writeFixture(t, dir, "x.go", "first\n")
	writeFixture(t, dir, ".gitattributes", "*.go filter=marker diff=marker\n")
	git("add", "--all")
	git("commit", "-m", "Initial")
	marker := filepath.Join(dir, "marker")
	helper := "printf marker > '" + filepath.ToSlash(marker) + "'; cat"
	git("config", "filter.marker.clean", helper)
	git("config", "filter.marker.process", helper)
	git("config", "filter.marker.required", "true")
	git("config", "diff.marker.textconv", helper)
	git("config", "core.fsmonitor", helper)
	got, err := Inspect(t.Context(), dir, Request{Path: "x.go"})
	require.NoError(t, err)
	require.Len(t, got.Lines, 1)
	_, err = os.Stat(marker)
	assert.True(t, os.IsNotExist(err), "helper ran")
	writeFixture(t, dir, "x.go", strings.Repeat("x", maxFileBytes+1))
	_, err = Inspect(t.Context(), dir, Request{Path: "x.go", Start: 1, End: 1})
	require.ErrorContains(t, err, "file exceeds")
}

func TestInspectRejectsExternalSymlink(t *testing.T) {
	dir, git := fixtureRepo(t)
	writeFixture(t, dir, "x.go", "first\n")
	git("add", "--all")
	git("commit", "-m", "Initial")
	outside := t.TempDir()
	writeFixture(t, outside, "secret.go", "outside\n")
	if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
		t.Skip("symlinks unavailable")
	}
	_, err := Inspect(t.Context(), dir, Request{Path: "escape/secret.go"})
	require.Error(t, err)
}

func TestInspectIgnoresConfiguredAttributionOverrides(t *testing.T) {
	dir, git := fixtureRepo(t)
	writeFixture(t, dir, "x.go", "first\n")
	git("add", "--all")
	git("commit", "-m", "Initial")
	writeFixture(t, dir, "x.go", "second\n")
	git("add", "--all")
	git("commit", "-m", "Changed")
	changed := git("rev-parse", "HEAD")
	writeFixture(t, dir, "ignored", changed+"\n")
	git("config", "blame.ignoreRevsFile", "ignored")
	got, err := Inspect(t.Context(), dir, Request{Path: "x.go"})
	require.NoError(t, err)
	require.Len(t, got.Lines, 1)
	require.NotNil(t, got.Lines[0].SHA)
	assert.Equal(t, changed, *got.Lines[0].SHA)
}

func TestInspectMissingPartialCloneObjectDoesNotFetch(t *testing.T) {
	dir, git := fixtureRepo(t)
	writeFixture(t, dir, "x.go", "first\n")
	git("add", "--all")
	git("commit", "-m", "Initial")
	oid := git("rev-parse", "HEAD:x.go")
	require.NoError(t, os.Remove(filepath.Join(dir, ".git", "objects", oid[:2], oid[2:])))
	marker := filepath.ToSlash(filepath.Join(dir, "fetched"))
	git("config", "remote.origin.promisor", "true")
	git("config", "remote.origin.partialclonefilter", "blob:none")
	git("config", "remote.origin.url", "ext::sh -c 'echo fetched > "+marker+"'")
	git("config", "protocol.ext.allow", "always")
	_, err := Inspect(t.Context(), dir, Request{Path: "x.go", Rev: "HEAD"})
	require.Error(t, err)
	_, err = os.Stat(filepath.Join(dir, "fetched"))
	assert.True(t, os.IsNotExist(err), "lazy fetch ran")
}

func TestInspectLineLimitAndPinnedDocuments(t *testing.T) {
	dir, git := fixtureRepo(t)
	writeFixture(t, dir, "x.go", strings.Repeat("line\n", 401))
	writeFixture(t, dir, "docs/guide.md", "guide\n")
	git("add", "--all")
	git("commit", "-m", "Initial")
	_, err := Inspect(t.Context(), dir, Request{Path: "x.go"})
	require.ErrorContains(t, err, "400")
	got, err := Inspect(t.Context(), dir, Request{Path: "x.go", Start: 1, End: 400})
	require.NoError(t, err)
	require.Len(t, got.Lines, 400)
	require.NoError(t, os.Remove(filepath.Join(dir, "docs", "guide.md")))
	yes, err := got.DocumentExists(t.Context(), "docs/guide.md")
	require.NoError(t, err)
	assert.True(t, yes)
	yes, err = got.DocumentExists(t.Context(), "docs/missing.md")
	require.NoError(t, err)
	assert.False(t, yes)
}

func TestInspectNeverRunsSignatureHelper(t *testing.T) {
	dir, git := fixtureRepo(t)
	writeFixture(t, dir, "x.go", "first\n")
	git("add", "--all")
	git("commit", "-m", "Initial")
	tree := git("write-tree")
	raw := "tree " + tree + "\nauthor Example <example@example.invalid> 1 +0000\ncommitter Example <example@example.invalid> 1 +0000\ngpgsig -----BEGIN PGP SIGNATURE-----\n fixture\n -----END PGP SIGNATURE-----\n\nSigned fixture\n"
	writeFixture(t, dir, "signed-commit", raw)
	sha := git("hash-object", "-t", "commit", "-w", "signed-commit")
	git("update-ref", "HEAD", sha)
	marker := filepath.Join(dir, "signature-helper-ran")
	helper := filepath.Join(dir, "verifier.sh")
	require.NoError(t, os.WriteFile(helper, []byte("#!/bin/sh\nprintf marker > '"+filepath.ToSlash(marker)+"'\nexit 1\n"), 0700))
	git("config", "log.showSignature", "true")
	git("config", "gpg.program", filepath.ToSlash(helper))
	for _, req := range []Request{{Commit: sha}, {Path: "x.go"}} {
		_, err := Inspect(t.Context(), dir, req)
		require.NoError(t, err)
		_, err = os.Stat(marker)
		assert.True(t, os.IsNotExist(err), "signature verifier ran")
	}
}
func TestInspectCwdAliasMatchesPhysicalCheckout(t *testing.T) {
	dir, git := fixtureRepo(t)
	writeFixture(t, dir, "gone/x.go", "first\n")
	git("add", "--all")
	git("commit", "-m", "Initial")
	sha := git("rev-parse", "HEAD")
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(dir, alias); err != nil {
		t.Skip("directory symlinks unavailable")
	}
	for _, req := range []Request{{Path: "gone/x.go", Start: 1, End: 1}, {Path: "gone/x.go", Rev: sha}} {
		physical, err := Inspect(t.Context(), dir, req)
		require.NoError(t, err)
		got, err := Inspect(t.Context(), alias, req)
		require.NoError(t, err)
		assert.Equal(t, physical.Question, got.Question)
		assert.Equal(t, physical.Lines, got.Lines)
		req.Path = filepath.Join(alias, "gone", "x.go")
		for _, cwd := range []string{dir, alias} {
			got, err := Inspect(t.Context(), cwd, req)
			require.NoError(t, err)
			assert.Equal(t, physical.Question, got.Question)
			assert.Equal(t, physical.Lines, got.Lines)
		}
	}
	require.NoError(t, os.RemoveAll(filepath.Join(dir, "gone")))
	_, err := Inspect(t.Context(), alias, Request{Path: "gone/x.go", Rev: sha})
	require.NoError(t, err)
	got, err := Inspect(t.Context(), dir, Request{Path: filepath.Join(alias, "gone", "x.go"), Rev: sha})
	require.NoError(t, err)
	assert.Equal(t, "gone/x.go", got.Question.Path)
	require.Len(t, got.Lines, 1)
	assert.Equal(t, "first", got.Lines[0].Text)
	require.NoError(t, os.Symlink(dir, filepath.Join(dir, "gone")))
	for _, path := range []string{"gone/x.go", filepath.Join(alias, "gone", "x.go")} {
		got, err := Inspect(t.Context(), dir, Request{Path: path, Rev: sha})
		require.NoError(t, err)
		assert.Equal(t, "gone/x.go", got.Question.Path)
	}
}
