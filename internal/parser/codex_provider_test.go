package parser

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestCodexProviderSourceMethods(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "sessions")
	uuid := "019eb791-cf7d-75c1-8439-9ed74c1229e1"
	sourcePath := writeCodexProviderSession(t, root, uuid, "Rename me")
	indexPath := filepath.Join(base, CodexSessionIndexFilename)
	require.NoError(t, os.WriteFile(indexPath, []byte(
		`{"id":"`+uuid+`","thread_name":"Renamed title","updated_at":"2026-06-11T17:34:20Z"}`+"\n",
	), 0o644))
	newer := time.Now().Add(time.Hour)
	require.NoError(t, os.Chtimes(indexPath, newer, newer))

	provider, ok := NewProvider(AgentCodex, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	plan, err := provider.WatchPlan(context.Background())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 2)
	assert.Equal(t, root, plan.Roots[0].Path)
	assert.True(t, plan.Roots[0].Recursive)
	assert.Equal(t, []string{"*.jsonl"}, plan.Roots[0].IncludeGlobs)
	assert.Equal(t, base, plan.Roots[1].Path)
	assert.False(t, plan.Roots[1].Recursive)
	assert.Equal(t, []string{CodexSessionIndexFilename}, plan.Roots[1].IncludeGlobs)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	source := discovered[0]
	assert.Equal(t, AgentCodex, source.Provider)
	assert.Equal(t, sourcePath, source.DisplayPath)
	assert.Equal(t, sourcePath, source.FingerprintKey)

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		FullSessionID: "host~codex:" + uuid,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, sourcePath, found.DisplayPath)

	for _, path := range []string{sourcePath, indexPath} {
		changed, err := provider.SourcesForChangedPath(
			context.Background(),
			ChangedPathRequest{Path: path, EventKind: "write"},
		)
		require.NoError(t, err)
		require.Len(t, changed, 1)
		assert.Equal(t, sourcePath, changed[0].DisplayPath)
	}

	info, err := os.Stat(sourcePath)
	require.NoError(t, err)
	fingerprint, err := provider.Fingerprint(context.Background(), found)
	require.NoError(t, err)
	assert.Equal(t, sourcePath, fingerprint.Key)
	assert.Equal(t, info.Size(), fingerprint.Size)
	assert.Equal(t, newer.UnixNano(), fingerprint.MTimeNS)
	wantInode, wantDevice := sourceFileIdentity(info)
	assert.Equal(t, wantInode, fingerprint.Inode)
	assert.Equal(t, wantDevice, fingerprint.Device)
	assert.NotEmpty(t, fingerprint.Hash)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source:      found,
		Fingerprint: fingerprint,
	})
	require.NoError(t, err)
	require.True(t, outcome.ResultSetComplete)
	require.Len(t, outcome.Results, 1)
	result := outcome.Results[0]
	assert.Equal(t, DataVersionCurrent, result.DataVersion)
	assert.Equal(t, "codex:"+uuid, result.Result.Session.ID)
	assert.Equal(t, AgentCodex, result.Result.Session.Agent)
	assert.Equal(t, "devbox", result.Result.Session.Machine)
	assert.Equal(t, "api", result.Result.Session.Project)
	assert.Equal(t, "Renamed title", result.Result.Session.SessionName)
	assert.Equal(t, fingerprint.Hash, result.Result.Session.File.Hash)
	assert.Len(t, result.Result.Messages, 1)
}

func TestCodexProviderForceParseReloadsSameStatSessionIndex(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "sessions")
	uuid := "019eb791-cf7d-75c1-8439-9ed74c1229f3"
	writeCodexProviderSession(t, root, uuid, "keep this prompt")
	indexPath := filepath.Join(base, CodexSessionIndexFilename)
	original := `{"id":"` + uuid + `","thread_name":"Alpha title"}` + "\n"
	rewritten := `{"id":"` + uuid + `","thread_name":"Bravo title"}` + "\n"
	require.Len(t, rewritten, len(original), "index fixtures must have equal length")
	require.NoError(t, os.WriteFile(indexPath, []byte(original), 0o644))
	stableTime := time.Unix(1_800_000_000, 123_000_000)
	require.NoError(t, os.Chtimes(indexPath, stableTime, stableTime))

	provider, ok := NewProvider(AgentCodex, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	source := requireCodexProviderSource(t, provider, uuid)
	fingerprint, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	first, err := provider.Parse(context.Background(), ParseRequest{
		Source: source, Fingerprint: fingerprint,
	})
	require.NoError(t, err)
	require.Len(t, first.Results, 1)
	assert.Equal(t, "Alpha title", first.Results[0].Result.Session.SessionName)

	before, err := os.Stat(indexPath)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(indexPath, []byte(rewritten), 0o644))
	require.NoError(t, os.Chtimes(indexPath, before.ModTime(), before.ModTime()))
	after, err := os.Stat(indexPath)
	require.NoError(t, err)
	require.Equal(t, before.Size(), after.Size(), "index size must stay unchanged")
	require.Equal(t, before.ModTime(), after.ModTime(), "index mtime must stay unchanged")

	forced, err := provider.Parse(context.Background(), ParseRequest{
		Source:      source,
		Fingerprint: fingerprint,
		ForceParse:  true,
	})

	require.NoError(t, err)
	require.Len(t, forced.Results, 1)
	assert.Equal(t, "Bravo title", forced.Results[0].Result.Session.SessionName)
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
	def, ok := AgentByType(AgentCodex)
	require.True(t, ok)
	root := t.TempDir()
	uuid := "019eb791-cf7d-75c1-8439-9ed74c1229e3"
	path := writeCodexProviderSession(t, root, uuid, "seed shared cache")

	sharedFactory := newCodexProviderFactory(def)
	seedingProvider, ok := sharedFactory.NewProvider(ProviderConfig{
		Roots: []string{root},
	}).(*codexProvider)
	require.True(t, ok)
	siblingProvider, ok := sharedFactory.NewProvider(ProviderConfig{
		Roots: []string{root},
	}).(*codexProvider)
	require.True(t, ok)
	isolatedFactory := newCodexProviderFactory(def)
	isolatedProvider, ok := isolatedFactory.NewProvider(ProviderConfig{
		Roots: []string{root},
	}).(*codexProvider)
	require.True(t, ok)

	sess, _, err := seedingProvider.parseSession(path, "local", false)
	require.NoError(t, err)
	require.NotNil(t, sess)
	info, err := os.Stat(path)
	require.NoError(t, err)
	inode, device := sourceFileIdentity(info)

	_, siblingHit := siblingProvider.cursorCache.Get(
		path, info.Size(), inode, device,
	)
	_, isolatedHit := isolatedProvider.cursorCache.Get(
		path, info.Size(), inode, device,
	)
	assert.True(t, siblingHit)
	assert.False(t, isolatedHit)
}

