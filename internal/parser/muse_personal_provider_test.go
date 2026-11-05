package parser

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMusePersonalRawSourceIdentityAndCompanions(t *testing.T) {
	p, path := musePersonalTestProvider(t, musePersonalTestItem(0, `{"type":"message","role":"user","text":"hello"}`))
	index := filepath.Join(filepath.Dir(path), "sessions.json")
	writeSourceFile(t, index, `{"version":99}`)
	writeSourceFile(t, filepath.Join(filepath.Dir(path), "secret.json"), "not a source")
	sources, err := p.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	assert.Equal(t, "muse-personal:"+musePersonalTestID, sources[0].Key)
	planner, ok := p.(RawCaptureProvider)
	require.True(t, ok, "raw custody must be supported")
	plan, err := planner.PlanRawCapture(t.Context(), sources[0])
	require.NoError(t, err)
	require.Len(t, plan.Entries, 2)
	assert.Equal(t, sources[0].Key, plan.SourceKey)
	assert.Equal(t, "agent-66666666-7777-4888-8999-000000000000/sessions/"+musePersonalTestID+".jsonl", plan.Entries[0].Path)
	assert.True(t, plan.Entries[0].Appendable)
	assert.False(t, plan.Entries[1].Appendable)
	assert.Equal(t, "sessions.json", filepath.Base(plan.Entries[1].LocalPath))
	root := filepath.Dir(filepath.Dir(filepath.Dir(path)))
	moved := filepath.Join(root, "agent-aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", "sessions", filepath.Base(path))
	require.NoError(t, os.MkdirAll(filepath.Dir(moved), 0o700))
	require.NoError(t, os.Rename(path, moved))
	next, err := p.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, next, 1)
	assert.Equal(t, sources[0].Key, next[0].Key)
	found, ok, err := p.FindSource(t.Context(), FindSourceRequest{RawSessionID: musePersonalTestID, StoredFilePath: path, RequireFreshSource: true})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, moved, found.DisplayPath)
	assert.Equal(t, next[0].Key, found.Key)
}

func TestMusePersonalIndexEventBoundedToOwningDirectory(t *testing.T) {
	for _, count := range []int{1, 300} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			p, path := musePersonalTestProvider(t, "")
			root := filepath.Dir(filepath.Dir(filepath.Dir(path)))
			for i := range count {
				writeSourceFile(t, filepath.Join(root, fmt.Sprintf("agent-%08x-2222-4333-8444-555555555555", i), "sessions", "decoy.jsonl"), "{}\n")
			}
			secondID := "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
			second := filepath.Join(filepath.Dir(path), secondID+".jsonl")
			writeSourceFile(t, second, strings.ReplaceAll(musePersonalTestHeader, musePersonalTestID, secondID))
			// A removed/malformed index must still select every transcript sharing it.
			for _, event := range []string{"write", "remove"} {
				var opened []string
				ctx := withStreamingDirectoryReader(t.Context(), func(ctx context.Context, dir string, yield func(os.DirEntry) error) error {
					opened = append(opened, dir)
					return streamDirectoryEntriesDirect(ctx, dir, yield)
				})
				sources, err := p.SourcesForChangedPath(ctx, ChangedPathRequest{Path: filepath.Join(filepath.Dir(path), "sessions.json"), WatchRoot: root, EventKind: event})
				assert.Equal(t, []string{filepath.Dir(path)}, opened)
				require.NoError(t, err)
				require.Len(t, sources, 2)
				assert.ElementsMatch(t, []string{path, second}, []string{sources[0].DisplayPath, sources[1].DisplayPath})
			}
		})
	}
}

