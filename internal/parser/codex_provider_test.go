package parser

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestCodexProviderSourceMethods(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	base := t.TempDir()
	root := filepath.Join(base, "sessions")
	uuid := "019eb791-cf7d-75c1-8439-9ed74c1229e1"
	sourcePath := writeCodexProviderSession(t, root, uuid, "Rename me")
	indexPath := filepath.Join(base, CodexSessionIndexFilename)
	require.NoError(os.WriteFile(indexPath, []byte(
		`{"id":"`+uuid+`","thread_name":"Renamed title","updated_at":"2026-06-11T17:34:20Z"}`+"\n",
	), 0o644))
	newer := time.Now().Add(time.Hour)
	require.NoError(os.Chtimes(indexPath, newer, newer))

	provider, ok := NewProvider(AgentCodex, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(ok)

	plan, err := provider.WatchPlan(t.Context())
	require.NoError(err)
	require.Len(plan.Roots, 2)
	assert.Equal(root, plan.Roots[0].Path)
	assert.True(plan.Roots[0].Recursive)
	assert.Equal([]string{"*.jsonl"}, plan.Roots[0].IncludeGlobs)
	assert.Equal(base, plan.Roots[1].Path)
	assert.False(plan.Roots[1].Recursive)
	assert.Equal([]string{CodexSessionIndexFilename}, plan.Roots[1].IncludeGlobs)

	discovered, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(discovered, 1)
	source := discovered[0]
	assert.Equal(AgentCodex, source.Provider)
	assert.Equal(sourcePath, source.DisplayPath)
	assert.Equal(sourcePath, source.FingerprintKey)

	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		FullSessionID: "host~codex:" + uuid,
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(sourcePath, found.DisplayPath)

	for _, path := range []string{sourcePath, indexPath} {
		changed, err := provider.SourcesForChangedPath(
			t.Context(),
			ChangedPathRequest{Path: path, EventKind: "write"},
		)
		require.NoError(err)
		require.Len(changed, 1)
		assert.Equal(sourcePath, changed[0].DisplayPath)
	}

	info, err := os.Stat(sourcePath)
	require.NoError(err)
	fingerprint, err := provider.Fingerprint(t.Context(), found)
	require.NoError(err)
	assert.Equal(sourcePath, fingerprint.Key)
	assert.Equal(info.Size(), fingerprint.Size)
	assert.Equal(newer.UnixNano(), fingerprint.MTimeNS)
	wantInode, wantDevice := sourceFileIdentityForPath(sourcePath, info)
	assert.Equal(wantInode, fingerprint.Inode)
	assert.Equal(wantDevice, fingerprint.Device)
	assert.NotEmpty(fingerprint.Hash)

	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source:      found,
		Fingerprint: fingerprint,
	})
	require.NoError(err)
	require.True(outcome.ResultSetComplete)
	require.Len(outcome.Results, 1)
	result := outcome.Results[0]
	assert.Equal(DataVersionCurrent, result.DataVersion)
	assert.Equal("codex:"+uuid, result.Result.Session.ID)
	assert.Equal(AgentCodex, result.Result.Session.Agent)
	assert.Equal("devbox", result.Result.Session.Machine)
	assert.Equal("api", result.Result.Session.Project)
	assert.Equal("Renamed title", result.Result.Session.SessionName)
	assert.Equal(fingerprint.Hash, result.Result.Session.File.Hash)
	assert.Len(result.Result.Messages, 1)
}

func TestCodexProviderUnresolvedParentNeedsRetry(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	const childID = "22222222-2222-4222-8222-222222222222"
	const parentID = "11111111-1111-4111-8111-111111111111"
	content := testjsonl.JoinJSONL(
		testjsonl.CodexForkedSessionMetaJSON(
			childID, parentID, "/workspace/project", "codex_cli_rs", tsEarly,
		),
		testjsonl.CodexTurnContextWithIDJSON("gpt-5.4", "child-turn", tsEarlyS1),
		testjsonl.CodexMsgJSON("user", "child task", tsEarlyS1),
		testjsonl.CodexMsgJSON("assistant", "child answer", tsEarlyS5),
	)
	writeCodexProviderSessionContent(t, root, childID, content)
	provider, ok := NewProvider(AgentCodex, ProviderConfig{Roots: []string{root}})
	require.True(ok)
	source := requireCodexProviderSource(t, provider, childID)

	outcome, err := provider.Parse(t.Context(), ParseRequest{Source: source})

	require.NoError(err)
	require.Len(outcome.Results, 1)
	result := outcome.Results[0]
	assert.Equal(DataVersionNeedsRetry, result.DataVersion)
	assert.Contains(result.RetryReason, "parent turns")
	require.Len(result.Result.Messages, 2)
	assert.Equal("child task", result.Result.Messages[0].Content)
	assert.Equal("child answer", result.Result.Messages[1].Content)
}

func TestCodexProviderTurnlessParentResolvesWithoutRetry(t *testing.T) {
	// Codex Desktop writes a rollout when a thread opens; forking before the
	// first prompt produces a parent holding only session_meta and settings
	// events. Such a parent is present and fully readable, so the fork must
	// parse as current instead of being marked for a retry that can never
	// succeed. Whether the child carries a copied parent session_meta is
	// not a signal: real forks replay parent history with and without it.
	for _, copiedParentMeta := range []bool{false, true} {
		t.Run(fmt.Sprintf("copied_parent_meta_%t", copiedParentMeta), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			root := t.TempDir()
			const childID = "22222222-2222-4222-8222-222222222222"
			const parentID = "11111111-1111-4111-8111-111111111111"
			writeCodexProviderSessionContent(t, root, parentID, testjsonl.JoinJSONL(
				testjsonl.CodexSessionMetaJSON(parentID, "/workspace/project", "codex_cli_rs", tsEarly),
			))
			var copiedMeta string
			if copiedParentMeta {
				copiedMeta = testjsonl.CodexSessionMetaJSON(parentID, "/workspace/project", "codex_cli_rs", tsEarly)
			}
			writeCodexProviderSessionContent(t, root, childID, testjsonl.JoinJSONL(
				testjsonl.CodexForkedSessionMetaJSON(
					childID, parentID, "/workspace/project", "codex_cli_rs", tsEarly,
				),
				copiedMeta,
				testjsonl.CodexTurnContextWithIDJSON("gpt-5.4", "child-turn", tsEarlyS1),
				testjsonl.CodexMsgJSON("user", "child task", tsEarlyS1),
				testjsonl.CodexMsgJSON("assistant", "child answer", tsEarlyS5),
			))
			provider, ok := NewProvider(AgentCodex, ProviderConfig{Roots: []string{root}})
			require.True(ok)
			source := requireCodexProviderSource(t, provider, childID)

			outcome, err := provider.Parse(t.Context(), ParseRequest{Source: source})

			require.NoError(err)
			require.Len(outcome.Results, 1)
			result := outcome.Results[0]
			assert.Equal(DataVersionCurrent, result.DataVersion)
			assert.Empty(result.RetryReason)
			require.Len(result.Result.Messages, 2)
			assert.Equal("child task", result.Result.Messages[0].Content)
			assert.Equal("child answer", result.Result.Messages[1].Content)
		})
	}
}
func TestCodexProviderUnresolvedParentWithoutFinalNewlineNeedsRetry(t *testing.T) {
	require := require.New(t)

	root := t.TempDir()
	const childID = "22222222-2222-4222-8222-222222222222"
	const parentID = "11111111-1111-4111-8111-111111111111"
	content := strings.TrimSuffix(testjsonl.JoinJSONL(
		testjsonl.CodexForkedSessionMetaJSON(
			childID, parentID, "/workspace/project", "codex_cli_rs", tsEarly,
		),
	), "\n")
	writeCodexProviderSessionContent(t, root, childID, content)
	provider, ok := NewProvider(AgentCodex, ProviderConfig{Roots: []string{root}})
	require.True(ok)
	source := requireCodexProviderSource(t, provider, childID)

	outcome, err := provider.Parse(t.Context(), ParseRequest{Source: source})

	require.NoError(err)
	require.Len(outcome.Results, 1)
	assert.Equal(t, DataVersionNeedsRetry, outcome.Results[0].DataVersion)
}

func TestCodexProviderChildOnlySubagentWithoutParentStaysCurrent(t *testing.T) {
	require := require.New(t)

	root := t.TempDir()
	const childID = "22222222-2222-4222-8222-222222222222"
	const parentID = "11111111-1111-4111-8111-111111111111"
	content := testjsonl.JoinJSONL(
		testjsonl.CodexSubagentSessionMetaJSON(
			childID, parentID, "/workspace/project", "codex_cli_rs", tsEarly,
		),
		testjsonl.CodexTurnContextWithIDJSON("gpt-5.4", "child-turn", tsEarlyS1),
		testjsonl.CodexMsgJSON("user", "child task", tsEarlyS1),
	)
	writeCodexProviderSessionContent(t, root, childID, content)
	provider, ok := NewProvider(AgentCodex, ProviderConfig{Roots: []string{root}})
	require.True(ok)
	source := requireCodexProviderSource(t, provider, childID)

	outcome, err := provider.Parse(t.Context(), ParseRequest{Source: source})

	require.NoError(err)
	require.Len(outcome.Results, 1)
	assert.Equal(t, DataVersionCurrent, outcome.Results[0].DataVersion)
}

func TestCodexActivityHintsUseConfiguredRootParent(t *testing.T) {
	base := t.TempDir()
	provider, ok := NewProvider(AgentCodex, ProviderConfig{Roots: []string{
		"",
		filepath.Join(base, "sessions"),
		filepath.Join(base, "archived_sessions"),
		"s3://bucket/archive/sessions",
	}})
	require.True(t, ok)

	hints := provider.(ActivityHintProvider)
	sources, err := hints.ActivityHintSources(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []ActivityHintSource{{
		Path: filepath.Join(base, "history.jsonl"),
	}}, sources)
}

func TestCodexActivityHintDecodesIdentityWithoutPrompt(t *testing.T) {
	provider, ok := NewProvider(AgentCodex, ProviderConfig{
		Roots: []string{t.TempDir()},
	})
	require.True(t, ok)

	hint, accepted := provider.(ActivityHintProvider).DecodeActivityHint(
		[]byte(`{"session_id":"019f0000-0000-7000-8000-000000000001",` +
			`"ts":1785376202,"text":"private prompt sentinel"}`),
	)

	assert.True(t, accepted)
	assert.Equal(t, ActivityHint{
		RawSessionID: "019f0000-0000-7000-8000-000000000001",
		Timestamp:    time.Unix(1785376202, 0).UTC(),
	}, hint)
}

func TestCodexActivityHintRejectsInvalidRecords(t *testing.T) {
	provider, ok := NewProvider(AgentCodex, ProviderConfig{
		Roots: []string{t.TempDir()},
	})
	require.True(t, ok)
	hints := provider.(ActivityHintProvider)

	for _, line := range []string{
		`{`,
		`{"session_id":"","ts":1785376202}`,
		`{"session_id":"not-a-uuid","ts":1785376202}`,
		`{"session_id":"019f0000-0000-7000-8000-000000000001","ts":0}`,
	} {
		_, accepted := hints.DecodeActivityHint([]byte(line))
		assert.Falsef(t, accepted, "line %q", line)
	}
}