func TestCodexProviderFullParseSnapshotExcludesLaterGrowth(t *testing.T) {
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
	require.True(t, ok)
	concrete, ok := provider.(*codexProvider)
	require.True(t, ok)
	source := requireCodexProviderSource(t, provider, uuid)

	snapshot, err := os.Open(path)
	require.NoError(t, err)
	defer snapshot.Close()
	capturedInfo, err := snapshot.Stat()
	require.NoError(t, err)
	tail := testjsonl.JoinJSONL(
		testjsonl.CodexTurnContextJSON("gpt-5.5", "2024-01-01T10:00:04Z"),
		testjsonl.CodexMsgJSON("assistant", "later answer", "2024-01-01T10:00:05Z"),
		codexEventMsgJSON("task_started", "2024-01-01T10:00:06Z"),
	)
	appendCodexProviderContent(t, path, tail)

	sess, messages, err := concrete.parseSessionSnapshot(
		path, "local", false, snapshot, capturedInfo,
	)

	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, int64(len(initial)), sess.File.Size)
	require.Len(t, messages, 1)
	assert.Equal(t, RoleUser, messages[0].Role)
	assert.Equal(t, "captured request", messages[0].Content)
	inode, device := sourceFileIdentity(capturedInfo)
	seed, seeded := concrete.cursorCache.Get(
		path, int64(len(initial)), inode, device,
	)
	require.True(t, seeded)
	assert.Equal(t, "gpt-5.4", seed.model)
	assert.Equal(t, "task_complete", seed.lastTaskEvent)

	currentFingerprint, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	outcome, status, err := provider.ParseIncremental(
		context.Background(),
		IncrementalRequest{
			Source:       source,
			Fingerprint:  currentFingerprint,
			SessionID:    "codex:" + uuid,
			Offset:       int64(len(initial)),
			StartOrdinal: 1,
		},
	)

	require.NoError(t, err)
	assert.Equal(t, IncrementalApplied, status)
	require.Len(t, outcome.Messages, 1)
	assert.Equal(t, RoleAssistant, outcome.Messages[0].Role)
	assert.Equal(t, "later answer", outcome.Messages[0].Content)
	assert.Equal(t, 1, outcome.Messages[0].Ordinal)
	assert.Equal(t, "gpt-5.5", outcome.Messages[0].Model)
	assert.Equal(t, int64(len(tail)), outcome.ConsumedBytes)
}

func TestCodexProviderFullParseSnapshotKeepsDescriptorIdentityAfterReplacement(
	t *testing.T,
) {
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
	require.True(t, ok)
	concrete, ok := provider.(*codexProvider)
	require.True(t, ok)

	snapshot, err := os.Open(path)
	require.NoError(t, err)
	defer snapshot.Close()
	snapshotInfo, err := snapshot.Stat()
	require.NoError(t, err)
	oldInode, oldDevice := sourceFileIdentity(snapshotInfo)

	replacementPath := path + ".replacement"
	require.NoError(t, os.WriteFile(replacementPath, []byte(replacement), 0o644))
	require.NoError(t, os.Rename(replacementPath, path))
	currentInfo, err := os.Stat(path)
	require.NoError(t, err)
	newInode, newDevice := sourceFileIdentity(currentInfo)

	sess, messages, err := concrete.parseSessionSnapshot(
		path, "local", false, snapshot, snapshotInfo,
	)

	require.NoError(t, err)
	require.NotNil(t, sess)
	require.Len(t, messages, 1)
	assert.Equal(t, "old snapshot", messages[0].Content)
	assert.Equal(t, "/workspace/original", sess.Cwd)
	assert.Equal(t, int64(oldInode), sess.File.Inode)
	assert.Equal(t, int64(oldDevice), sess.File.Device)
	_, oldCursor := concrete.cursorCache.Get(
		path, snapshotInfo.Size(), oldInode, oldDevice,
	)
	assert.True(t, oldCursor, "snapshot cursor must use the descriptor identity")
	if oldInode != newInode || oldDevice != newDevice {
		_, replacementCursor := concrete.cursorCache.Get(
			path, snapshotInfo.Size(), newInode, newDevice,
		)
		assert.False(t, replacementCursor,
			"old snapshot state must not be keyed to the replacement identity")
	}
}

