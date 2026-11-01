package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"hegel.dev/go/hegel"
	"os"
	"path/filepath"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/service"
	"go.kenn.io/agentsview/internal/testutil"
)

type whyServiceStub struct {
	service.SessionService
	lookup func(context.Context, []string) (map[string][]service.CommitSessionLink, error)
}

func (s whyServiceStub) CommitSessions(ctx context.Context, shas []string) (map[string][]service.CommitSessionLink, error) {
	return s.lookup(ctx, shas)
}
func whyCLIRepo(t *testing.T) (string, string) {
	t.Helper()
	t.Setenv("AGENTSVIEW_DATA_DIR", t.TempDir())
	dir, git := testutil.GitRepo(t, "")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "x.go"), []byte("first\n"), 0600))
	git("add", "--", "x.go")
	git("commit", "-m", "Initial")
	return dir, git("rev-parse", "HEAD")
}
func TestWhyCLICommitNoSync(t *testing.T) {
	dir, sha := whyCLIRepo(t)
	t.Chdir(dir)
	resolved, cleaned := 0, 0
	cmd := newWhyCommandWithServiceResolver(func(cmd *cobra.Command) (service.SessionService, func(), error) {
		resolved++
		return whyServiceStub{lookup: func(ctx context.Context, shas []string) (map[string][]service.CommitSessionLink, error) {
			assert.Equal(t, []string{sha}, shas)
			_, hasDeadline := ctx.Deadline()
			assert.True(t, hasDeadline)
			return map[string][]service.CommitSessionLink{sha: {{SessionID: "codex:example", CommitSHA: sha, Source: "transcript", Session: &service.CommitSession{ID: "codex:example"}}}}, nil
		}}, func() { cleaned++ }, nil
	})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--commit", sha[:8], "--format", "json", "--no-sync"})
	require.NoError(t, cmd.Execute())
	assert.Equal(t, 1, resolved)
	assert.Equal(t, 1, cleaned)
	var result map[string]any
	require.NoError(t, json.Unmarshal(out.Bytes(), &result))
	assert.Equal(t, sha, result["question"].(map[string]any)["commit"])
	assert.Equal(t, true, result["explained"])
}
func TestWhyCLIDirtySkipsServiceAndValidatesConflicts(t *testing.T) {
	dir, _ := whyCLIRepo(t)
	t.Chdir(dir)
	require.NoError(t, os.WriteFile("x.go", []byte("dirty\n"), 0600))
	resolver := func(*cobra.Command) (service.SessionService, func(), error) {
		t.Fatal("must not resolve service")
		return nil, nil, nil
	}
	cmd := newWhyCommandWithServiceResolver(resolver)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"x.go", "--json"})
	require.NoError(t, cmd.Execute())
	assert.Contains(t, out.String(), `"sha":null`)
	cmd = newWhyCommandWithServiceResolver(resolver)
	cmd.SetArgs([]string{"x.go", "--server", "http://127.0.0.1:1", "--pg"})
	assert.ErrorContains(t, cmd.Execute(), "mutually exclusive")
	cmd = newWhyCommandWithServiceResolver(resolver)
	cmd.SetArgs([]string{"x.go:0"})
	assert.Error(t, cmd.Execute())
}
func TestWhyCLIErrorIsTerminalSafeAndPreservesCause(t *testing.T) {
	dir, sha := whyCLIRepo(t)
	t.Chdir(dir)
	sentinel := errors.New("remote\x1b]0;title\x07\nfailed")
	cmd := newWhyCommandWithServiceResolver(func(*cobra.Command) (service.SessionService, func(), error) { return nil, nil, sentinel })
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--commit", sha, "--json"})
	err := cmd.Execute()
	require.ErrorIs(t, err, sentinel)
	assert.NotContains(t, err.Error(), "\x1b")
	assert.NotContains(t, err.Error(), "\n")
	assert.Empty(t, out.String())
}