func TestCodexProviderForceParseReloadsSameStatSessionIndex(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	base := t.TempDir()
	root := filepath.Join(base, "sessions")
	uuid := "019eb791-cf7d-75c1-8439-9ed74c1229f3"
	writeCodexProviderSession(t, root, uuid, "keep this prompt")
	indexPath := filepath.Join(base, CodexSessionIndexFilename)
	original := `{"id":"` + uuid + `","thread_name":"Alpha title"}` + "\n"
	rewritten := `{"id":"` + uuid + `","thread_name":"Bravo title"}` + "\n"
	require.Len(rewritten, len(original), "index fixtures must have equal length")
	require.NoError(os.WriteFile(indexPath, []byte(original), 0o644))
	stableTime := time.Unix(1_800_000_000, 123_000_000)
	require.NoError(os.Chtimes(indexPath, stableTime, stableTime))

	provider, ok := NewProvider(AgentCodex, ProviderConfig{Roots: []string{root}})
	require.True(ok)
	source := requireCodexProviderSource(t, provider, uuid)
	fingerprint, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)
	first, err := provider.Parse(t.Context(), ParseRequest{
		Source: source, Fingerprint: fingerprint,
	})
	require.NoError(err)
	require.Len(first.Results, 1)
	assert.Equal("Alpha title", first.Results[0].Result.Session.SessionName)

	before, err := os.Stat(indexPath)
	require.NoError(err)
	require.NoError(os.WriteFile(indexPath, []byte(rewritten), 0o644))
	require.NoError(os.Chtimes(indexPath, before.ModTime(), before.ModTime()))
	after, err := os.Stat(indexPath)
	require.NoError(err)
	require.Equal(before.Size(), after.Size(), "index size must stay unchanged")
	require.Equal(before.ModTime(), after.ModTime(), "index mtime must stay unchanged")

	forced, err := provider.Parse(t.Context(), ParseRequest{
		Source:      source,
		Fingerprint: fingerprint,
		ForceParse:  true,
	})

	require.NoError(err)
	require.Len(forced.Results, 1)
	assert.Equal("Bravo title", forced.Results[0].Result.Session.SessionName)
}

func TestCodexProviderAdvertisesIncrementalAppend(t *testing.T) {
	root := t.TempDir()
	uuid := "019eb791-cf7d-75c1-8439-9ed74c1229e2"
	writeCodexProviderSession(t, root, uuid, "hello")

	provider, ok := NewProvider(AgentCodex, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	assert.Equal(t,
		CapabilitySupported,
		provider.Capabilities().Source.IncrementalAppend,
	)
}

func TestCodexProviderFactoryScopesCursorCache(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	def, ok := AgentByType(AgentCodex)
	require.True(ok)
	root := t.TempDir()
	uuid := "019eb791-cf7d-75c1-8439-9ed74c1229e3"
	path := writeCodexProviderSession(t, root, uuid, "seed shared cache")

	sharedFactory := newCodexProviderFactory(def)
	seedingProvider, ok := sharedFactory.NewProvider(ProviderConfig{
		Roots: []string{root},
	}).(*codexProvider)
	require.True(ok)
	siblingProvider, ok := sharedFactory.NewProvider(ProviderConfig{
		Roots: []string{root},
	}).(*codexProvider)
	require.True(ok)
	isolatedFactory := newCodexProviderFactory(def)
	isolatedProvider, ok := isolatedFactory.NewProvider(ProviderConfig{
		Roots: []string{root},
	}).(*codexProvider)
	require.True(ok)

	sess, _, err := seedingProvider.parseSession(path, "local", false)
	require.NoError(err)
	require.NotNil(sess)
	info, err := os.Stat(path)
	require.NoError(err)
	inode, device := sourceFileIdentityForPath(path, info)

	_, siblingHit := siblingProvider.cursorCache.Get(
		path, info.Size(), inode, device,
	)
	_, isolatedHit := isolatedProvider.cursorCache.Get(
		path, info.Size(), inode, device,
	)
	assert.True(siblingHit)
	assert.False(isolatedHit)
}

func TestCodexProviderFullParseSnapshotExcludesLaterGrowth(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	uuid := "019eb791-cf7d-75c1-8439-9ed74c1229ec"
	initial := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			uuid, "/home/user/code/api", "codex_cli_rs", tsEarly,
		),
		testjsonl.CodexTurnContextJSON("gpt-5.4", tsEarlyS1),
		testjsonl.CodexMsgJSON("user", "captured request", "2024-01-01T10:00:02Z"),
		codexEventMsgJSON("task_complete", "2024-01-01T10:00:03Z"),
	)
	path := writeCodexProviderSessionContent(t, root, uuid, initial)
	provider, ok := NewProvider(AgentCodex, ProviderConfig{Roots: []string{root}})
	require.True(ok)
	concrete, ok := provider.(*codexProvider)
	require.True(ok)
	source := requireCodexProviderSource(t, provider, uuid)

	snapshot, err := os.Open(path)
	require.NoError(err)
	defer snapshot.Close()
	capturedInfo, err := snapshot.Stat()
	require.NoError(err)
	tail := testjsonl.JoinJSONL(
		testjsonl.CodexTurnContextJSON("gpt-5.5", "2024-01-01T10:00:04Z"),
		testjsonl.CodexMsgJSON("assistant", "later answer", "2024-01-01T10:00:05Z"),
		codexEventMsgJSON("task_started", "2024-01-01T10:00:06Z"),
	)
	appendCodexProviderContent(t, path, tail)

	sess, messages, err := concrete.parseSessionSnapshot(
		path, "local", false, snapshot, capturedInfo,
	)

	require.NoError(err)
	require.NotNil(sess)
	assert.Equal(int64(len(initial)), sess.File.Size)
	require.Len(messages, 1)
	assert.Equal(RoleUser, messages[0].Role)
	assert.Equal("captured request", messages[0].Content)
	inode, device := sourceFileIdentityForFile(snapshot, capturedInfo)
	seed, seeded := concrete.cursorCache.Get(
		path, int64(len(initial)), inode, device,
	)
	require.True(seeded)
	assert.Equal("gpt-5.4", seed.model)
	assert.Equal("task_complete", seed.lastTaskEvent)

	currentFingerprint, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)
	outcome, status, err := provider.ParseIncremental(
		t.Context(),
		IncrementalRequest{
			Source:       source,
			Fingerprint:  currentFingerprint,
			SessionID:    "codex:" + uuid,
			Offset:       int64(len(initial)),
			StartOrdinal: 1,
		},
	)

	require.NoError(err)
	assert.Equal(IncrementalApplied, status)
	require.Len(outcome.Messages, 1)
	assert.Equal(RoleAssistant, outcome.Messages[0].Role)
	assert.Equal("later answer", outcome.Messages[0].Content)
	assert.Equal(1, outcome.Messages[0].Ordinal)
	assert.Equal("gpt-5.5", outcome.Messages[0].Model)
	assert.Equal(int64(len(tail)), outcome.ConsumedBytes)
}

func TestCodexFullParseHonorsContextBetweenLines(t *testing.T) {
	require := require.New(t)

	root := t.TempDir()
	uuid := "019eb791-cf7d-75c1-8439-9ed74c1229ed"
	path := writeCodexProviderSession(t, root, uuid, "question")
	provider, ok := NewProvider(AgentCodex, ProviderConfig{Roots: []string{root}})
	require.True(ok)
	concrete, ok := provider.(*codexProvider)
	require.True(ok)
	snapshot, err := os.Open(path)
	require.NoError(err)
	defer snapshot.Close()
	info, err := snapshot.Stat()
	require.NoError(err)
	ctx := newCancelOnErrCheckContext(t, 4)

	_, _, err = concrete.parseSessionSnapshotContext(
		ctx, path, "local", false, snapshot, info,
	)

	require.ErrorIs(err, context.Canceled)
}

func TestCodexProviderFullParseSnapshotKeepsDescriptorIdentityAfterReplacement(
	t *testing.T,
) {
	assert := assert.New(t)
	require := require.New(t)

	if runtime.GOOS == "windows" {
		t.Skip("Windows cannot atomically replace an open file")
	}
	root := t.TempDir()
	uuid := "019eb791-cf7d-75c1-8439-9ed74c1229f4"
	original := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			uuid, "/workspace/original", "codex_cli_rs", tsEarly,
		),
		testjsonl.CodexMsgJSON("user", "old snapshot", tsEarlyS1),
	)
	replacement := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			uuid, "/workspace/replaced", "codex_cli_rs", tsEarly,
		),
		testjsonl.CodexMsgJSON("user", "new pathname", tsEarlyS1),
	)
	path := writeCodexProviderSessionContent(t, root, uuid, original)
	provider, ok := NewProvider(AgentCodex, ProviderConfig{Roots: []string{root}})
	require.True(ok)
	concrete, ok := provider.(*codexProvider)
	require.True(ok)

	snapshot, err := os.Open(path)
	require.NoError(err)
	defer snapshot.Close()
	snapshotInfo, err := snapshot.Stat()
	require.NoError(err)
	oldInode, oldDevice := sourceFileIdentityForFile(snapshot, snapshotInfo)

	replacementPath := path + ".replacement"
	require.NoError(os.WriteFile(replacementPath, []byte(replacement), 0o644))
	require.NoError(os.Rename(replacementPath, path))
	currentInfo, err := os.Stat(path)
	require.NoError(err)
	newInode, newDevice := sourceFileIdentityForPath(path, currentInfo)

	sess, messages, err := concrete.parseSessionSnapshot(
		path, "local", false, snapshot, snapshotInfo,
	)

	require.NoError(err)
	require.NotNil(sess)
	require.Len(messages, 1)
	assert.Equal("old snapshot", messages[0].Content)
	assert.Equal("/workspace/original", sess.Cwd)
	assert.Equal(int64(oldInode), sess.File.Inode)
	assert.Equal(int64(oldDevice), sess.File.Device)
	_, oldCursor := concrete.cursorCache.Get(
		path, snapshotInfo.Size(), oldInode, oldDevice,
	)
	assert.True(oldCursor, "snapshot cursor must use the descriptor identity")
	if oldInode != newInode || oldDevice != newDevice {
		_, replacementCursor := concrete.cursorCache.Get(
			path, snapshotInfo.Size(), newInode, newDevice,
		)
		assert.False(replacementCursor,
			"old snapshot state must not be keyed to the replacement identity")
	}
}

