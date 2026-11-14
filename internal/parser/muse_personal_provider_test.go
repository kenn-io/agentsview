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
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
	p, path := musePersonalTestProvider(t, "")
	root := filepath.Dir(filepath.Dir(filepath.Dir(path)))
	for i := range 300 {
		writeSourceFile(t, filepath.Join(root, fmt.Sprintf("agent-%08x-2222-4333-8444-555555555555", i), "sessions", "decoy.jsonl"), "{}\n")
	}
	secondID := "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	second := filepath.Join(filepath.Dir(path), secondID+".jsonl")
	writeSourceFile(t, second, strings.ReplaceAll(musePersonalTestHeader, musePersonalTestID, secondID))
	// An absent index still selects every transcript sharing its path.
	var opened []string
	ctx := withStreamingDirectoryReader(t.Context(), func(ctx context.Context, dir string, yield func(os.DirEntry) error) error {
		opened = append(opened, dir)
		return streamDirectoryEntriesDirect(ctx, dir, yield)
	})
	sources, err := p.SourcesForChangedPath(ctx, ChangedPathRequest{Path: filepath.Join(filepath.Dir(path), "sessions.json"), WatchRoot: root, EventKind: "remove"})
	assert.Equal(t, []string{filepath.Dir(path)}, opened)
	require.NoError(t, err)
	require.Len(t, sources, 2)
	assert.ElementsMatch(t, []string{path, second}, []string{sources[0].DisplayPath, sources[1].DisplayPath})
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
	assert.Equal(t, initial.Hash, first.Hash)
	digest := hasher.ComputeMultiFileStatHash(path)
	require.NotZero(t, digest)
	info, err := os.Stat(index)
	require.NoError(t, err)
	beforeChangeTime, hasChangeTime := codexIndexChangeTime(index, info)
	writeSourceFile(t, index, `{"version":2}`)
	require.NoError(t, os.Chtimes(index, info.ModTime(), info.ModTime()))
	if hasChangeTime {
		// Preserve size and mtime, but establish a distinct ctime before
		// checking the stat digest. Rapid writes can share a timestamp tick.
		require.EventuallyWithT(t, func(c *assert.CollectT) {
			require.NoError(c, os.Chtimes(index, info.ModTime(), info.ModTime()))
			changed, err := os.Stat(index)
			require.NoError(c, err)
			changeTime, ok := codexIndexChangeTime(index, changed)
			require.True(c, ok)
			assert.NotEqual(c, beforeChangeTime, changeTime)
		}, 2*time.Second, time.Millisecond)
	}
	second, err := p.Fingerprint(t.Context(), sources[0])
	require.NoError(t, err)
	assert.Equal(t, first.Size, second.Size)
	assert.Equal(t, first.Hash, second.Hash)
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
	require.ErrorContains(t, err, "unsupported Muse Personal transcript version 2")
	require.Len(t, sources, 1)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = p.Discover(ctx)
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
				writeSourceFile(t, outside, `{"version":1,"sessions":[{"session_id":"11111111-2222-4333-8444-555555555555","agent_id":"66666666-7777-4888-8999-000000000000","created_at":"2026-10-05T15:04:05Z","updated_at":"2026-10-05T18:00:00Z"}]}`)
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
				assert.Equal(t, "2026-10-05T15:04:06Z", out.Results[0].Result.Session.EndedAt.Format(time.RFC3339))
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