func TestCodexProviderIncrementalSnapshotExcludesLaterGrowth(t *testing.T) {
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
	require.True(t, ok)
	concrete, ok := provider.(*codexProvider)
	require.True(t, ok)
	source := requireCodexProviderSource(t, provider, uuid)
	prefixFingerprint, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	_, err = provider.Parse(context.Background(), ParseRequest{
		Source: source, Fingerprint: prefixFingerprint,
	})
	require.NoError(t, err)

	capturedTail := testjsonl.JoinJSONL(
		testjsonl.CodexMsgJSON("assistant", "captured answer", "2024-01-01T10:00:03Z"),
	)
	appendCodexProviderContent(t, path, capturedTail)
	capturedFingerprint, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	laterTail := testjsonl.JoinJSONL(
		testjsonl.CodexMsgJSON("assistant", "later answer", "2024-01-01T10:00:04Z"),
	)
	appendCodexProviderContent(t, path, laterTail)

	first, status, err := provider.ParseIncremental(
		context.Background(),
		IncrementalRequest{
			Source:       source,
			Fingerprint:  capturedFingerprint,
			SessionID:    "codex:" + uuid,
			Offset:       int64(len(prefix)),
			StartOrdinal: 1,
		},
	)

	require.NoError(t, err)
	assert.Equal(t, IncrementalApplied, status)
	require.Len(t, first.Messages, 1)
	assert.Equal(t, "captured answer", first.Messages[0].Content)
	assert.Equal(t, 1, first.Messages[0].Ordinal)
	assert.Equal(t, int64(len(capturedTail)), first.ConsumedBytes)
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
	assert.True(t, capturedStaged)
	assert.False(t, laterStaged)

	currentFingerprint, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	second, status, err := provider.ParseIncremental(
		context.Background(),
		IncrementalRequest{
			Source:       source,
			Fingerprint:  currentFingerprint,
			SessionID:    "codex:" + uuid,
			Offset:       capturedFingerprint.Size,
			StartOrdinal: 2,
		},
	)

	require.NoError(t, err)
	assert.Equal(t, IncrementalApplied, status)
	require.Len(t, second.Messages, 1)
	assert.Equal(t, "later answer", second.Messages[0].Content)
	assert.Equal(t, 2, second.Messages[0].Ordinal)
	assert.Equal(t, int64(len(laterTail)), second.ConsumedBytes)
}

func TestCodexProviderIncrementalSnapshotKeepsCapturedEOFConservative(t *testing.T) {
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
	require.True(t, ok)
	concrete, ok := provider.(*codexProvider)
	require.True(t, ok)
	source := requireCodexProviderSource(t, provider, uuid)
	capturedFingerprint, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	later := testjsonl.CodexMsgJSON(
		"assistant", "later answer", tsLateS5,
	) + "\n"
	appendCodexProviderContent(t, path, "\n"+later)

	outcome, status, err := provider.ParseIncremental(
		context.Background(),
		IncrementalRequest{
			Source:       source,
			Fingerprint:  capturedFingerprint,
			SessionID:    "codex:" + uuid,
			Offset:       int64(len(prefix)),
			StartOrdinal: 1,
		},
	)

	require.NoError(t, err)
	assert.Equal(t, IncrementalNeedsFullParse, status)
	assert.True(t, outcome.ForceReplace)
	assert.Empty(t, outcome.Messages)
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
	assert.False(t, oldStaged)
	assert.False(t, newStaged)
}

func TestCodexProviderIncrementalRejectsFingerprintIdentityMismatch(t *testing.T) {
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
	require.True(t, ok)
	source := requireCodexProviderSource(t, provider, uuid)
	fingerprint, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	fingerprint.Inode++
	if fingerprint.Inode == 0 {
		fingerprint.Inode = 1
	}

	outcome, status, err := provider.ParseIncremental(
		context.Background(),
		IncrementalRequest{
			Source:       source,
			Fingerprint:  fingerprint,
			SessionID:    "codex:" + uuid,
			Offset:       int64(len(prefix)),
			StartOrdinal: 1,
		},
	)

	require.NoError(t, err)
	assert.Equal(t, IncrementalNeedsFullParse, status)
	assert.True(t, outcome.ForceReplace)
	assert.Empty(t, outcome.Messages)
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
			require.True(t, ok)
			concrete, ok := provider.(*codexProvider)
			require.True(t, ok)
			source := requireCodexProviderSource(t, provider, uuid)

			if mode == "warm" {
				prefixFingerprint, err := provider.Fingerprint(
					context.Background(), source,
				)
				require.NoError(t, err)
				full, err := provider.Parse(
					context.Background(),
					ParseRequest{
						Source: source, Fingerprint: prefixFingerprint,
					},
				)
				require.NoError(t, err)
				require.Len(t, full.Results, 1)
				assert.Empty(t, full.Results[0].Result.Session.FirstMessage)
				require.Len(t, full.Results[0].Result.Messages, 1)
				assert.Equal(
					t, "Orphan task finished",
					full.Results[0].Result.Messages[0].Content,
				)
				appendCodexProviderContent(t, path, tail)
			}

			fingerprint, err := provider.Fingerprint(
				context.Background(), source,
			)
			require.NoError(t, err)
			inode, device := fingerprint.Inode, fingerprint.Device
			prefixOffset := int64(len(prefix))
			_, oldBefore := concrete.cursorCache.Get(
				path, prefixOffset, inode, device,
			)

			outcome, status, err := provider.ParseIncremental(
				context.Background(),
				IncrementalRequest{
					Source:       source,
					Fingerprint:  fingerprint,
					SessionID:    "codex:" + uuid,
					Offset:       prefixOffset,
					StartOrdinal: 1,
				},
			)

			require.NoError(t, err)
			assert.Equal(t, IncrementalNeedsFullParse, status)
			assert.True(t, outcome.ForceReplace)
			assert.Empty(t, outcome.Messages)
			_, oldAfter := concrete.cursorCache.Get(
				path, prefixOffset, inode, device,
			)
			_, newAfter := concrete.cursorCache.Get(
				path, fingerprint.Size, inode, device,
			)
			assert.Equal(t, oldBefore, oldAfter)
			assert.False(t, newAfter)
			if mode == "cold" {
				assert.False(t, oldAfter)
			}

			full, err := provider.Parse(
				context.Background(),
				ParseRequest{Source: source, Fingerprint: fingerprint},
			)
			require.NoError(t, err)
			require.Len(t, full.Results, 1)
			assert.Equal(
				t, "first genuine request",
				full.Results[0].Result.Session.FirstMessage,
			)
			require.Len(t, full.Results[0].Result.Messages, 2)
		})
	}
}