func TestMusePersonalCompositeFingerprintAndDigest(t *testing.T) {
	p, path := musePersonalTestProvider(t, "")
	sources, err := p.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	hasher, ok := p.(MultiFileStatHasher)
	require.True(t, ok)
	initial, err := p.Fingerprint(t.Context(), sources[0])
	require.NoError(t, err)
	index := filepath.Join(filepath.Dir(path), "sessions.json")
	writeSourceFile(t, index, `{"version":1}`)
	first, err := p.Fingerprint(t.Context(), sources[0])
	require.NoError(t, err)
	assert.NotEqual(t, initial.Hash, first.Hash)
	digest := hasher.ComputeMultiFileStatHash(path)
	require.NotZero(t, digest)
	info, err := os.Stat(index)
	require.NoError(t, err)
	writeSourceFile(t, index, `{"version":2}`)
	require.NoError(t, os.Chtimes(index, info.ModTime(), info.ModTime()))
	second, err := p.Fingerprint(t.Context(), sources[0])
	require.NoError(t, err)
	assert.Equal(t, first.Size, second.Size)
	assert.NotEqual(t, first.Hash, second.Hash)
	// A zero digest conservatively forces hashing on platforms without ctime.
	if digest != 0 {
		assert.NotEqual(t, digest, hasher.ComputeMultiFileStatHash(path))
	}
	require.NoError(t, os.Remove(index))
	final, err := p.Fingerprint(t.Context(), sources[0])
	require.NoError(t, err)
	assert.Equal(t, initial.Hash, final.Hash)
}

func TestMusePersonalPartialDiscoveryPreservesHealthySourcesAndRawNonAuthority(t *testing.T) {
	root := t.TempDir()
	sessionsDir := filepath.Join(root, "agent-66666666-7777-4888-8999-000000000000", "sessions")
	require.NoError(t, os.MkdirAll(sessionsDir, 0o700))
	incompleteID := "00000000-2222-4333-8444-555555555555"
	incompletePath := filepath.Join(sessionsDir, incompleteID+".jsonl")
	writeSourceFile(t, incompletePath, `{"type":"session_header"`)
	healthyID := "99999999-2222-4333-8444-555555555555"
	healthyPath := filepath.Join(sessionsDir, healthyID+".jsonl")
	header := strings.ReplaceAll(musePersonalTestHeader, musePersonalTestID, healthyID)
	writeSourceFile(t, healthyPath, header+musePersonalTestItem(0, `{"type":"message","role":"user","text":"healthy"}`))
	p, ok := NewProvider(AgentType("muse-personal"), ProviderConfig{Roots: []string{root}})
	require.True(t, ok)

	sources, err := p.Discover(t.Context())
	require.Error(t, err)
	_, incomplete := errors.AsType[DiscoveryIncompleteError](err)
	assert.True(t, incomplete)
	require.Len(t, sources, 1)
	assert.Equal(t, healthyPath, sources[0].DisplayPath)

	rawProvider, ok := p.(StreamingRawCaptureSourceProvider)
	require.True(t, ok)
	var rawSources []SourceRef
	authoritative, err := rawProvider.DiscoverRawCaptureSourcesEach(t.Context(), func(source SourceRef) error {
		rawSources = append(rawSources, source)
		return nil
	})
	require.Error(t, err)
	_, incomplete = errors.AsType[DiscoveryIncompleteError](err)
	assert.True(t, incomplete)
	assert.False(t, authoritative)
	require.Len(t, rawSources, 1)
	assert.Equal(t, healthyPath, rawSources[0].DisplayPath)
}

func TestMusePersonalStrictClaimingAndCancellation(t *testing.T) {
	p, path := musePersonalTestProvider(t, "")
	dir := filepath.Dir(path)
	for i, header := range []string{"{}\n", strings.Replace(musePersonalTestHeader, `"version":1`, `"version":2`, 1), strings.Repeat(" ", 32<<10) + musePersonalTestHeader} {
		sessionID := fmt.Sprintf("%08x-2222-4333-8444-555555555555", i)
		header = strings.ReplaceAll(header, musePersonalTestID, sessionID)
		if i > 0 {
			require.Contains(t, header, `"session_id":"`+sessionID+`"`, "fixture session_id must match its filename")
		}
		name := sessionID + ".jsonl"
		writeSourceFile(t, filepath.Join(dir, name), header)
	}
	writeSourceFile(t, filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(path))), "workspace", "agents", "agent-66666666-7777-4888-8999-000000000000", "sessions", musePersonalTestID+".jsonl"), musePersonalTestHeader)
	sources, err := p.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = p.Discover(ctx)
	require.ErrorIs(t, err, context.Canceled)
	_, err = p.Parse(ctx, ParseRequest{Source: sources[0]})
	require.ErrorIs(t, err, context.Canceled)
}