func TestCodexProviderIncrementalSnapshotExcludesLaterGrowth(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	uuid := "019eb791-cf7d-75c1-8439-9ed74c1229f1"
	prefix := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			uuid, "/workspace/project-a", "codex_cli_rs", tsEarly,
		),
		testjsonl.CodexTurnContextJSON("gpt-5.4", tsEarlyS1),
		testjsonl.CodexMsgJSON("user", "persisted request", "2024-01-01T10:00:02Z"),
	)
	path := writeCodexProviderSessionContent(t, root, uuid, prefix)
	provider, ok := NewProvider(AgentCodex, ProviderConfig{Roots: []string{root}})
	require.True(ok)
	concrete, ok := provider.(*codexProvider)
	require.True(ok)
	source := requireCodexProviderSource(t, provider, uuid)
	prefixFingerprint, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)
	_, err = provider.Parse(t.Context(), ParseRequest{
		Source: source, Fingerprint: prefixFingerprint,
	})
	require.NoError(err)

	capturedTail := testjsonl.JoinJSONL(
		testjsonl.CodexMsgJSON("assistant", "captured answer", "2024-01-01T10:00:03Z"),
	)
	appendCodexProviderContent(t, path, capturedTail)
	capturedFingerprint, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)
	laterTail := testjsonl.JoinJSONL(
		testjsonl.CodexMsgJSON("assistant", "later answer", "2024-01-01T10:00:04Z"),
	)
	appendCodexProviderContent(t, path, laterTail)

	first, status, err := provider.ParseIncremental(
		t.Context(),
		IncrementalRequest{
			Source:       source,
			Fingerprint:  capturedFingerprint,
			SessionID:    "codex:" + uuid,
			Offset:       int64(len(prefix)),
			StartOrdinal: 1,
		},
	)

	require.NoError(err)
	assert.Equal(IncrementalApplied, status)
	require.Len(first.Messages, 1)
	assert.Equal("captured answer", first.Messages[0].Content)
	assert.Equal(1, first.Messages[0].Ordinal)
	assert.Equal(int64(len(capturedTail)), first.ConsumedBytes)
	_, capturedStaged := concrete.cursorCache.Get(
		path,
		capturedFingerprint.Size,
		capturedFingerprint.Inode,
		capturedFingerprint.Device,
	)
	_, laterStaged := concrete.cursorCache.Get(
		path,
		capturedFingerprint.Size+int64(len(laterTail)),
		capturedFingerprint.Inode,
		capturedFingerprint.Device,
	)
	assert.True(capturedStaged)
	assert.False(laterStaged)

	currentFingerprint, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)
	second, status, err := provider.ParseIncremental(
		t.Context(),
		IncrementalRequest{
			Source:       source,
			Fingerprint:  currentFingerprint,
			SessionID:    "codex:" + uuid,
			Offset:       capturedFingerprint.Size,
			StartOrdinal: 2,
		},
	)

	require.NoError(err)
	assert.Equal(IncrementalApplied, status)
	require.Len(second.Messages, 1)
	assert.Equal("later answer", second.Messages[0].Content)
	assert.Equal(2, second.Messages[0].Ordinal)
	assert.Equal(int64(len(laterTail)), second.ConsumedBytes)
}

func TestCodexProviderIncrementalSnapshotKeepsCapturedEOFConservative(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	uuid := "019eb791-cf7d-75c1-8439-9ed74c1229f2"
	prefix := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			uuid, "/workspace/project-a", "codex_cli_rs", tsEarly,
		),
		testjsonl.CodexMsgJSON("user", "persisted request", tsEarlyS1),
	)
	unterminated := testjsonl.CodexMsgJSON(
		"assistant", "captured without newline", tsLate,
	)
	path := writeCodexProviderSessionContent(
		t, root, uuid, prefix+unterminated,
	)
	provider, ok := NewProvider(AgentCodex, ProviderConfig{Roots: []string{root}})
	require.True(ok)
	concrete, ok := provider.(*codexProvider)
	require.True(ok)
	source := requireCodexProviderSource(t, provider, uuid)
	capturedFingerprint, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)
	later := testjsonl.CodexMsgJSON(
		"assistant", "later answer", tsLateS5,
	) + "\n"
	appendCodexProviderContent(t, path, "\n"+later)

	outcome, status, err := provider.ParseIncremental(
		t.Context(),
		IncrementalRequest{
			Source:       source,
			Fingerprint:  capturedFingerprint,
			SessionID:    "codex:" + uuid,
			Offset:       int64(len(prefix)),
			StartOrdinal: 1,
		},
	)

	require.NoError(err)
	assert.Equal(IncrementalNeedsFullParse, status)
	assert.True(outcome.ForceReplace)
	assert.Empty(outcome.Messages)
	_, oldStaged := concrete.cursorCache.Get(
		path,
		int64(len(prefix)),
		capturedFingerprint.Inode,
		capturedFingerprint.Device,
	)
	_, newStaged := concrete.cursorCache.Get(
		path,
		capturedFingerprint.Size,
		capturedFingerprint.Inode,
		capturedFingerprint.Device,
	)
	assert.False(oldStaged)
	assert.False(newStaged)
}

func TestCodexProviderIncrementalAppendedSessionMetaNeedsFullParse(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	childID := "019eb791-cf7d-75c1-8439-9ed74c1229f3"
	parentID := "019eb791-cb95-7a14-830f-9968e582f290"
	prefix := testjsonl.JoinJSONL(
		testjsonl.CodexSubagentSessionMetaJSON(
			childID, parentID, "/workspace/project-a", "codex_cli_rs", tsEarly,
		),
	)
	path := writeCodexProviderSessionContent(t, root, childID, prefix)
	provider, ok := NewProvider(AgentCodex, ProviderConfig{Roots: []string{root}})
	require.True(ok)
	source := requireCodexProviderSource(t, provider, childID)
	initialFingerprint, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)

	appendCodexProviderContent(t, path, testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			parentID, "/workspace/project-a", "codex_cli_rs", tsEarly,
		),
		testjsonl.CodexMsgJSON(
			"assistant", "replayed parent answer", tsEarlyS5,
		),
	))
	currentFingerprint, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)

	outcome, status, err := provider.ParseIncremental(
		t.Context(),
		IncrementalRequest{
			Source:       source,
			Fingerprint:  currentFingerprint,
			SessionID:    "codex:" + childID,
			Offset:       initialFingerprint.Size,
			StartOrdinal: 0,
		},
	)

	require.NoError(err)
	assert.Equal(IncrementalNeedsFullParse, status)
	assert.True(outcome.ForceReplace)
	assert.Empty(outcome.Messages)
}

func TestCodexProviderIncrementalRejectsFingerprintIdentityMismatch(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	uuid := "019eb791-cf7d-75c1-8439-9ed74c1229f3"
	prefix := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			uuid, "/workspace/project-a", "codex_cli_rs", tsEarly,
		),
		testjsonl.CodexMsgJSON("user", "persisted request", tsEarlyS1),
	)
	tail := testjsonl.JoinJSONL(
		testjsonl.CodexMsgJSON("assistant", "new answer", tsLate),
	)
	writeCodexProviderSessionContent(t, root, uuid, prefix+tail)
	provider, ok := NewProvider(AgentCodex, ProviderConfig{Roots: []string{root}})
	require.True(ok)
	source := requireCodexProviderSource(t, provider, uuid)
	fingerprint, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)
	fingerprint.Inode++
	if fingerprint.Inode == 0 {
		fingerprint.Inode = 1
	}

	outcome, status, err := provider.ParseIncremental(
		t.Context(),
		IncrementalRequest{
			Source:       source,
			Fingerprint:  fingerprint,
			SessionID:    "codex:" + uuid,
			Offset:       int64(len(prefix)),
			StartOrdinal: 1,
		},
	)

	require.NoError(err)
	assert.Equal(IncrementalNeedsFullParse, status)
	assert.True(outcome.ForceReplace)
	assert.Empty(outcome.Messages)
}

func TestCodexProviderIncrementalFirstGenuinePromptNeedsFullParse(t *testing.T) {
	const childID = "019c9c96-6ee7-77c0-ba4c-380f844289d5"
	orphanNotification := "<subagent_notification>\n" +
		`{"agent_id":"` + childID +
		`","status":{"completed":"Orphan task finished"}}` + "\n" +
		"</subagent_notification>"
	tail := testjsonl.JoinJSONL(
		testjsonl.CodexMsgJSON(
			"user", "first genuine request", tsLate,
		),
	)

	for _, mode := range []string{"warm", "cold"} {
		t.Run(mode, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			root := t.TempDir()
			uuid := "019eb791-cf7d-75c1-8439-9ed74c1229f4"
			prefix := testjsonl.JoinJSONL(
				testjsonl.CodexSessionMetaJSON(
					uuid, "/workspace/project-a", "codex_cli_rs", tsEarly,
				),
				testjsonl.CodexMsgJSON(
					"user", orphanNotification, tsEarlyS1,
				),
			)
			content := prefix
			if mode == "cold" {
				content += tail
			}
			path := writeCodexProviderSessionContent(
				t, root, uuid, content,
			)
			provider, ok := NewProvider(
				AgentCodex, ProviderConfig{Roots: []string{root}},
			)
			require.True(ok)
			concrete, ok := provider.(*codexProvider)
			require.True(ok)
			source := requireCodexProviderSource(t, provider, uuid)

			if mode == "warm" {
				prefixFingerprint, err := provider.Fingerprint(
					t.Context(), source,
				)
				require.NoError(err)
				full, err := provider.Parse(
					t.Context(),
					ParseRequest{
						Source: source, Fingerprint: prefixFingerprint,
					},
				)
				require.NoError(err)
				require.Len(full.Results, 1)
				assert.Empty(full.Results[0].Result.Session.FirstMessage)
				require.Len(full.Results[0].Result.Messages, 1)
				assert.Equal(
					"Orphan task finished",
					full.Results[0].Result.Messages[0].Content,
				)
				appendCodexProviderContent(t, path, tail)
			}

			fingerprint, err := provider.Fingerprint(
				t.Context(), source,
			)
			require.NoError(err)
			inode, device := fingerprint.Inode, fingerprint.Device
			prefixOffset := int64(len(prefix))
			_, oldBefore := concrete.cursorCache.Get(
				path, prefixOffset, inode, device,
			)

			outcome, status, err := provider.ParseIncremental(
				t.Context(),
				IncrementalRequest{
					Source:       source,
					Fingerprint:  fingerprint,
					SessionID:    "codex:" + uuid,
					Offset:       prefixOffset,
					StartOrdinal: 1,
				},
			)

			require.NoError(err)
			assert.Equal(IncrementalNeedsFullParse, status)
			assert.True(outcome.ForceReplace)
			assert.Empty(outcome.Messages)
			_, oldAfter := concrete.cursorCache.Get(
				path, prefixOffset, inode, device,
			)
			_, newAfter := concrete.cursorCache.Get(
				path, fingerprint.Size, inode, device,
			)
			assert.Equal(oldBefore, oldAfter)
			assert.False(newAfter)
			if mode == "cold" {
				assert.False(oldAfter)
			}

			full, err := provider.Parse(
				t.Context(),
				ParseRequest{Source: source, Fingerprint: fingerprint},
			)
			require.NoError(err)
			require.Len(full.Results, 1)
			assert.Equal(
				"first genuine request",
				full.Results[0].Result.Session.FirstMessage,
			)
			require.Len(full.Results[0].Result.Messages, 2)
		})
	}
}