func TestCodexProviderColdIncrementalStagesRetryCursorVersions(t *testing.T) {
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
	require.True(t, ok)
	concrete, ok := provider.(*codexProvider)
	require.True(t, ok)
	source := requireCodexProviderSource(t, provider, uuid)
	fingerprint, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	prefixOffset := int64(len(prefix))
	_, oldBefore := concrete.cursorCache.Get(
		path, prefixOffset, fingerprint.Inode, fingerprint.Device,
	)
	_, newBefore := concrete.cursorCache.Get(
		path, fingerprint.Size, fingerprint.Inode, fingerprint.Device,
	)
	assert.False(t, oldBefore)
	assert.False(t, newBefore)

	req := IncrementalRequest{
		Source:       source,
		Fingerprint:  fingerprint,
		SessionID:    "codex:" + uuid,
		Offset:       prefixOffset,
		StartOrdinal: 1,
	}
	first, status, err := provider.ParseIncremental(context.Background(), req)

	require.NoError(t, err)
	assert.Equal(t, IncrementalApplied, status)
	require.Len(t, first.Messages, 1)
	assert.Equal(t, RoleAssistant, first.Messages[0].Role)
	assert.Equal(t, "proposed answer", first.Messages[0].Content)
	assert.Equal(t, 1, first.Messages[0].Ordinal)
	assert.Equal(t, "gpt-5.5", first.Messages[0].Model)
	oldCursor, oldStaged := concrete.cursorCache.Get(
		path, prefixOffset, fingerprint.Inode, fingerprint.Device,
	)
	newCursor, newStaged := concrete.cursorCache.Get(
		path, fingerprint.Size, fingerprint.Inode, fingerprint.Device,
	)
	require.True(t, oldStaged, "persisted offset must be warm for a DB retry")
	require.True(t, newStaged, "proposed offset must keep its separate cursor")
	assert.Equal(t, "gpt-5.4", oldCursor.model)
	assert.Equal(t, "task_complete", oldCursor.lastTaskEvent)
	assert.Equal(t, "gpt-5.5", newCursor.model)
	assert.Equal(t, "task_started", newCursor.lastTaskEvent)

	// Simulate a failed DB write: the persisted offset remains unchanged and
	// the same tail is retried from the old cursor version.
	retry, retryStatus, err := provider.ParseIncremental(
		context.Background(), req,
	)

	require.NoError(t, err)
	assert.Equal(t, IncrementalApplied, retryStatus)
	assert.Equal(t, first, retry)
}