func TestWhyReadServiceDoesNotAutostartAndReadsSQLite(t *testing.T) {
	dir, sha := whyCLIRepo(t)
	t.Chdir(dir)
	dataDir := os.Getenv("AGENTSVIEW_DATA_DIR")
	seedSessionArchiveRows(t, dataDir, sessionSeed{id: "fixture-session", project: "fixture-project"})
	d, err := db.Open(t.Context(), sessionsDBPath(dataDir))
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	require.NoError(t, d.StoreCommitLinks(t.Context(), []db.CommitLink{{
		SessionID: "fixture-session", CommitSHA: sha, Resolved: true, Source: "trailer",
	}}))
	require.NoError(t, d.RefreshCommitLinks(t.Context(), "fixture-session"))
	require.NoError(t, d.Close())
	for _, name := range []string{"config.toml", "telemetry-install-id", "telemetry-install-created"} {
		require.NoFileExists(t, filepath.Join(dataDir, name))
	}
	forbidStartBackgroundServeForTransport(t, "why must not start or replace a daemon")
	cmd := newWhyCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--commit", sha, "--json"})
	require.NoError(t, cmd.ExecuteContext(t.Context()))
	var result struct {
		Explained bool `json:"explained"`
		Refs      []struct {
			Card service.CommitSessionLink `json:"card"`
		} `json:"refs"`
	}
	require.NoError(t, json.Unmarshal(out.Bytes(), &result))
	assert.True(t, result.Explained)
	require.Len(t, result.Refs, 1)
	assert.Equal(t, "fixture-session", result.Refs[0].Card.SessionID)
	assert.Equal(t, "fixture-project", result.Refs[0].Card.Project)
	assert.Equal(t, sha, result.Refs[0].Card.CommitSHA)
	for _, name := range []string{"config.toml", "telemetry-install-id", "telemetry-install-created"} {
		assert.NoFileExists(t, filepath.Join(dataDir, name))
	}
}

func TestWhyCLIFlagErrorsAreTerminalSafe(t *testing.T) {
	for _, arg := range []string{"--oops\x1b[2J", "--format=bad\nvalue", "--unknown\u009b\u202e", "--unknown\xff"} {
		cmd := newWhyCommand()
		cmd.SetArgs([]string{arg})
		cmd.SetErr(&bytes.Buffer{})
		err := cmd.Execute()
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "\x1b")
		assert.NotContains(t, err.Error(), "\n")
		assert.NotContains(t, err.Error(), "\u009b")
		assert.NotContains(t, err.Error(), "\u202e")
		assert.NotContains(t, err.Error(), "\xff")
	}
}

func TestWhyCLIArbitraryFlagErrorsAreSafe(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		text := hegel.Draw(ht, hegel.Text().MaxSize(300))
		prefix := hegel.Draw(ht, hegel.SampledFrom([]string{"--unknown-", "-X", "--format="}))
		cmd := newWhyCommand()
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetArgs([]string{prefix + text})
		err := cmd.Execute()
		require.Error(ht, err)
		assert.True(ht, utf8.ValidString(err.Error()))
		for _, r := range err.Error() {
			assert.False(ht, unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp), "unsafe error rune %U", r)
		}
		assert.NotNil(ht, errors.Unwrap(err))
	}, hegel.WithTestCases(2000))
}

type whyPartialWriter struct{ err error }

func (w whyPartialWriter) Write(p []byte) (int, error) { return min(1, len(p)), w.err }
func TestWhyCLIJSONWriteFailurePreservesCause(t *testing.T) {
	dir, sha := whyCLIRepo(t)
	t.Chdir(dir)
	sentinel := errors.New("consumer closed output")
	cmd := newWhyCommandWithServiceResolver(func(*cobra.Command) (service.SessionService, func(), error) {
		return whyServiceStub{lookup: func(context.Context, []string) (map[string][]service.CommitSessionLink, error) {
			return map[string][]service.CommitSessionLink{sha: {}}, nil
		}}, func() {}, nil
	})
	cmd.SetOut(whyPartialWriter{err: sentinel})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--commit", sha, "--json"})
	assert.ErrorIs(t, cmd.Execute(), sentinel)
}