func TestCodexProviderIncrementalCustomToolOutputUpdatesStoredCall(t *testing.T) {
	const (
		uuid   = "019eb791-cf7d-75c1-8439-9ed74c1229f7"
		call   = `{"timestamp":"2026-08-02T09:00:02Z","type":"response_item","payload":{"type":"custom_tool_call","name":"apply_patch","call_id":"call_patch","input":"*** Begin Patch\n*** End Patch"}}`
		output = `{"timestamp":"2026-08-02T09:00:03Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call_patch","output":"Success. Updated one file.","status":"completed"}}`
	)
	prefix := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			uuid, "/workspace/project-a", "codex_cli_rs", tsEarly,
		),
		testjsonl.CodexMsgJSON("user", "apply the patch", tsEarlyS1),
		call,
	)
	tail := testjsonl.JoinJSONL(output)

	for _, mode := range []string{"warm", "cold"} {
		t.Run(mode, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			root := t.TempDir()
			content := prefix
			if mode == "cold" {
				content += tail
			}
			path := writeCodexProviderSessionContent(
				t, root, uuid, content,
			)
			provider, ok := NewProvider(
				AgentCodex, ProviderConfig{Roots: []string{root}},
			)
			require.True(ok)
			source := requireCodexProviderSource(t, provider, uuid)

			if mode == "warm" {
				fingerprint, err := provider.Fingerprint(
					t.Context(), source,
				)
				require.NoError(err)
				_, err = provider.Parse(t.Context(), ParseRequest{
					Source: source, Fingerprint: fingerprint,
				})
				require.NoError(err)
				appendCodexProviderContent(t, path, tail)
			}

			fingerprint, err := provider.Fingerprint(
				t.Context(), source,
			)
			require.NoError(err)
			outcome, status, err := provider.ParseIncremental(
				t.Context(), IncrementalRequest{
					Source:       source,
					Fingerprint:  fingerprint,
					SessionID:    "codex:" + uuid,
					Offset:       int64(len(prefix)),
					StartOrdinal: 2,
				},
			)

			require.NoError(err)
			assert.Equal(IncrementalApplied, status)
			assert.Empty(outcome.Messages)
			require.Len(outcome.ToolCallUpdates, 1)
			assert.Equal("call_patch", outcome.ToolCallUpdates[0].ToolUseID)
			require.Len(outcome.ToolCallUpdates[0].ResultEvents, 1)
			event := outcome.ToolCallUpdates[0].ResultEvents[0]
			assert.Equal("custom_tool_call_output", event.Source)
			assert.Equal("completed", event.Status)
			assert.Equal("Success. Updated one file.", event.Content)
		})
	}
}

func TestCodexProviderIncrementalLateOutputAttachesCommittedUsage(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	const uuid = "019eb791-cf7d-75c1-8439-9ed74c1229f8"
	prefix := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			uuid, "/workspace/project-a", "codex_cli_rs", tsEarly,
		),
		testjsonl.CodexTurnContextJSON("gpt-5.4", tsEarlyS1),
		testjsonl.CodexMsgJSON("user", "inspect the repository", tsEarlyS1),
		testjsonl.CodexFunctionCallWithCallIDJSON(
			"shell", "call_late", map[string]any{"cmd": "git status"}, tsEarlyS5,
		),
	)
	tail := testjsonl.JoinJSONL(
		testjsonl.CodexFunctionCallOutputJSON(
			"call_late", "working tree clean", tsLate,
		),
		testjsonl.CodexTokenCountJSON(tsLateS5, 100_000, 250, 64_000),
	)

	root := t.TempDir()
	path := writeCodexProviderSessionContent(t, root, uuid, prefix)
	provider, ok := NewProvider(AgentCodex, ProviderConfig{Roots: []string{root}})
	require.True(ok)
	source := requireCodexProviderSource(t, provider, uuid)
	prefixFingerprint, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)
	full, err := provider.Parse(t.Context(), ParseRequest{
		Source: source, Fingerprint: prefixFingerprint,
	})
	require.NoError(err)
	require.Len(full.Results, 1)
	require.Len(full.Results[0].Result.Messages, 2)
	assert.Equal(RoleAssistant, full.Results[0].Result.Messages[1].Role)

	appendCodexProviderContent(t, path, tail)
	fingerprint, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)
	pendingUsageOrdinal := 1
	outcome, status, err := provider.ParseIncremental(
		t.Context(), IncrementalRequest{
			Source:                    source,
			Fingerprint:               fingerprint,
			SessionID:                 "codex:" + uuid,
			Offset:                    prefixFingerprint.Size,
			StartOrdinal:              2,
			StoredPendingUsageOrdinal: &pendingUsageOrdinal,
		},
	)

	require.NoError(err)
	assert.Equal(IncrementalApplied, status)
	assert.False(outcome.ForceReplace)
	assert.Empty(outcome.Messages)
	require.Len(outcome.ToolCallUpdates, 1)
	assert.Equal("call_late", outcome.ToolCallUpdates[0].ToolUseID)
	require.Len(outcome.MessageTokenUsageUpdates, 1)
	usage := outcome.MessageTokenUsageUpdates[0]
	assert.Equal(1, usage.Ordinal)
	assert.Equal(100_000, usage.ContextTokens)
	assert.Equal(250, usage.OutputTokens)
	assert.True(usage.HasContextTokens)
	assert.True(usage.HasOutputTokens)
	assert.NotEmpty(usage.TokenUsage)
}

func TestCodexProviderColdIncrementalStagesRetryCursorVersions(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	uuid := "019eb791-cf7d-75c1-8439-9ed74c1229ed"
	prefix := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			uuid, "/home/user/code/api", "codex_cli_rs", tsEarly,
		),
		testjsonl.CodexTurnContextJSON("gpt-5.4", tsEarlyS1),
		testjsonl.CodexMsgJSON("user", "persisted request", "2024-01-01T10:00:02Z"),
		codexEventMsgJSON("task_complete", "2024-01-01T10:00:03Z"),
	)
	tail := testjsonl.JoinJSONL(
		testjsonl.CodexTurnContextJSON("gpt-5.5", "2024-01-01T10:00:04Z"),
		testjsonl.CodexMsgJSON("assistant", "proposed answer", "2024-01-01T10:00:05Z"),
		codexEventMsgJSON("task_started", "2024-01-01T10:00:06Z"),
	)
	path := writeCodexProviderSessionContent(t, root, uuid, prefix+tail)
	provider, ok := NewProvider(AgentCodex, ProviderConfig{Roots: []string{root}})
	require.True(ok)
	concrete, ok := provider.(*codexProvider)
	require.True(ok)
	source := requireCodexProviderSource(t, provider, uuid)
	fingerprint, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)
	prefixOffset := int64(len(prefix))
	_, oldBefore := concrete.cursorCache.Get(
		path, prefixOffset, fingerprint.Inode, fingerprint.Device,
	)
	_, newBefore := concrete.cursorCache.Get(
		path, fingerprint.Size, fingerprint.Inode, fingerprint.Device,
	)
	assert.False(oldBefore)
	assert.False(newBefore)

	req := IncrementalRequest{
		Source:       source,
		Fingerprint:  fingerprint,
		SessionID:    "codex:" + uuid,
		Offset:       prefixOffset,
		StartOrdinal: 1,
	}
	first, status, err := provider.ParseIncremental(t.Context(), req)

	require.NoError(err)
	assert.Equal(IncrementalApplied, status)
	require.Len(first.Messages, 1)
	assert.Equal(RoleAssistant, first.Messages[0].Role)
	assert.Equal("proposed answer", first.Messages[0].Content)
	assert.Equal(1, first.Messages[0].Ordinal)
	assert.Equal("gpt-5.5", first.Messages[0].Model)
	oldCursor, oldStaged := concrete.cursorCache.Get(
		path, prefixOffset, fingerprint.Inode, fingerprint.Device,
	)
	newCursor, newStaged := concrete.cursorCache.Get(
		path, fingerprint.Size, fingerprint.Inode, fingerprint.Device,
	)
	require.True(oldStaged, "persisted offset must be warm for a DB retry")
	require.True(newStaged, "proposed offset must keep its separate cursor")
	assert.Equal("gpt-5.4", oldCursor.model)
	assert.Equal("task_complete", oldCursor.lastTaskEvent)
	assert.Equal("gpt-5.5", newCursor.model)
	assert.Equal("task_started", newCursor.lastTaskEvent)

	// Simulate a failed DB write: the persisted offset remains unchanged and
	// the same tail is retried from the old cursor version.
	retry, retryStatus, err := provider.ParseIncremental(
		t.Context(), req,
	)

	require.NoError(err)
	assert.Equal(IncrementalApplied, retryStatus)
	assert.Equal(first, retry)
}

func TestCodexProviderParseIncrementalSuccess(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	uuid := "019eb791-cf7d-75c1-8439-9ed74c1229e4"
	initial := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			uuid, "/home/user/code/api", "codex_cli_rs", tsEarly,
		),
		testjsonl.CodexTurnContextJSON("gpt-5.4", tsEarlyS1),
		testjsonl.CodexMsgJSON("user", "initial request", "2024-01-01T10:00:02Z"),
		codexEventMsgJSON("task_complete", "2024-01-01T10:00:03Z"),
	)
	path := writeCodexProviderSessionContent(t, root, uuid, initial)
	provider, ok := NewProvider(AgentCodex, ProviderConfig{Roots: []string{root}})
	require.True(ok)
	source := requireCodexProviderSource(t, provider, uuid)
	initialFingerprint, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)
	full, err := provider.Parse(t.Context(), ParseRequest{
		Source:      source,
		Fingerprint: initialFingerprint,
	})
	require.NoError(err)
	require.Len(full.Results, 1)
	require.Len(full.Results[0].Result.Messages, 1)

	tail := testjsonl.JoinJSONL(
		testjsonl.CodexTurnContextJSON("gpt-5.5", "2024-01-01T10:00:04Z"),
		testjsonl.CodexMsgJSON("user", "follow-up request", "2024-01-01T10:00:05Z"),
		codexEventMsgJSON("task_started", "2024-01-01T10:00:06Z"),
		testjsonl.CodexMsgJSON("assistant", "tail answer", "2024-01-01T10:00:07Z"),
		testjsonl.CodexTokenCountJSON("2024-01-01T10:00:08Z", 100_000, 250, 64_000),
		codexEventMsgJSON("task_complete", "2024-01-01T10:00:09Z"),
	)
	appendCodexProviderContent(t, path, tail)
	currentFingerprint, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)

	outcome, status, err := provider.ParseIncremental(
		t.Context(),
		IncrementalRequest{
			Source:       source,
			Fingerprint:  currentFingerprint,
			SessionID:    "codex:" + uuid,
			Offset:       initialFingerprint.Size,
			StartOrdinal: 1,
		},
	)

	require.NoError(err)
	assert.Equal(IncrementalApplied, status)
	assert.Equal("codex:"+uuid, outcome.SessionID)
	require.Len(outcome.Messages, 2)
	assert.Equal(RoleUser, outcome.Messages[0].Role)
	assert.Equal("follow-up request", outcome.Messages[0].Content)
	assert.Equal(1, outcome.Messages[0].Ordinal)
	assert.Equal("gpt-5.5", outcome.Messages[0].Model)
	assert.Equal(RoleAssistant, outcome.Messages[1].Role)
	assert.Equal("tail answer", outcome.Messages[1].Content)
	assert.Equal(2, outcome.Messages[1].Ordinal)
	assert.Equal("gpt-5.5", outcome.Messages[1].Model)
	assert.Equal(time.Date(2024, time.January, 1, 10, 0, 9, 0, time.UTC), outcome.EndedAt)
	assert.Equal(int64(len(tail)), outcome.ConsumedBytes)
	assert.Equal(2, outcome.MessageCount)
	assert.Equal(1, outcome.UserMessageCount)
	assert.Equal(250, outcome.TotalOutputTokens)
	assert.Equal(100_000, outcome.PeakContextTokens)
	assert.True(outcome.HasTotalOutputTokens)
	assert.True(outcome.HasPeakContextTokens)
	require.NotNil(outcome.TerminationStatus)
	assert.Equal(TerminationAwaitingUser, *outcome.TerminationStatus)
	assert.False(outcome.ForceReplace)

	concrete, ok := provider.(*codexProvider)
	require.True(ok)
	oldSeed, oldOK := concrete.cursorCache.Get(
		path,
		initialFingerprint.Size,
		initialFingerprint.Inode,
		initialFingerprint.Device,
	)
	newSeed, newOK := concrete.cursorCache.Get(
		path,
		initialFingerprint.Size+int64(len(tail)),
		currentFingerprint.Inode,
		currentFingerprint.Device,
	)
	require.True(oldOK)
	require.True(newOK)
	assert.Equal("task_complete", oldSeed.lastTaskEvent)
	assert.Equal("task_complete", newSeed.lastTaskEvent)
}