func TestMusePersonalIndexCacheAcrossProviders(t *testing.T) {
	for _, count := range []int{2, 100} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			_, path := musePersonalTestProvider(t, "")
			root := filepath.Dir(filepath.Dir(filepath.Dir(path)))
			require.NoError(t, os.Remove(path))
			var rows []string
			for i := range count {
				id := fmt.Sprintf("%08x-2222-4333-8444-555555555555", i)
				writeSourceFile(t, filepath.Join(filepath.Dir(path), id+".jsonl"), strings.ReplaceAll(musePersonalTestHeader, musePersonalTestID, id))
				rows = append(rows, fmt.Sprintf(`{"session_id":%q,"agent_id":"66666666-7777-4888-8999-000000000000","created_at":"2026-10-05T17:04:05+02:00","updated_at":"2026-10-05T16:00:00Z"}`, id))
			}
			index := filepath.Join(filepath.Dir(path), "sessions.json")
			text := `{"version":1,"sessions":[` + strings.Join(rows, ",") + `]}`
			writeSourceFile(t, index, text)
			for range 2 {
				var scans atomic.Int64
				ctx, closeCache, err := WithReconciliationCache(WithSharedContainerScanObserver(t.Context(), func() { scans.Add(1) }))
				require.NoError(t, err)
				p, ok := NewProvider(AgentMusePersonal, ProviderConfig{Roots: []string{root}})
				require.True(t, ok)
				sources, err := p.Discover(ctx)
				require.NoError(t, err)
				require.Len(t, sources, count)
				parseAll := func(want string) {
					var workers sync.WaitGroup
					errs := make([]error, len(sources))
					ended := make([]string, len(sources))
					for i, source := range sources {
						workers.Go(func() {
							other, ok := NewProvider(AgentMusePersonal, ProviderConfig{Roots: []string{root}})
							if !ok {
								errs[i] = errors.New("muse personal provider unavailable")
								return
							}
							fp, err := other.Fingerprint(ctx, source)
							if err != nil {
								errs[i] = fmt.Errorf("fingerprint %s: %w", source.Key, err)
								return
							}
							out, err := other.Parse(ctx, ParseRequest{Source: source, Fingerprint: fp})
							if err != nil {
								errs[i] = fmt.Errorf("parse %s: %w", source.Key, err)
								return
							}
							if len(out.Results) != 1 {
								errs[i] = fmt.Errorf("parse %s: got %d results, want 1", source.Key, len(out.Results))
								return
							}
							ended[i] = out.Results[0].Result.Session.EndedAt.Format(time.RFC3339)
						})
					}
					workers.Wait()
					require.NoError(t, errors.Join(errs...))
					for _, got := range ended {
						assert.Equal(t, want, got)
					}
				}
				parseAll("2026-10-05T16:00:00Z")
				assert.Equal(t, int64(1), scans.Load())
				// Atomic replacement changes the generation even with identical size/mtime.
				info, err := os.Stat(index)
				require.NoError(t, err)
				replacement := index + ".new"
				writeSourceFile(t, replacement, strings.ReplaceAll(text, "16:00:00", "17:00:00"))
				require.NoError(t, os.Chtimes(replacement, info.ModTime(), info.ModTime()))
				require.NoError(t, os.Rename(replacement, index))
				parseAll("2026-10-05T17:00:00Z")
				assert.Equal(t, int64(2), scans.Load())
				require.NoError(t, closeCache())
				writeSourceFile(t, index, text)
			}
		})
	}
}

func TestMusePersonalUnverifiedIndexLoadCannotEstablishFreshness(t *testing.T) {
	for _, failure := range []string{"cancel", "replace", "remove", "cache"} {
		t.Run(failure, func(t *testing.T) {
			p, path := musePersonalTestProvider(t, "")
			index := filepath.Join(filepath.Dir(path), "sessions.json")
			writeSourceFile(t, index, `{"version":1}`)
			sources, err := p.Discover(t.Context())
			require.NoError(t, err)
			require.Len(t, sources, 1)
			base, cancel := context.WithCancel(t.Context())
			defer cancel()
			ctx, closeCache, err := WithReconciliationCache(WithSharedContainerScanObserver(base, func() {
				switch failure {
				case "cancel":
					cancel()
				case "replace":
					writeSourceFile(t, index+".new", `{"version":2}`)
					require.NoError(t, os.Rename(index+".new", index))
				case "remove":
					require.NoError(t, os.Remove(index))
				}
			}))
			require.NoError(t, err)
			if failure == "cache" {
				require.NoError(t, closeCache())
			}
			_, err = p.Fingerprint(ctx, sources[0])
			require.Error(t, err, "an unverified load cannot establish freshness")
			if failure != "cache" {
				require.NoError(t, closeCache())
			}
		})
	}
}

func TestMusePersonalDuplicateIdentityIsIncomplete(t *testing.T) {
	p, path := musePersonalTestProvider(t, "")
	root := filepath.Dir(filepath.Dir(filepath.Dir(path)))
	duplicate := filepath.Join(root, "agent-aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", "sessions", musePersonalTestID+".jsonl")
	writeSourceFile(t, duplicate, strings.ReplaceAll(musePersonalTestHeader, "66666666-7777-4888-8999-000000000000", "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"))
	sources, err := p.Discover(t.Context())
	require.ErrorContains(t, err, "duplicate session identity")
	require.Len(t, sources, 1)
	discovery, err := DiscoverRawCaptureSources(t.Context(), p)
	require.ErrorContains(t, err, "duplicate session identity")
	assert.False(t, discovery.Complete)
}