func TestCodexProviderParseIncrementalSuccess(t *testing.T) {
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
	require.True(t, ok)
	source := requireCodexProviderSource(t, provider, uuid)
	initialFingerprint, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	full, err := provider.Parse(context.Background(), ParseRequest{
		Source:      source,
		Fingerprint: initialFingerprint,
	})
	require.NoError(t, err)
	require.Len(t, full.Results, 1)
	require.Len(t, full.Results[0].Result.Messages, 1)

	tail := testjsonl.JoinJSONL(
		testjsonl.CodexTurnContextJSON("gpt-5.5", "2024-01-01T10:00:04Z"),
		testjsonl.CodexMsgJSON("user", "follow-up request", "2024-01-01T10:00:05Z"),
		codexEventMsgJSON("task_started", "2024-01-01T10:00:06Z"),
		testjsonl.CodexMsgJSON("assistant", "tail answer", "2024-01-01T10:00:07Z"),
		testjsonl.CodexTokenCountJSON("2024-01-01T10:00:08Z", 100_000, 250, 64_000),
		codexEventMsgJSON("task_complete", "2024-01-01T10:00:09Z"),
	)
	appendCodexProviderContent(t, path, tail)
	currentFingerprint, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)

	outcome, status, err := provider.ParseIncremental(
		context.Background(),
		IncrementalRequest{
			Source:       source,
			Fingerprint:  currentFingerprint,
			SessionID:    "codex:" + uuid,
			Offset:       initialFingerprint.Size,
			StartOrdinal: 1,
		},
	)

	require.NoError(t, err)
	assert.Equal(t, IncrementalApplied, status)
	assert.Equal(t, "codex:"+uuid, outcome.SessionID)
	require.Len(t, outcome.Messages, 2)
	assert.Equal(t, RoleUser, outcome.Messages[0].Role)
	assert.Equal(t, "follow-up request", outcome.Messages[0].Content)
	assert.Equal(t, 1, outcome.Messages[0].Ordinal)
	assert.Equal(t, "gpt-5.5", outcome.Messages[0].Model)
	assert.Equal(t, RoleAssistant, outcome.Messages[1].Role)
	assert.Equal(t, "tail answer", outcome.Messages[1].Content)
	assert.Equal(t, 2, outcome.Messages[1].Ordinal)
	assert.Equal(t, "gpt-5.5", outcome.Messages[1].Model)
	assert.Equal(t, time.Date(2024, time.January, 1, 10, 0, 9, 0, time.UTC), outcome.EndedAt)
	assert.Equal(t, int64(len(tail)), outcome.ConsumedBytes)
	assert.Equal(t, 2, outcome.MessageCount)
	assert.Equal(t, 1, outcome.UserMessageCount)
	assert.Equal(t, 250, outcome.TotalOutputTokens)
	assert.Equal(t, 100_000, outcome.PeakContextTokens)
	assert.True(t, outcome.HasTotalOutputTokens)
	assert.True(t, outcome.HasPeakContextTokens)
	require.NotNil(t, outcome.TerminationStatus)
	assert.Equal(t, TerminationAwaitingUser, *outcome.TerminationStatus)
	assert.False(t, outcome.ForceReplace)

	concrete, ok := provider.(*codexProvider)
	require.True(t, ok)
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
	require.True(t, oldOK)
	require.True(t, newOK)
	assert.Equal(t, "task_complete", oldSeed.lastTaskEvent)
	assert.Equal(t, "task_complete", newSeed.lastTaskEvent)
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
			require.True(t, ok)
			fullSource := requireCodexProviderSource(t, fullProvider, tc.uuid)
			fullFingerprint, err := fullProvider.Fingerprint(
				context.Background(), fullSource,
			)
			require.NoError(t, err)
			full, err := fullProvider.Parse(context.Background(), ParseRequest{
				Source: fullSource, Fingerprint: fullFingerprint,
			})
			require.NoError(t, err)
			require.Len(t, full.Results, 1)
			require.Len(t, full.Results[0].Result.Messages, 3)
			fullTail := full.Results[0].Result.Messages[2]
			assert.Equal(t, RoleAssistant, fullTail.Role)
			assert.Equal(t, "tail answer", fullTail.Content)
			assert.Equal(t, tc.wantHasTokens, fullTail.HasOutputTokens)
			assert.Equal(t, tc.wantOutput, fullTail.OutputTokens)
			assert.Equal(t, tc.wantPeakContext, fullTail.ContextTokens)

			for _, mode := range []string{"warm", "cold"} {
				t.Run(mode, func(t *testing.T) {
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
					require.True(t, ok)
					source := requireCodexProviderSource(t, provider, tc.uuid)
					if mode == "warm" {
						prefixFingerprint, err := provider.Fingerprint(
							context.Background(), source,
						)
						require.NoError(t, err)
						_, err = provider.Parse(context.Background(), ParseRequest{
							Source: source, Fingerprint: prefixFingerprint,
						})
						require.NoError(t, err)
						appendCodexProviderContent(t, path, tail)
					}
					fingerprint, err := provider.Fingerprint(
						context.Background(), source,
					)
					require.NoError(t, err)
					outcome, status, err := provider.ParseIncremental(
						context.Background(),
						IncrementalRequest{
							Source:       source,
							Fingerprint:  fingerprint,
							SessionID:    "codex:" + tc.uuid,
							Offset:       int64(len(prefix)),
							StartOrdinal: 2,
						},
					)
					require.NoError(t, err)
					assert.Equal(t, IncrementalApplied, status)
					require.Len(t, outcome.Messages, 1)
					message := outcome.Messages[0]
					assert.Equal(t, RoleAssistant, message.Role)
					assert.Equal(t, "tail answer", message.Content)
					assert.Equal(t, 2, message.Ordinal)
					assert.Equal(t, tc.wantHasTokens, message.HasOutputTokens)
					assert.Equal(t, tc.wantOutput, message.OutputTokens)
					assert.Equal(t, tc.wantPeakContext, message.ContextTokens)
					assert.Equal(t, tc.wantOutput, outcome.TotalOutputTokens)
					assert.Equal(t, tc.wantPeakContext, outcome.PeakContextTokens)
					assert.Equal(t, tc.wantHasTokens, outcome.HasTotalOutputTokens)
					assert.Equal(t, tc.wantHasTokens, outcome.HasPeakContextTokens)
					assert.Equal(t, int64(len(tail)), outcome.ConsumedBytes)
				})
			}
		})
	}
}

func TestCodexProviderParseIncrementalInboundAgentMessage(t *testing.T) {
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
	require.True(t, ok)
	source := requireCodexProviderSource(t, provider, uuid)
	initialFingerprint, err := provider.Fingerprint(
		context.Background(), source,
	)
	require.NoError(t, err)
	full, err := provider.Parse(context.Background(), ParseRequest{
		Source: source, Fingerprint: initialFingerprint,
	})
	require.NoError(t, err)
	require.Len(t, full.Results, 1)
	require.Len(t, full.Results[0].Result.Messages, 1)

	tail := testjsonl.CodexAgentMessageJSON(
		"/root", "/root/worker", "Follow-up received from parent.",
		"Check incremental parsing too.", tsEarlyS5,
	) + "\n"
	appendCodexProviderContent(t, path, tail)
	currentFingerprint, err := provider.Fingerprint(
		context.Background(), source,
	)
	require.NoError(t, err)

	outcome, status, err := provider.ParseIncremental(
		context.Background(),
		IncrementalRequest{
			Source: source, Fingerprint: currentFingerprint,
			SessionID: "codex:" + uuid,
			Offset:    initialFingerprint.Size, StartOrdinal: 1,
		},
	)

	require.NoError(t, err)
	assert.Equal(t, IncrementalApplied, status)
	require.Len(t, outcome.Messages, 1)
	assert.Equal(t, RoleUser, outcome.Messages[0].Role)
	assert.Equal(t, "Check incremental parsing too.", outcome.Messages[0].Content)
	assert.Equal(t, 1, outcome.Messages[0].Ordinal)
}