func TestCodexProviderTokenCountCursorParity(t *testing.T) {
	cases := []struct {
		name            string
		uuid            string
		tailToken       string
		wantHasTokens   bool
		wantOutput      int
		wantPeakContext int
	}{
		{
			name:      "duplicate usage stays suppressed",
			uuid:      "019eb791-cf7d-75c1-8439-9ed74c1229ee",
			tailToken: testjsonl.CodexTokenCountJSON("2024-01-01T10:00:06Z", 100_000, 250, 64_000),
		},
		{
			name:            "changed usage attaches",
			uuid:            "019eb791-cf7d-75c1-8439-9ed74c1229ef",
			tailToken:       testjsonl.CodexTokenCountJSON("2024-01-01T10:00:06Z", 110_000, 300, 70_000),
			wantHasTokens:   true,
			wantOutput:      300,
			wantPeakContext: 110_000,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parentAssert := assert.New(t)
			parentRequire := require.New(t)

			prefix := testjsonl.JoinJSONL(
				testjsonl.CodexSessionMetaJSON(
					tc.uuid, "/workspace/project-a", "codex_cli_rs", tsEarly,
				),
				testjsonl.CodexTurnContextJSON("gpt-5.4", tsEarlyS1),
				testjsonl.CodexMsgJSON("user", "measure usage", "2024-01-01T10:00:02Z"),
				testjsonl.CodexMsgJSON("assistant", "prefix answer", "2024-01-01T10:00:03Z"),
				testjsonl.CodexTokenCountJSON("2024-01-01T10:00:04Z", 100_000, 250, 64_000),
			)
			tail := testjsonl.JoinJSONL(
				testjsonl.CodexMsgJSON("assistant", "tail answer", "2024-01-01T10:00:05Z"),
				tc.tailToken,
			)
			fullRoot := t.TempDir()
			writeCodexProviderSessionContent(
				t, fullRoot, tc.uuid, prefix+tail,
			)
			fullProvider, ok := NewProvider(
				AgentCodex, ProviderConfig{Roots: []string{fullRoot}},
			)
			parentRequire.True(ok)
			fullSource := requireCodexProviderSource(t, fullProvider, tc.uuid)
			fullFingerprint, err := fullProvider.Fingerprint(
				t.Context(), fullSource,
			)
			parentRequire.NoError(err)
			full, err := fullProvider.Parse(t.Context(), ParseRequest{
				Source: fullSource, Fingerprint: fullFingerprint,
			})
			parentRequire.NoError(err)
			parentRequire.Len(full.Results, 1)
			parentRequire.Len(full.Results[0].Result.Messages, 3)
			fullTail := full.Results[0].Result.Messages[2]
			parentAssert.Equal(RoleAssistant, fullTail.Role)
			parentAssert.Equal("tail answer", fullTail.Content)
			parentAssert.Equal(tc.wantHasTokens, fullTail.HasOutputTokens)
			parentAssert.Equal(tc.wantOutput, fullTail.OutputTokens)
			parentAssert.Equal(tc.wantPeakContext, fullTail.ContextTokens)

			for _, mode := range []string{"warm", "cold"} {
				t.Run(mode, func(t *testing.T) {
					assert := assert.New(t)
					require := require.New(t)

					root := t.TempDir()
					content := prefix
					if mode == "cold" {
						content += tail
					}
					path := writeCodexProviderSessionContent(
						t, root, tc.uuid, content,
					)
					provider, ok := NewProvider(
						AgentCodex, ProviderConfig{Roots: []string{root}},
					)
					require.True(ok)
					source := requireCodexProviderSource(t, provider, tc.uuid)
					if mode == "warm" {
						prefixFingerprint, err := provider.Fingerprint(
							t.Context(), source,
						)
						require.NoError(err)
						_, err = provider.Parse(t.Context(), ParseRequest{
							Source: source, Fingerprint: prefixFingerprint,
						})
						require.NoError(err)
						appendCodexProviderContent(t, path, tail)
					}
					fingerprint, err := provider.Fingerprint(
						t.Context(), source,
					)
					require.NoError(err)
					outcome, status, err := provider.ParseIncremental(
						t.Context(),
						IncrementalRequest{
							Source:       source,
							Fingerprint:  fingerprint,
							SessionID:    "codex:" + tc.uuid,
							Offset:       int64(len(prefix)),
							StartOrdinal: 2,
						},
					)
					require.NoError(err)
					assert.Equal(IncrementalApplied, status)
					require.Len(outcome.Messages, 1)
					message := outcome.Messages[0]
					assert.Equal(RoleAssistant, message.Role)
					assert.Equal("tail answer", message.Content)
					assert.Equal(2, message.Ordinal)
					assert.Equal(tc.wantHasTokens, message.HasOutputTokens)
					assert.Equal(tc.wantOutput, message.OutputTokens)
					assert.Equal(tc.wantPeakContext, message.ContextTokens)
					assert.Equal(tc.wantOutput, outcome.TotalOutputTokens)
					assert.Equal(tc.wantPeakContext, outcome.PeakContextTokens)
					assert.Equal(tc.wantHasTokens, outcome.HasTotalOutputTokens)
					assert.Equal(tc.wantHasTokens, outcome.HasPeakContextTokens)
					assert.Equal(int64(len(tail)), outcome.ConsumedBytes)
				})
			}
		})
	}
}

func TestCodexProviderParseIncrementalInboundAgentMessage(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	const uuid = "01900000-0000-7000-8000-000000000002"
	initial := testjsonl.JoinJSONL(
		testjsonl.CodexSubagentSessionMetaJSON(
			uuid, "01900000-0000-7000-8000-000000000001",
			"/tmp/project", "user", tsEarly,
		),
		testjsonl.CodexAgentMessageJSON(
			"/root", "/root/worker", "Task received from parent.",
			"Inspect the parser.", tsEarlyS1,
		),
	)
	path := writeCodexProviderSessionContent(t, root, uuid, initial)
	provider, ok := NewProvider(
		AgentCodex, ProviderConfig{Roots: []string{root}},
	)
	require.True(ok)
	source := requireCodexProviderSource(t, provider, uuid)
	initialFingerprint, err := provider.Fingerprint(
		t.Context(), source,
	)
	require.NoError(err)
	full, err := provider.Parse(t.Context(), ParseRequest{
		Source: source, Fingerprint: initialFingerprint,
	})
	require.NoError(err)
	require.Len(full.Results, 1)
	require.Len(full.Results[0].Result.Messages, 1)

	tail := testjsonl.CodexAgentMessageJSON(
		"/root", "/root/worker", "Follow-up received from parent.",
		"Check incremental parsing too.", tsEarlyS5,
	) + "\n"
	appendCodexProviderContent(t, path, tail)
	currentFingerprint, err := provider.Fingerprint(
		t.Context(), source,
	)
	require.NoError(err)

	outcome, status, err := provider.ParseIncremental(
		t.Context(),
		IncrementalRequest{
			Source: source, Fingerprint: currentFingerprint,
			SessionID: "codex:" + uuid,
			Offset:    initialFingerprint.Size, StartOrdinal: 1,
		},
	)

	require.NoError(err)
	assert.Equal(IncrementalApplied, status)
	require.Len(outcome.Messages, 1)
	assert.Equal(RoleUser, outcome.Messages[0].Role)
	assert.Equal("Check incremental parsing too.", outcome.Messages[0].Content)
	assert.Equal(1, outcome.Messages[0].Ordinal)
}

func TestCodexProviderParseIncrementalNoLifecycleMarker(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	uuid := "019eb791-cf7d-75c1-8439-9ed74c1229e5"
	path := writeCodexProviderSession(t, root, uuid, "initial request")
	provider, ok := NewProvider(AgentCodex, ProviderConfig{Roots: []string{root}})
	require.True(ok)
	source := requireCodexProviderSource(t, provider, uuid)
	initialFingerprint, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)
	_, err = provider.Parse(t.Context(), ParseRequest{
		Source: source, Fingerprint: initialFingerprint,
	})
	require.NoError(err)

	tail := testjsonl.JoinJSONL(
		testjsonl.CodexMsgJSON("assistant", "tail answer", tsLate),
	)
	appendCodexProviderContent(t, path, tail)
	currentFingerprint, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)
	outcome, status, err := provider.ParseIncremental(
		t.Context(),
		IncrementalRequest{
			Source:       source,
			Fingerprint:  currentFingerprint,
			SessionID:    "codex:" + uuid,
			Offset:       initialFingerprint.Size,
			StartOrdinal: 1,
		},
	)

	require.NoError(err)
	assert.Equal(IncrementalApplied, status)
	require.Len(outcome.Messages, 1)
	assert.Equal("tail answer", outcome.Messages[0].Content)
	assert.Nil(outcome.TerminationStatus)
}

func TestCodexProviderParseIncrementalStagesCursorWithoutMessages(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	uuid := "019eb791-cf7d-75c1-8439-9ed74c1229eb"
	path := writeCodexProviderSession(t, root, uuid, "initial request")
	provider, ok := NewProvider(AgentCodex, ProviderConfig{Roots: []string{root}})
	require.True(ok)
	source := requireCodexProviderSource(t, provider, uuid)
	initialFingerprint, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)
	_, err = provider.Parse(t.Context(), ParseRequest{
		Source: source, Fingerprint: initialFingerprint,
	})
	require.NoError(err)

	tail := testjsonl.JoinJSONL(
		testjsonl.CodexTurnContextJSON("gpt-5.6", "2024-01-01T10:00:04Z"),
		codexEventMsgJSON("task_started", "2024-01-01T10:00:05Z"),
	)
	appendCodexProviderContent(t, path, tail)
	currentFingerprint, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)
	outcome, status, err := provider.ParseIncremental(
		t.Context(),
		IncrementalRequest{
			Source:       source,
			Fingerprint:  currentFingerprint,
			SessionID:    "codex:" + uuid,
			Offset:       initialFingerprint.Size,
			StartOrdinal: 1,
		},
	)

	require.NoError(err)
	assert.Equal(IncrementalApplied, status)
	assert.Empty(outcome.Messages)
	assert.Equal(0, outcome.MessageCount)
	assert.Equal(0, outcome.UserMessageCount)
	assert.Equal(int64(len(tail)), outcome.ConsumedBytes)
	assert.Equal(time.Date(2024, time.January, 1, 10, 0, 5, 0, time.UTC), outcome.EndedAt)
	require.NotNil(outcome.TerminationStatus)
	assert.Equal(TerminationToolCallPending, *outcome.TerminationStatus)

	concrete, ok := provider.(*codexProvider)
	require.True(ok)
	seed, staged := concrete.cursorCache.Get(
		path,
		currentFingerprint.Size,
		currentFingerprint.Inode,
		currentFingerprint.Device,
	)
	require.True(staged)
	assert.Equal("gpt-5.6", seed.model)
	assert.Equal("task_started", seed.lastTaskEvent)
}