func TestMusePersonalRejectsSymlinkInputs(t *testing.T) {
	for _, target := range []string{"transcript", "index", "sessions directory", "agent directory"} {
		t.Run(target, func(t *testing.T) {
			p, path := musePersonalTestProvider(t, musePersonalTestItem(0, `{"type":"message","role":"user","text":"safe"}`))
			original, err := p.Discover(t.Context())
			require.NoError(t, err)
			require.Len(t, original, 1)
			link := path
			switch target {
			case "index":
				link = filepath.Join(filepath.Dir(path), "sessions.json")
			case "sessions directory":
				link = filepath.Dir(path)
			case "agent directory":
				link = filepath.Dir(filepath.Dir(path))
			}
			outside := filepath.Join(t.TempDir(), "target")
			if target == "index" {
				writeSourceFile(t, outside, `{"version":1,"secret":"must not be read"}`)
			} else {
				require.NoError(t, os.Rename(link, outside))
			}
			if err := os.Symlink(outside, link); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			sources, err := p.Discover(t.Context())
			require.NoError(t, err)
			if target == "index" {
				require.Len(t, sources, 1)
				_, err = p.(RawCaptureProvider).PlanRawCapture(t.Context(), sources[0])
				require.Error(t, err)
				out := musePersonalTestParse(t, p)
				require.Len(t, out.Results, 1)
				assert.Equal(t, 1, out.Results[0].Result.Session.MessageCount)
			} else {
				assert.Empty(t, sources)
			}
			// Already discovered references cannot bypass current safety validation.
			if target != "index" {
				_, err := p.Fingerprint(t.Context(), original[0])
				require.Error(t, err)
			}
		})
	}
}

func TestMusePersonalRawDiscoveryReportsRejectedEntries(t *testing.T) {
	p, path := musePersonalTestProvider(t, "")
	for i := range 300 {
		writeSourceFile(t, filepath.Join(filepath.Dir(path), fmt.Sprintf("ignored-%d", i)), "not a transcript")
	}
	steps, batch, yielded := 0, 0, 0
	ctx := WithRawCaptureDiscoveryProgress(t.Context(), func() error { steps++; return nil })
	ctx = WithStreamingDiscoveryBufferObserver(ctx, func(n int) { batch = max(batch, n) })
	complete, err := StreamRawCaptureSources(ctx, p, func(SourceRef) error { yielded++; return nil })
	require.NoError(t, err)
	assert.True(t, complete)
	assert.Equal(t, 1, yielded)
	assert.GreaterOrEqual(t, steps, 300)
	assert.LessOrEqual(t, batch, streamingDirectoryBatchSize)
	stop := errors.New("pause traversal")
	ctx = WithRawCaptureDiscoveryProgress(t.Context(), func() error { return stop })
	complete, err = StreamRawCaptureSources(ctx, p, func(SourceRef) error { require.FailNow(t, "yielded after stop"); return nil })
	require.ErrorIs(t, err, stop)
	assert.False(t, complete)
}

func TestMusePersonalOversizedLineDoesNotHideFollowingRecords(t *testing.T) {
	p, path := musePersonalTestProvider(t, "")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = io.Copy(f, io.LimitReader(strings.NewReader(strings.Repeat("x", 1<<20)), 1<<20))
	require.NoError(t, err)
	// Reuse one bounded chunk when creating an over-limit synthetic record.
	chunk := strings.Repeat("x", 1<<20)
	for written := 1 << 20; written <= maxLineSize; written += len(chunk) {
		_, err = f.WriteString(chunk)
		require.NoError(t, err)
	}
	_, err = f.WriteString("\n" + musePersonalTestItem(0, `{"type":"message","role":"user","text":"after oversized record"}`))
	require.NoError(t, err)
	require.NoError(t, f.Close())
	out := musePersonalTestParse(t, p)
	require.Len(t, out.Results, 1)
	assert.Equal(t, 1, out.Results[0].Result.Session.MalformedLines)
	require.Len(t, out.Results[0].Result.Messages, 1)
	assert.Equal(t, "after oversized record", out.Results[0].Result.Messages[0].Content)
}