func TestCodexProviderParseIncrementalNoLifecycleMarker(t *testing.T) {
	root := t.TempDir()
	uuid := "019eb791-cf7d-75c1-8439-9ed74c1229e5"
	path := writeCodexProviderSession(t, root, uuid, "initial request")
	provider, ok := NewProvider(AgentCodex, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	source := requireCodexProviderSource(t, provider, uuid)
	initialFingerprint, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	_, err = provider.Parse(context.Background(), ParseRequest{
		Source: source, Fingerprint: initialFingerprint,
	})
	require.NoError(t, err)

	tail := testjsonl.JoinJSONL(
		testjsonl.CodexMsgJSON("assistant", "tail answer", tsLate),
	)
	appendCodexProviderContent(t, path, tail)
	currentFingerprint, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	outcome, status, err := provider.ParseIncremental(
		context.Background(),
		IncrementalRequest{
			Source:       source,
			Fingerprint:  currentFingerprint,
			SessionID:    "codex:" + uuid,
			Offset:       initialFingerprint.Size,
			StartOrdinal: 1,
		},
	)

	require.NoError(t, err)
	assert.Equal(t, IncrementalApplied, status)
	require.Len(t, outcome.Messages, 1)
	assert.Equal(t, "tail answer", outcome.Messages[0].Content)
	assert.Nil(t, outcome.TerminationStatus)
}

func TestCodexProviderParseIncrementalStagesCursorWithoutMessages(t *testing.T) {
	root := t.TempDir()
	uuid := "019eb791-cf7d-75c1-8439-9ed74c1229eb"
	path := writeCodexProviderSession(t, root, uuid, "initial request")
	provider, ok := NewProvider(AgentCodex, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	source := requireCodexProviderSource(t, provider, uuid)
	initialFingerprint, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	_, err = provider.Parse(context.Background(), ParseRequest{
		Source: source, Fingerprint: initialFingerprint,
	})
	require.NoError(t, err)

	tail := testjsonl.JoinJSONL(
		testjsonl.CodexTurnContextJSON("gpt-5.6", "2024-01-01T10:00:04Z"),
		codexEventMsgJSON("task_started", "2024-01-01T10:00:05Z"),
	)
	appendCodexProviderContent(t, path, tail)
	currentFingerprint, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	outcome, status, err := provider.ParseIncremental(
		context.Background(),
		IncrementalRequest{
			Source:       source,
			Fingerprint:  currentFingerprint,
			SessionID:    "codex:" + uuid,
			Offset:       initialFingerprint.Size,
			StartOrdinal: 1,
		},
	)

	require.NoError(t, err)
	assert.Equal(t, IncrementalApplied, status)
	assert.Empty(t, outcome.Messages)
	assert.Equal(t, 0, outcome.MessageCount)
	assert.Equal(t, 0, outcome.UserMessageCount)
	assert.Equal(t, int64(len(tail)), outcome.ConsumedBytes)
	assert.Equal(t, time.Date(2024, time.January, 1, 10, 0, 5, 0, time.UTC), outcome.EndedAt)
	require.NotNil(t, outcome.TerminationStatus)
	assert.Equal(t, TerminationToolCallPending, *outcome.TerminationStatus)

	concrete, ok := provider.(*codexProvider)
	require.True(t, ok)
	seed, staged := concrete.cursorCache.Get(
		path,
		currentFingerprint.Size,
		currentFingerprint.Inode,
		currentFingerprint.Device,
	)
	require.True(t, staged)
	assert.Equal(t, "gpt-5.6", seed.model)
	assert.Equal(t, "task_started", seed.lastTaskEvent)
}

func TestCodexProviderParseIncrementalNoDataAndTruncation(t *testing.T) {
	root := t.TempDir()
	uuid := "019eb791-cf7d-75c1-8439-9ed74c1229e6"
	writeCodexProviderSession(t, root, uuid, "initial request")
	provider, ok := NewProvider(AgentCodex, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	source := requireCodexProviderSource(t, provider, uuid)
	fingerprint, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	_, err = provider.Parse(context.Background(), ParseRequest{
		Source: source, Fingerprint: fingerprint,
	})
	require.NoError(t, err)

	noData, status, err := provider.ParseIncremental(
		context.Background(),
		IncrementalRequest{
			Source:      source,
			Fingerprint: fingerprint,
			SessionID:   "codex:" + uuid,
			Offset:      fingerprint.Size,
		},
	)
	require.NoError(t, err)
	assert.Equal(t, IncrementalNoNewData, status)
	assert.Equal(t, IncrementalOutcome{}, noData)

	truncated, status, err := provider.ParseIncremental(
		context.Background(),
		IncrementalRequest{
			Source:      source,
			Fingerprint: fingerprint,
			SessionID:   "codex:" + uuid,
			Offset:      fingerprint.Size + 1,
		},
	)
	require.NoError(t, err)
	assert.Equal(t, IncrementalNeedsFullParse, status)
	assert.True(t, truncated.ForceReplace)
	assert.Empty(t, truncated.Messages)
}

func TestCodexProviderParseIncrementalUnsafePartialOffset(t *testing.T) {
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
	require.True(t, ok)
	source := requireCodexProviderSource(t, provider, uuid)
	partialFingerprint, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	initial, err := provider.Parse(context.Background(), ParseRequest{
		Source: source, Fingerprint: partialFingerprint,
	})
	require.NoError(t, err)
	require.Len(t, initial.Results, 1)
	assert.Empty(t, initial.Results[0].Result.Messages)
	assert.Equal(t, int64(len(partial)), initial.Results[0].Result.Session.File.Size)

	appendCodexProviderContent(t, path, messageLine[cut:]+"\n")
	completeFingerprint, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	outcome, status, err := provider.ParseIncremental(
		context.Background(),
		IncrementalRequest{
			Source:       source,
			Fingerprint:  completeFingerprint,
			SessionID:    "codex:" + uuid,
			Offset:       partialFingerprint.Size,
			StartOrdinal: 0,
		},
	)

	require.NoError(t, err)
	assert.Equal(t, IncrementalNeedsFullParse, status)
	assert.True(t, outcome.ForceReplace)
	assert.Empty(t, outcome.Messages)

	full, err := provider.Parse(context.Background(), ParseRequest{
		Source: source, Fingerprint: completeFingerprint,
	})
	require.NoError(t, err)
	require.Len(t, full.Results, 1)
	require.Len(t, full.Results[0].Result.Messages, 1)
	assert.Equal(t,
		"record completed after the first parse",
		full.Results[0].Result.Messages[0].Content,
	)
}

func TestCodexProviderParseIncrementalStagesCompleteRecordsOnly(t *testing.T) {
	root := t.TempDir()
	uuid := "019eb791-cf7d-75c1-8439-9ed74c1229e8"
	path := writeCodexProviderSession(t, root, uuid, "initial request")
	provider, ok := NewProvider(AgentCodex, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	source := requireCodexProviderSource(t, provider, uuid)
	initialFingerprint, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	_, err = provider.Parse(context.Background(), ParseRequest{
		Source: source, Fingerprint: initialFingerprint,
	})
	require.NoError(t, err)

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
	unterminatedFingerprint, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	outcome, status, err := provider.ParseIncremental(
		context.Background(),
		IncrementalRequest{
			Source:       source,
			Fingerprint:  unterminatedFingerprint,
			SessionID:    "codex:" + uuid,
			Offset:       initialFingerprint.Size,
			StartOrdinal: 1,
		},
	)

	require.NoError(t, err)
	assert.Equal(t, IncrementalApplied, status)
	require.Len(t, outcome.Messages, 1)
	assert.Equal(t, "complete tail record", outcome.Messages[0].Content)
	assert.Equal(t, 1, outcome.Messages[0].Ordinal)
	assert.Equal(t, int64(len(completeRecord)), outcome.ConsumedBytes)
	stagedOffset := initialFingerprint.Size + int64(len(completeRecord))
	concrete, ok := provider.(*codexProvider)
	require.True(t, ok)
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
	assert.True(t, stagedOK)
	assert.False(t, eofOK)

	appendCodexProviderContent(t, path, deferredRecord[deferredCut:]+"\n")
	completeFingerprint, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	completed, status, err := provider.ParseIncremental(
		context.Background(),
		IncrementalRequest{
			Source:       source,
			Fingerprint:  completeFingerprint,
			SessionID:    "codex:" + uuid,
			Offset:       stagedOffset,
			StartOrdinal: 2,
		},
	)

	require.NoError(t, err)
	assert.Equal(t, IncrementalApplied, status)
	require.Len(t, completed.Messages, 1)
	assert.Equal(t, RoleUser, completed.Messages[0].Role)
	assert.Equal(t, "partial record completed later", completed.Messages[0].Content)
	assert.Equal(t, 2, completed.Messages[0].Ordinal)
	assert.Equal(t, int64(len(deferredRecord)+1), completed.ConsumedBytes)
}

func TestCodexProviderParseIncrementalValidEOFNeedsFullParse(t *testing.T) {
	root := t.TempDir()
	uuid := "019eb791-cf7d-75c1-8439-9ed74c1229ea"
	path := writeCodexProviderSession(t, root, uuid, "initial request")
	provider, ok := NewProvider(AgentCodex, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	source := requireCodexProviderSource(t, provider, uuid)
	initialFingerprint, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	_, err = provider.Parse(context.Background(), ParseRequest{
		Source: source, Fingerprint: initialFingerprint,
	})
	require.NoError(t, err)

	unterminatedRecord := testjsonl.CodexMsgJSON(
		"assistant", "valid record without newline", tsLate,
	)
	appendCodexProviderContent(t, path, unterminatedRecord)
	currentFingerprint, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	outcome, status, err := provider.ParseIncremental(
		context.Background(),
		IncrementalRequest{
			Source:       source,
			Fingerprint:  currentFingerprint,
			SessionID:    "codex:" + uuid,
			Offset:       initialFingerprint.Size,
			StartOrdinal: 1,
		},
	)

	require.NoError(t, err)
	assert.Equal(t, IncrementalNeedsFullParse, status)
	assert.True(t, outcome.ForceReplace)
	concrete, ok := provider.(*codexProvider)
	require.True(t, ok)
	_, staged := concrete.cursorCache.Get(
		path,
		currentFingerprint.Size,
		currentFingerprint.Inode,
		currentFingerprint.Device,
	)
	assert.False(t, staged)

	full, err := provider.Parse(context.Background(), ParseRequest{
		Source: source, Fingerprint: currentFingerprint,
	})
	require.NoError(t, err)
	require.Len(t, full.Results, 1)
	require.Len(t, full.Results[0].Result.Messages, 2)
	assert.Equal(t,
		"valid record without newline",
		full.Results[0].Result.Messages[1].Content,
	)
}

func TestCodexProviderParseIncrementalFallbackDoesNotStage(t *testing.T) {
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
	require.True(t, ok)
	source := requireCodexProviderSource(t, provider, uuid)
	initialFingerprint, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	concrete, ok := provider.(*codexProvider)
	require.True(t, ok)

	tail := testjsonl.JoinJSONL(
		testjsonl.CodexTokenCountJSON(tsLateS5, 100_000, 250, 64_000),
	)
	appendCodexProviderContent(t, path, tail)
	currentFingerprint, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	outcome, status, err := provider.ParseIncremental(
		context.Background(),
		IncrementalRequest{
			Source:       source,
			Fingerprint:  currentFingerprint,
			SessionID:    "codex:" + uuid,
			Offset:       initialFingerprint.Size,
			StartOrdinal: 2,
		},
	)

	require.NoError(t, err)
	assert.Equal(t, IncrementalNeedsFullParse, status)
	assert.True(t, outcome.ForceReplace)
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
	assert.False(t, oldStaged)
	assert.False(t, newStaged)
}

func TestCodexProviderColdSeedErrorDoesNotStage(t *testing.T) {
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
	require.True(t, ok)
	concrete, ok := provider.(*codexProvider)
	require.True(t, ok)
	source := requireCodexProviderSource(t, provider, uuid)
	fingerprint, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
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

	require.ErrorIs(t, err, wantErr)
	assert.Empty(t, result.messages)
	assert.False(t, tailReadCalled)
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
	assert.False(t, oldStaged)
	assert.False(t, newStaged)
}

func TestCodexProviderParseIncrementalHonorsContext(t *testing.T) {
	provider, ok := NewProvider(AgentCodex, ProviderConfig{})
	require.True(t, ok)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, status, err := provider.ParseIncremental(ctx, IncrementalRequest{})

	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, IncrementalUnsupported, status)
}

func TestCodexProviderDiscoverDedupesLiveAndArchivedByUUID(t *testing.T) {
	base := t.TempDir()
	liveRoot := filepath.Join(base, "sessions")
	archivedRoot := filepath.Join(base, "archived_sessions")
	uuid := "019eb791-cf7d-75c1-8439-9ed74c1229e5"
	livePath := writeCodexProviderSession(t, liveRoot, uuid, "live")
	archivedPath := writeCodexProviderArchivedSession(
		t, archivedRoot, uuid, "archived",
	)

	provider, ok := NewProvider(AgentCodex, ProviderConfig{
		Roots: []string{archivedRoot, liveRoot},
	})
	require.True(t, ok)
	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, livePath, discovered[0].DisplayPath)
	assert.NotEqual(t, archivedPath, discovered[0].DisplayPath)
}

func TestCodexProviderFindSourcePinsExactArchivedDuplicate(t *testing.T) {
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
	require.True(t, ok)

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		StoredFilePath: archivedPath,
		FullSessionID:  "codex:" + uuid,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, archivedPath, found.DisplayPath)

	found, ok, err = provider.FindSource(context.Background(), FindSourceRequest{
		FullSessionID: "codex:" + uuid,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, livePath, found.DisplayPath)
}

func TestCodexProviderFindSourcePreferStoredSourceKeepsArchivedDuplicate(t *testing.T) {
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
	require.True(t, ok)

	// PreferStoredSource pins the stored archived duplicate even when a fresh
	// source is required, instead of canonicalizing to the live duplicate.
	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		StoredFilePath:     archivedPath,
		FullSessionID:      "codex:" + uuid,
		RequireFreshSource: true,
		PreferStoredSource: true,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, archivedPath, found.DisplayPath,
		"PreferStoredSource must preserve the stored archived path")

	// Without the hint, RequireFreshSource canonicalizes to the live duplicate,
	// which is exactly the behavior PreferStoredSource opts out of.
	found, ok, err = provider.FindSource(context.Background(), FindSourceRequest{
		StoredFilePath:     archivedPath,
		FullSessionID:      "codex:" + uuid,
		RequireFreshSource: true,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, livePath, found.DisplayPath,
		"RequireFreshSource without PreferStoredSource canonicalizes to live")
}

func TestCodexProviderFindSourceAcceptsLegacyShapedStoredPath(t *testing.T) {
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
	require.NoError(t, os.MkdirAll(filepath.Dir(sourcePath), 0o755))
	require.NoError(t, os.WriteFile(sourcePath, []byte(content), 0o644))

	provider, ok := NewProvider(AgentCodex, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, sourcePath, discovered[0].DisplayPath)

	changed, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: sourcePath, EventKind: "write"},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, sourcePath, changed[0].DisplayPath)

	source, found, err := provider.FindSource(context.Background(), FindSourceRequest{
		StoredFilePath: sourcePath,
		FingerprintKey: sourcePath,
	})
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, AgentCodex, source.Provider)
	assert.Equal(t, sourcePath, source.DisplayPath)
	assert.Equal(t, sourcePath, source.FingerprintKey)

	fingerprint, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	assert.Equal(t, sourcePath, fingerprint.Key)
	assert.NotEmpty(t, fingerprint.Hash)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source:      source,
		Fingerprint: fingerprint,
		Machine:     "devbox",
	})
	require.NoError(t, err)
	require.True(t, outcome.ResultSetComplete)
	require.Len(t, outcome.Results, 1)
	result := outcome.Results[0]
	assert.Equal(t, "codex:"+sessionID, result.Result.Session.ID)
	assert.Equal(t, "api", result.Result.Session.Project)
	assert.Equal(t, "devbox", result.Result.Session.Machine)
	assert.Equal(t, fingerprint.Hash, result.Result.Session.File.Hash)
	assert.Len(t, result.Result.Messages, 1)
}