func TestCodexProviderParseIncrementalNoDataAndTruncation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	uuid := "019eb791-cf7d-75c1-8439-9ed74c1229e6"
	writeCodexProviderSession(t, root, uuid, "initial request")
	provider, ok := NewProvider(AgentCodex, ProviderConfig{Roots: []string{root}})
	require.True(ok)
	source := requireCodexProviderSource(t, provider, uuid)
	fingerprint, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)
	_, err = provider.Parse(t.Context(), ParseRequest{
		Source: source, Fingerprint: fingerprint,
	})
	require.NoError(err)

	noData, status, err := provider.ParseIncremental(
		t.Context(),
		IncrementalRequest{
			Source:      source,
			Fingerprint: fingerprint,
			SessionID:   "codex:" + uuid,
			Offset:      fingerprint.Size,
		},
	)
	require.NoError(err)
	assert.Equal(IncrementalNoNewData, status)
	assert.Equal(IncrementalOutcome{}, noData)

	truncated, status, err := provider.ParseIncremental(
		t.Context(),
		IncrementalRequest{
			Source:      source,
			Fingerprint: fingerprint,
			SessionID:   "codex:" + uuid,
			Offset:      fingerprint.Size + 1,
		},
	)
	require.NoError(err)
	assert.Equal(IncrementalNeedsFullParse, status)
	assert.True(truncated.ForceReplace)
	assert.Empty(truncated.Messages)
}

func TestCodexProviderParseIncrementalUnsafePartialOffset(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	uuid := "019eb791-cf7d-75c1-8439-9ed74c1229e7"
	messageLine := testjsonl.CodexMsgJSON(
		"user", "record completed after the first parse", tsEarlyS1,
	)
	cut := len(messageLine) / 2
	partial := testjsonl.CodexSessionMetaJSON(
		uuid, "/home/user/code/api", "codex_cli_rs", tsEarly,
	) + "\n" + messageLine[:cut]
	path := writeCodexProviderSessionContent(t, root, uuid, partial)
	provider, ok := NewProvider(AgentCodex, ProviderConfig{Roots: []string{root}})
	require.True(ok)
	source := requireCodexProviderSource(t, provider, uuid)
	partialFingerprint, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)
	initial, err := provider.Parse(t.Context(), ParseRequest{
		Source: source, Fingerprint: partialFingerprint,
	})
	require.NoError(err)
	require.Len(initial.Results, 1)
	assert.Empty(initial.Results[0].Result.Messages)
	assert.Equal(int64(len(partial)), initial.Results[0].Result.Session.File.Size)

	appendCodexProviderContent(t, path, messageLine[cut:]+"\n")
	completeFingerprint, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)
	outcome, status, err := provider.ParseIncremental(
		t.Context(),
		IncrementalRequest{
			Source:       source,
			Fingerprint:  completeFingerprint,
			SessionID:    "codex:" + uuid,
			Offset:       partialFingerprint.Size,
			StartOrdinal: 0,
		},
	)

	require.NoError(err)
	assert.Equal(IncrementalNeedsFullParse, status)
	assert.True(outcome.ForceReplace)
	assert.Empty(outcome.Messages)

	full, err := provider.Parse(t.Context(), ParseRequest{
		Source: source, Fingerprint: completeFingerprint,
	})
	require.NoError(err)
	require.Len(full.Results, 1)
	require.Len(full.Results[0].Result.Messages, 1)
	assert.Equal("record completed after the first parse",
		full.Results[0].Result.Messages[0].Content,
	)
}

func TestCodexProviderParseIncrementalStagesCompleteRecordsOnly(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	uuid := "019eb791-cf7d-75c1-8439-9ed74c1229e8"
	path := writeCodexProviderSession(t, root, uuid, "initial request")
	provider, ok := NewProvider(AgentCodex, ProviderConfig{Roots: []string{root}})
	require.True(ok)
	source := requireCodexProviderSource(t, provider, uuid)
	initialFingerprint, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)
	_, err = provider.Parse(t.Context(), ParseRequest{
		Source: source, Fingerprint: initialFingerprint,
	})
	require.NoError(err)

	completeRecord := testjsonl.CodexMsgJSON(
		"assistant", "complete tail record", tsLate,
	) + "\n"
	deferredRecord := testjsonl.CodexMsgJSON(
		"user", "partial record completed later", tsLateS5,
	)
	deferredCut := len(deferredRecord) / 2
	appendCodexProviderContent(
		t, path, completeRecord+deferredRecord[:deferredCut],
	)
	unterminatedFingerprint, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)
	outcome, status, err := provider.ParseIncremental(
		t.Context(),
		IncrementalRequest{
			Source:       source,
			Fingerprint:  unterminatedFingerprint,
			SessionID:    "codex:" + uuid,
			Offset:       initialFingerprint.Size,
			StartOrdinal: 1,
		},
	)

	require.NoError(err)
	assert.Equal(IncrementalApplied, status)
	require.Len(outcome.Messages, 1)
	assert.Equal("complete tail record", outcome.Messages[0].Content)
	assert.Equal(1, outcome.Messages[0].Ordinal)
	assert.Equal(int64(len(completeRecord)), outcome.ConsumedBytes)
	stagedOffset := initialFingerprint.Size + int64(len(completeRecord))
	concrete, ok := provider.(*codexProvider)
	require.True(ok)
	_, stagedOK := concrete.cursorCache.Get(
		path,
		stagedOffset,
		unterminatedFingerprint.Inode,
		unterminatedFingerprint.Device,
	)
	_, eofOK := concrete.cursorCache.Get(
		path,
		unterminatedFingerprint.Size,
		unterminatedFingerprint.Inode,
		unterminatedFingerprint.Device,
	)
	assert.True(stagedOK)
	assert.False(eofOK)

	appendCodexProviderContent(t, path, deferredRecord[deferredCut:]+"\n")
	completeFingerprint, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)
	completed, status, err := provider.ParseIncremental(
		t.Context(),
		IncrementalRequest{
			Source:       source,
			Fingerprint:  completeFingerprint,
			SessionID:    "codex:" + uuid,
			Offset:       stagedOffset,
			StartOrdinal: 2,
		},
	)

	require.NoError(err)
	assert.Equal(IncrementalApplied, status)
	require.Len(completed.Messages, 1)
	assert.Equal(RoleUser, completed.Messages[0].Role)
	assert.Equal("partial record completed later", completed.Messages[0].Content)
	assert.Equal(2, completed.Messages[0].Ordinal)
	assert.Equal(int64(len(deferredRecord)+1), completed.ConsumedBytes)
}

func TestCodexProviderParseIncrementalValidEOFNeedsFullParse(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	uuid := "019eb791-cf7d-75c1-8439-9ed74c1229ea"
	path := writeCodexProviderSession(t, root, uuid, "initial request")
	provider, ok := NewProvider(AgentCodex, ProviderConfig{Roots: []string{root}})
	require.True(ok)
	source := requireCodexProviderSource(t, provider, uuid)
	initialFingerprint, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)
	_, err = provider.Parse(t.Context(), ParseRequest{
		Source: source, Fingerprint: initialFingerprint,
	})
	require.NoError(err)

	unterminatedRecord := testjsonl.CodexMsgJSON(
		"assistant", "valid record without newline", tsLate,
	)
	appendCodexProviderContent(t, path, unterminatedRecord)
	currentFingerprint, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)
	outcome, status, err := provider.ParseIncremental(
		t.Context(),
		IncrementalRequest{
			Source:       source,
			Fingerprint:  currentFingerprint,
			SessionID:    "codex:" + uuid,
			Offset:       initialFingerprint.Size,
			StartOrdinal: 1,
		},
	)

	require.NoError(err)
	assert.Equal(IncrementalNeedsFullParse, status)
	assert.True(outcome.ForceReplace)
	concrete, ok := provider.(*codexProvider)
	require.True(ok)
	_, staged := concrete.cursorCache.Get(
		path,
		currentFingerprint.Size,
		currentFingerprint.Inode,
		currentFingerprint.Device,
	)
	assert.False(staged)

	full, err := provider.Parse(t.Context(), ParseRequest{
		Source: source, Fingerprint: currentFingerprint,
	})
	require.NoError(err)
	require.Len(full.Results, 1)
	require.Len(full.Results[0].Result.Messages, 2)
	assert.Equal("valid record without newline",
		full.Results[0].Result.Messages[1].Content,
	)
}

func TestCodexProviderParseIncrementalFallbackDoesNotStage(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	uuid := "019eb791-cf7d-75c1-8439-9ed74c1229e9"
	initial := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			uuid, "/home/user/code/api", "codex_cli_rs", tsEarly,
		),
		testjsonl.CodexMsgJSON("user", "initial request", tsEarlyS1),
		testjsonl.CodexMsgJSON("assistant", "initial answer", tsLate),
	)
	path := writeCodexProviderSessionContent(t, root, uuid, initial)
	provider, ok := NewProvider(AgentCodex, ProviderConfig{Roots: []string{root}})
	require.True(ok)
	source := requireCodexProviderSource(t, provider, uuid)
	initialFingerprint, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)
	concrete, ok := provider.(*codexProvider)
	require.True(ok)

	tail := testjsonl.JoinJSONL(
		testjsonl.CodexTokenCountJSON(tsLateS5, 100_000, 250, 64_000),
	)
	appendCodexProviderContent(t, path, tail)
	currentFingerprint, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)
	outcome, status, err := provider.ParseIncremental(
		t.Context(),
		IncrementalRequest{
			Source:       source,
			Fingerprint:  currentFingerprint,
			SessionID:    "codex:" + uuid,
			Offset:       initialFingerprint.Size,
			StartOrdinal: 2,
		},
	)

	require.NoError(err)
	assert.Equal(IncrementalNeedsFullParse, status)
	assert.True(outcome.ForceReplace)
	_, oldStaged := concrete.cursorCache.Get(
		path,
		initialFingerprint.Size,
		currentFingerprint.Inode,
		currentFingerprint.Device,
	)
	_, newStaged := concrete.cursorCache.Get(
		path,
		currentFingerprint.Size,
		currentFingerprint.Inode,
		currentFingerprint.Device,
	)
	assert.False(oldStaged)
	assert.False(newStaged)
}