// Cancel at an Err-check boundary to cover cancellation during every read stage
// without timing a concurrent decoder or relying on its number of reads.
type musePersonalCancelAfterChecks struct {
	context.Context
	cancel    context.CancelFunc
	remaining int
}

func (c *musePersonalCancelAfterChecks) Err() error {
	if c.remaining == 0 {
		c.cancel()
	} else {
		c.remaining--
	}
	return c.Context.Err()
}

func TestMusePersonalCancellationDoesNotPublish(t *testing.T) {
	p, path := musePersonalTestProvider(t, musePersonalTestItem(0, `{"type":"message","role":"user","text":"fixture"}`))
	index := `{"version":1,"sessions":[{"session_id":"11111111-2222-4333-8444-555555555555","agent_id":"66666666-7777-4888-8999-000000000000","created_at":"2026-10-05T15:04:05Z","updated_at":"2026-10-05T16:00:00Z"}]}`
	require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(path), "sessions.json"), []byte(index), 0o600))
	sources, err := p.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	fp, err := p.Fingerprint(t.Context(), sources[0])
	require.NoError(t, err)
	for budget := range 32 {
		t.Run(strconv.Itoa(budget), func(t *testing.T) {
			base, cancel := context.WithCancel(t.Context())
			defer cancel()
			ctx := &musePersonalCancelAfterChecks{Context: base, cancel: cancel, remaining: budget}
			out, err := p.Parse(ctx, ParseRequest{Source: sources[0], Fingerprint: fp})
			if base.Err() != nil {
				require.ErrorIs(t, err, context.Canceled)
				assert.Empty(t, out.Results)
				assert.False(t, out.ResultSetComplete)
				assert.False(t, out.ForceReplace)
			} else {
				require.NoError(t, err)
				require.Len(t, out.Results, 1)
			}
		})
	}
}

func TestMusePersonalTranscriptEventUsesConfiguredWatchRoot(t *testing.T) {
	p, path := musePersonalTestProvider(t, "")
	root := filepath.Dir(filepath.Dir(filepath.Dir(path)))
	for _, watchRoot := range []string{"", root, filepath.Dir(path)} {
		sources, err := p.SourcesForChangedPath(t.Context(), ChangedPathRequest{Path: path, WatchRoot: watchRoot, EventKind: "write"})
		require.NoError(t, err)
		require.Len(t, sources, 1, "watch root %s", watchRoot)
		assert.Equal(t, path, sources[0].DisplayPath)
	}
	sources, err := p.SourcesForChangedPath(t.Context(), ChangedPathRequest{Path: path, WatchRoot: t.TempDir(), EventKind: "write"})
	require.NoError(t, err)
	assert.Empty(t, sources)
}

func TestMusePersonalUnfinishedHeaderCannotReplaceDiscoveredSource(t *testing.T) {
	p, path := musePersonalTestProvider(t, musePersonalTestItem(0, `{"type":"message","role":"user","text":"preserved"}`))
	sources, err := p.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	fp, err := p.Fingerprint(t.Context(), sources[0])
	require.NoError(t, err)
	for _, body := range []string{"", `{"type":"session_header"`, strings.TrimSuffix(musePersonalTestHeader, "\n")} {
		require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
		for _, fingerprint := range []SourceFingerprint{{}, fp} {
			out, err := p.Parse(t.Context(), ParseRequest{Source: sources[0], Fingerprint: fingerprint})
			require.NoError(t, err)
			assert.False(t, out.ResultSetComplete)
			assert.Empty(t, out.Results)
			assert.False(t, out.ForceReplace)
		}
		complete, err := StreamRawCaptureSources(t.Context(), p, func(SourceRef) error { return nil })
		require.ErrorIs(t, err, errMusePersonalIncompleteHeader)
		assert.False(t, complete)
	}
}