func TestCodexProviderChangedPathPinsArchivedDuplicate(t *testing.T) {
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
	require.True(t, ok)
	changed, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: archivedPath, EventKind: "write"},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, archivedPath, changed[0].DisplayPath)
}

func TestCodexProviderChangedPathClassifiesRemovedTranscript(t *testing.T) {
	root := t.TempDir()
	uuid := "019eb791-cf7d-75c1-8439-9ed74c1229e8"
	sourcePath := writeCodexProviderSession(t, root, uuid, "remove")
	provider, ok := NewProvider(AgentCodex, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	require.NoError(t, os.Remove(sourcePath))

	changed, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: sourcePath, EventKind: "remove"},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, sourcePath, changed[0].DisplayPath)
}

func TestCodexProviderIndexPathClassifiesAllSiblingSources(t *testing.T) {

	base := t.TempDir()
	root := filepath.Join(base, "sessions")
	firstUUID := "019eb791-cf7d-75c1-8439-9ed74c1229e9"
	secondUUID := "019eb791-cf7d-75c1-8439-9ed74c1229ea"
	firstPath := writeCodexProviderSession(t, root, firstUUID, "first")
	secondPath := writeCodexProviderSession(t, root, secondUUID, "second")
	indexPath := filepath.Join(base, CodexSessionIndexFilename)
	require.NoError(t, os.WriteFile(indexPath, []byte(
		`{"id":"`+firstUUID+`","thread_name":"Only first remains","updated_at":"2026-06-11T17:34:20Z"}`+"\n",
	), 0o644))

	provider, ok := NewProvider(AgentCodex, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	changed, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: indexPath, EventKind: "write"},
	)
	require.NoError(t, err)
	assert.Equal(t, []string{firstPath, secondPath}, sourceDisplayPaths(changed))
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
		context.Background(),
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