func TestCodexProviderColdSeedErrorDoesNotStage(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	uuid := "019eb791-cf7d-75c1-8439-9ed74c1229f0"
	prefix := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			uuid, "/workspace/project-a", "codex_cli_rs", tsEarly,
		),
		testjsonl.CodexMsgJSON("user", "persisted request", tsEarlyS1),
	)
	tail := testjsonl.JoinJSONL(
		testjsonl.CodexMsgJSON("assistant", "new answer", tsLate),
	)
	path := writeCodexProviderSessionContent(t, root, uuid, prefix+tail)
	provider, ok := NewProvider(AgentCodex, ProviderConfig{Roots: []string{root}})
	require.True(ok)
	concrete, ok := provider.(*codexProvider)
	require.True(ok)
	source := requireCodexProviderSource(t, provider, uuid)
	fingerprint, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)
	wantErr := errors.New("cold prefix failed")
	tailReadCalled := false

	result, err := concrete.parseSessionFromWithReaders(
		path,
		int64(len(prefix)),
		1,
		false,
		func(string, int64, func(string)) (int64, error) {
			tailReadCalled = true
			return int64(len(tail)), nil
		},
		func(string, int64) (codexIncrementalSeed, error) {
			return codexIncrementalSeed{}, wantErr
		},
	)

	require.ErrorIs(err, wantErr)
	assert.Empty(result.messages)
	assert.False(tailReadCalled)
	_, oldStaged := concrete.cursorCache.Get(
		path,
		int64(len(prefix)),
		fingerprint.Inode,
		fingerprint.Device,
	)
	_, newStaged := concrete.cursorCache.Get(
		path,
		fingerprint.Size,
		fingerprint.Inode,
		fingerprint.Device,
	)
	assert.False(oldStaged)
	assert.False(newStaged)
}

func TestCodexProviderParseIncrementalHonorsContext(t *testing.T) {
	provider, ok := NewProvider(AgentCodex, ProviderConfig{})
	require.True(t, ok)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, status, err := provider.ParseIncremental(ctx, IncrementalRequest{})

	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, IncrementalUnsupported, status)
}

func TestCodexProviderRawDiscoveryKeepsLiveAndArchivedPhysicalSources(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	base := t.TempDir()
	liveRoot := filepath.Join(base, "sessions")
	archivedRoot := filepath.Join(base, "archived_sessions")
	uuid := "019eb791-cf7d-75c1-8439-9ed74c1229e5"
	livePath := writeCodexProviderSession(t, liveRoot, uuid, "live")
	archivedPath := writeCodexProviderArchivedSession(
		t, archivedRoot, uuid, "archived",
	)
	sharedMeta := testjsonl.CodexSessionMetaJSON(
		uuid, "/home/user/code/api", "codex_cli_rs", "2026-06-11T12:44:06Z",
	) + "\n"
	require.NoError(os.WriteFile(livePath, []byte(sharedMeta), 0o644))
	require.NoError(os.WriteFile(archivedPath, []byte(sharedMeta), 0o644))
	renamedArchivedPath := filepath.Join(
		archivedRoot, "rollout-2026-06-12T08-00-00-"+uuid+".jsonl",
	)
	require.NoError(os.Rename(archivedPath, renamedArchivedPath))
	archivedPath = renamedArchivedPath

	provider, ok := NewProvider(AgentCodex, ProviderConfig{
		Roots: []string{archivedRoot, liveRoot},
	})
	require.True(ok)
	discovered, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(discovered, 1)
	assert.Equal(livePath, discovered[0].DisplayPath)
	assert.NotEqual(archivedPath, discovered[0].DisplayPath)

	rawDiscovery, err := DiscoverRawCaptureSources(t.Context(), provider)
	require.NoError(err)
	require.True(rawDiscovery.Complete)
	require.Len(rawDiscovery.Sources, 2)
	assert.ElementsMatch([]string{archivedPath, livePath},
		sourceDisplayPaths(rawDiscovery.Sources))
}

func TestCodexProviderDiscoverEachYieldsDuplicateCandidates(t *testing.T) {
	require := require.New(t)

	base := t.TempDir()
	liveRoot := filepath.Join(base, "sessions")
	archivedRoot := filepath.Join(base, "archived_sessions")
	uuid := "019eb791-cf7d-75c1-8439-9ed74c1229e5"
	livePath := writeCodexProviderSession(t, liveRoot, uuid, "live")
	archivedPath := writeCodexProviderArchivedSession(t, archivedRoot, uuid, "archived")
	provider, ok := NewProvider(AgentCodex, ProviderConfig{
		Roots: []string{archivedRoot, liveRoot},
	})
	require.True(ok)
	discoverer, ok := provider.(StreamingDiscoverer)
	require.True(ok)

	var paths []string
	require.NoError(discoverer.DiscoverEach(t.Context(), func(source SourceRef) error {
		paths = append(paths, source.DisplayPath)
		return nil
	}))

	assert.ElementsMatch(t, []string{archivedPath, livePath}, paths)
}

func TestCodexRawCaptureDiscoveryContinuesAfterIncompleteRoot(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	unreadableRoot := t.TempDir()
	healthyRoot := t.TempDir()
	uuid := "019eb791-cf7d-75c1-8439-9ed74c1229e5"
	healthyPath := writeCodexProviderSession(t, healthyRoot, uuid, "healthy")
	provider, ok := NewProvider(AgentCodex, ProviderConfig{
		Roots: []string{unreadableRoot, healthyRoot},
	})
	require.True(ok)
	ctx := withStreamingDirectoryReader(t.Context(), func(
		ctx context.Context,
		dir string,
		yield func(os.DirEntry) error,
	) error {
		if dir == unreadableRoot {
			return os.ErrPermission
		}
		return streamDirectoryEntriesDirect(ctx, dir, yield)
	})

	discovery, err := DiscoverRawCaptureSources(ctx, provider)
	require.Error(err)
	_, incomplete := errors.AsType[DiscoveryIncompleteError](err)
	require.True(incomplete)
	assert.False(discovery.Complete)
	require.Len(discovery.Sources, 1)
	assert.Equal(healthyPath, discovery.Sources[0].DisplayPath)
}

func TestCodexProviderDiscoverEachIncludesNoncanonicalRollout(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	path := filepath.Join(
		root, "2026", "06", "11", "rollout-manually-restored.jsonl",
	)
	uuid := "019eb791-cf7d-75c1-8439-9ed74c1229ef"
	content := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			uuid, "/workspace/project-a", "codex_cli_rs", tsEarly,
		),
		testjsonl.CodexMsgJSON("user", "Restore this session", tsEarlyS1),
	)
	require.NoError(os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(os.WriteFile(path, []byte(content), 0o644))
	provider, ok := NewProvider(AgentCodex, ProviderConfig{Roots: []string{root}})
	require.True(ok)

	discovered, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(discovered, 1)
	changed, err := provider.SourcesForChangedPath(
		t.Context(), ChangedPathRequest{Path: path, EventKind: "write"},
	)
	require.NoError(err)
	require.Len(changed, 1)
	discoverer, ok := provider.(StreamingDiscoverer)
	require.True(ok)
	var streamed []SourceRef
	require.NoError(discoverer.DiscoverEach(
		t.Context(), func(source SourceRef) error {
			streamed = append(streamed, source)
			return nil
		},
	))

	assert.Equal([]string{path}, sourceDisplayPaths(discovered))
	assert.Equal([]string{path}, sourceDisplayPaths(changed))
	assert.Equal([]string{path}, sourceDisplayPaths(streamed),
		"streaming discovery must preserve direct-path rollout fallback parity")
}

func TestCodexProviderDiscoverEachExcludesNoncanonicalRolloutOutsideSupportedLayouts(
	t *testing.T,
) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	path := filepath.Join(
		root, "arbitrary", "nesting", "rollout-manually-restored.jsonl",
	)
	require.NoError(os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(os.WriteFile(path, []byte("{}\n"), 0o644))
	provider, ok := NewProvider(AgentCodex, ProviderConfig{Roots: []string{root}})
	require.True(ok)

	discovered, err := provider.Discover(t.Context())
	require.NoError(err)
	assert.Empty(discovered)
	discoverer, ok := provider.(StreamingDiscoverer)
	require.True(ok)
	var streamed []SourceRef
	require.NoError(discoverer.DiscoverEach(
		t.Context(), func(source SourceRef) error {
			streamed = append(streamed, source)
			return nil
		},
	))

	assert.Empty(streamed,
		"streaming discovery must preserve slice discovery's layout boundary")
}

func TestCodexProviderFindSourcePinsExactArchivedDuplicate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	base := t.TempDir()
	liveRoot := filepath.Join(base, "sessions")
	archivedRoot := filepath.Join(base, "archived_sessions")
	uuid := "019eb791-cf7d-75c1-8439-9ed74c1229e6"
	livePath := writeCodexProviderSession(t, liveRoot, uuid, "live")
	archivedPath := writeCodexProviderArchivedSession(
		t, archivedRoot, uuid, "archived",
	)

	provider, ok := NewProvider(AgentCodex, ProviderConfig{
		Roots: []string{archivedRoot, liveRoot},
	})
	require.True(ok)

	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		StoredFilePath: archivedPath,
		FullSessionID:  "codex:" + uuid,
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(archivedPath, found.DisplayPath)

	found, ok, err = provider.FindSource(t.Context(), FindSourceRequest{
		FullSessionID: "codex:" + uuid,
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(livePath, found.DisplayPath)
}

func TestCodexProviderFindSourcePreferStoredSourceKeepsArchivedDuplicate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	base := t.TempDir()
	liveRoot := filepath.Join(base, "sessions")
	archivedRoot := filepath.Join(base, "archived_sessions")
	uuid := "019eb791-cf7d-75c1-8439-9ed74c1229e6"
	livePath := writeCodexProviderSession(t, liveRoot, uuid, "live")
	archivedPath := writeCodexProviderArchivedSession(
		t, archivedRoot, uuid, "archived",
	)

	provider, ok := NewProvider(AgentCodex, ProviderConfig{
		Roots: []string{archivedRoot, liveRoot},
	})
	require.True(ok)

	// PreferStoredSource pins the stored archived duplicate even when a fresh
	// source is required, instead of canonicalizing to the live duplicate.
	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		StoredFilePath:     archivedPath,
		FullSessionID:      "codex:" + uuid,
		RequireFreshSource: true,
		PreferStoredSource: true,
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(archivedPath, found.DisplayPath,
		"PreferStoredSource must preserve the stored archived path")

	// Without the hint, RequireFreshSource canonicalizes to the live duplicate,
	// which is exactly the behavior PreferStoredSource opts out of.
	found, ok, err = provider.FindSource(t.Context(), FindSourceRequest{
		StoredFilePath:     archivedPath,
		FullSessionID:      "codex:" + uuid,
		RequireFreshSource: true,
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(livePath, found.DisplayPath,
		"RequireFreshSource without PreferStoredSource canonicalizes to live")
}

func TestCodexProviderFindSourceAcceptsLegacyShapedStoredPath(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	sessionID := "test-uuid"
	sourcePath := filepath.Join(
		root,
		"2024",
		"01",
		"15",
		"rollout-20240115-"+sessionID+".jsonl",
	)
	content := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			sessionID,
			"/home/user/code/api",
			"codex_cli_rs",
			tsEarly,
		),
		testjsonl.CodexMsgJSON("user", "Add tests", tsEarlyS1),
	)
	require.NoError(os.MkdirAll(filepath.Dir(sourcePath), 0o755))
	require.NoError(os.WriteFile(sourcePath, []byte(content), 0o644))

	provider, ok := NewProvider(AgentCodex, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(ok)

	discovered, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(discovered, 1)
	assert.Equal(sourcePath, discovered[0].DisplayPath)

	changed, err := provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{Path: sourcePath, EventKind: "write"},
	)
	require.NoError(err)
	require.Len(changed, 1)
	assert.Equal(sourcePath, changed[0].DisplayPath)

	source, found, err := provider.FindSource(t.Context(), FindSourceRequest{
		StoredFilePath: sourcePath,
		FingerprintKey: sourcePath,
	})
	require.NoError(err)
	require.True(found)
	assert.Equal(AgentCodex, source.Provider)
	assert.Equal(sourcePath, source.DisplayPath)
	assert.Equal(sourcePath, source.FingerprintKey)

	fingerprint, err := provider.Fingerprint(t.Context(), source)
	require.NoError(err)
	assert.Equal(sourcePath, fingerprint.Key)
	assert.NotEmpty(fingerprint.Hash)

	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source:      source,
		Fingerprint: fingerprint,
		Machine:     "devbox",
	})
	require.NoError(err)
	require.True(outcome.ResultSetComplete)
	require.Len(outcome.Results, 1)
	result := outcome.Results[0]
	assert.Equal("codex:"+sessionID, result.Result.Session.ID)
	assert.Equal("api", result.Result.Session.Project)
	assert.Equal("devbox", result.Result.Session.Machine)
	assert.Equal(fingerprint.Hash, result.Result.Session.File.Hash)
	assert.Len(result.Result.Messages, 1)
}

func TestCodexProviderChangedPathPinsArchivedDuplicate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	base := t.TempDir()
	liveRoot := filepath.Join(base, "sessions")
	archivedRoot := filepath.Join(base, "archived_sessions")
	uuid := "019eb791-cf7d-75c1-8439-9ed74c1229e7"
	_ = writeCodexProviderSession(t, liveRoot, uuid, "live")
	archivedPath := writeCodexProviderArchivedSession(
		t, archivedRoot, uuid, "archived",
	)

	provider, ok := NewProvider(AgentCodex, ProviderConfig{
		Roots: []string{archivedRoot, liveRoot},
	})
	require.True(ok)
	changed, err := provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{Path: archivedPath, EventKind: "write"},
	)
	require.NoError(err)
	require.Len(changed, 1)
	assert.Equal(archivedPath, changed[0].DisplayPath)

	rawChanged, err := RawCaptureSourcesForChangedPath(
		t.Context(), provider,
		ChangedPathRequest{Path: archivedPath, EventKind: "write"},
	)
	require.NoError(err)
	require.Len(rawChanged, 1)
	assert.Equal(archivedPath, rawChanged[0].DisplayPath)
}

func TestCodexProviderChangedPathClassifiesRemovedTranscript(t *testing.T) {
	require := require.New(t)

	root := t.TempDir()
	uuid := "019eb791-cf7d-75c1-8439-9ed74c1229e8"
	sourcePath := writeCodexProviderSession(t, root, uuid, "remove")
	provider, ok := NewProvider(AgentCodex, ProviderConfig{Roots: []string{root}})
	require.True(ok)
	require.NoError(os.Remove(sourcePath))

	changed, err := provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{Path: sourcePath, EventKind: "remove"},
	)
	require.NoError(err)
	require.Len(changed, 1)
	assert.Equal(t, sourcePath, changed[0].DisplayPath)
}

func TestCodexProviderIndexPathClassifiesAllSiblingSources(t *testing.T) {
	require := require.New(t)

	base := t.TempDir()
	root := filepath.Join(base, "sessions")
	firstUUID := "019eb791-cf7d-75c1-8439-9ed74c1229e9"
	secondUUID := "019eb791-cf7d-75c1-8439-9ed74c1229ea"
	firstPath := writeCodexProviderSession(t, root, firstUUID, "first")
	secondPath := writeCodexProviderSession(t, root, secondUUID, "second")
	indexPath := filepath.Join(base, CodexSessionIndexFilename)
	require.NoError(os.WriteFile(indexPath, []byte(
		`{"id":"`+firstUUID+`","thread_name":"Only first remains","updated_at":"2026-06-11T17:34:20Z"}`+"\n",
	), 0o644))

	provider, ok := NewProvider(AgentCodex, ProviderConfig{Roots: []string{root}})
	require.True(ok)
	changed, err := provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{Path: indexPath, EventKind: "write"},
	)
	require.NoError(err)
	assert.Equal(t, []string{firstPath, secondPath}, sourceDisplayPaths(changed))
}

func TestCodexProviderRawIndexPathDefersToBoundedAudit(t *testing.T) {
	require := require.New(t)

	base := t.TempDir()
	root := filepath.Join(base, "sessions")
	writeCodexProviderSession(
		t, root, "019eb791-cf7d-75c1-8439-9ed74c1229e9", "first",
	)
	writeCodexProviderSession(
		t, root, "019eb791-cf7d-75c1-8439-9ed74c1229ea", "second",
	)
	indexPath := filepath.Join(base, CodexSessionIndexFilename)
	require.NoError(os.WriteFile(indexPath, []byte("{}\n"), 0o644))
	provider, ok := NewProvider(AgentCodex, ProviderConfig{Roots: []string{root}})
	require.True(ok)

	changed, err := RawCaptureSourcesForChangedPath(
		t.Context(), provider, ChangedPathRequest{
			Path: indexPath, EventKind: "write", WatchRoot: base,
		},
	)

	require.NoError(err)
	assert.Empty(t, changed)
}

func writeCodexProviderSession(
	t *testing.T,
	root, uuid, prompt string,
) string {
	t.Helper()
	content := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(uuid, "/home/user/code/api", "codex_cli_rs", tsEarly),
		testjsonl.CodexMsgJSON("user", prompt, tsEarlyS1),
	)
	return writeCodexProviderSessionContent(t, root, uuid, content)
}

func writeCodexProviderArchivedSession(
	t *testing.T,
	root, uuid, prompt string,
) string {
	t.Helper()
	content := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(uuid, "/home/user/code/archive", "codex_cli_rs", tsEarly),
		testjsonl.CodexMsgJSON("user", prompt, tsEarlyS1),
	)
	path := filepath.Join(root, "rollout-2026-06-11T12-44-06-"+uuid+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	return path
}

func writeCodexProviderSessionContent(
	t *testing.T,
	root, uuid, content string,
) string {
	t.Helper()
	path := filepath.Join(
		root,
		"2026",
		"06",
		"11",
		"rollout-2026-06-11T12-44-06-"+uuid+".jsonl",
	)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	return path
}

func requireCodexProviderSource(
	t *testing.T,
	provider Provider,
	uuid string,
) SourceRef {
	t.Helper()
	source, found, err := provider.FindSource(
		t.Context(),
		FindSourceRequest{FullSessionID: "codex:" + uuid},
	)
	require.NoError(t, err)
	require.True(t, found)
	return source
}

func appendCodexProviderContent(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = f.WriteString(content)
	require.NoError(t, err)
	require.NoError(t, f.Close())
}

func TestCodexProviderIncrementalUserBoundaryDoesNotBackfillCommittedUsage(t *testing.T) {
	const uuid = "019eb791-cf7d-75c1-8439-9ed74c1229f9"
	prefix := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			uuid, "/workspace/project-a", "codex_cli_rs", tsEarly,
		),
		testjsonl.CodexTurnContextJSON("gpt-5.4", tsEarlyS1),
		testjsonl.CodexMsgJSON("user", "first request", tsEarlyS1),
		testjsonl.CodexMsgJSON("assistant", "first response", tsEarlyS5),
	)

	t.Run("user without a new assistant blocks committed target", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)

		root := t.TempDir()
		path := writeCodexProviderSessionContent(t, root, uuid, prefix)
		provider, ok := NewProvider(AgentCodex, ProviderConfig{Roots: []string{root}})
		require.True(ok)
		source := requireCodexProviderSource(t, provider, uuid)
		prefixFingerprint, err := provider.Fingerprint(t.Context(), source)
		require.NoError(err)

		tail := testjsonl.JoinJSONL(
			testjsonl.CodexMsgJSON("user", "second request", tsLate),
			testjsonl.CodexTokenCountJSON(tsLateS5, 100_000, 250, 64_000),
		)
		appendCodexProviderContent(t, path, tail)
		fingerprint, err := provider.Fingerprint(t.Context(), source)
		require.NoError(err)
		pendingUsageOrdinal := 1
		outcome, status, err := provider.ParseIncremental(
			t.Context(), IncrementalRequest{
				Source:                    source,
				Fingerprint:               fingerprint,
				SessionID:                 "codex:" + uuid,
				Offset:                    prefixFingerprint.Size,
				StartOrdinal:              2,
				StoredPendingUsageOrdinal: &pendingUsageOrdinal,
			},
		)
		require.NoError(err)
		assert.Equal(IncrementalApplied, status)
		require.Len(outcome.Messages, 1)
		assert.Equal(RoleUser, outcome.Messages[0].Role)
		assert.Empty(outcome.MessageTokenUsageUpdates,
			"usage after a real user boundary must not update the prior turn")
	})

	t.Run("new assistant after the user receives usage", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)

		root := t.TempDir()
		path := writeCodexProviderSessionContent(t, root, uuid, prefix)
		provider, ok := NewProvider(AgentCodex, ProviderConfig{Roots: []string{root}})
		require.True(ok)
		source := requireCodexProviderSource(t, provider, uuid)
		prefixFingerprint, err := provider.Fingerprint(t.Context(), source)
		require.NoError(err)

		tail := testjsonl.JoinJSONL(
			testjsonl.CodexMsgJSON("user", "second request", tsLate),
			testjsonl.CodexMsgJSON("assistant", "second response", tsLateS5),
			testjsonl.CodexTokenCountJSON("2026-08-02T09:00:06Z", 100_000, 250, 64_000),
		)
		appendCodexProviderContent(t, path, tail)
		fingerprint, err := provider.Fingerprint(t.Context(), source)
		require.NoError(err)
		pendingUsageOrdinal := 1
		outcome, status, err := provider.ParseIncremental(
			t.Context(), IncrementalRequest{
				Source:                    source,
				Fingerprint:               fingerprint,
				SessionID:                 "codex:" + uuid,
				Offset:                    prefixFingerprint.Size,
				StartOrdinal:              2,
				StoredPendingUsageOrdinal: &pendingUsageOrdinal,
			},
		)
		require.NoError(err)
		assert.Equal(IncrementalApplied, status)
		require.Len(outcome.Messages, 2)
		assert.Equal(RoleAssistant, outcome.Messages[1].Role)
		assert.NotEmpty(outcome.Messages[1].TokenUsage)
		assert.Empty(outcome.MessageTokenUsageUpdates)
	})
}
