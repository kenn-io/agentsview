package rawcapture

import (
	"encoding/json/jsontext"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/codexcloud/mirror"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/zstdframe"
)

// A fetcher that crashed mid-write leaves a torn frame, and the next append
// cuts it before writing. A capture taken while the torn bytes existed must
// not be extended by that rewritten tail.
func TestCapturerReplacesCodexCloudMirrorAfterTornTail(t *testing.T) {
	store, _ := openCapturerTestStore(t, 1<<20)
	root := t.TempDir()
	path := filepath.Join(root, "account-key", "thread-a.jsonl.zstd")
	record := func(id string) []mirror.Record {
		return []mirror.Record{{
			Kind: mirror.KindThread, FetchedAt: time.Unix(1759320000, 0).UTC(),
			Data: jsontext.Value(`{"id":"` + id + `","createdAt":1759320000}`),
		}}
	}
	first, err := mirror.Append(path, 0, record("thread-a"))
	require.NoError(t, err)
	frame, err := zstdframe.EncodeFrame([]byte("{\"kind\":\"turn\"}\n"))
	require.NoError(t, err)
	require.NoError(t, appendFile(path, string(frame[:len(frame)/2])))

	provider, ok := parser.NewProvider(parser.AgentCodexCloud, parser.ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	discovery, err := parser.DiscoverRawCaptureSources(t.Context(), provider)
	require.NoError(t, err)
	require.Len(t, discovery.Sources, 1)
	source := discovery.Sources[0]
	capturer := New(store)
	_, err = capturer.Capture(t.Context(), provider, source)
	require.NoError(t, err)

	appended, err := mirror.Append(path, first.Offset, record("thread-a-renamed"))
	require.NoError(t, err)
	require.True(t, appended.Truncated)
	result, err := capturer.Capture(t.Context(), provider, source)

	require.NoError(t, err)
	info, err := os.Stat(path)
	require.NoError(t, err)
	base, ok, err := store.CaptureBase(t.Context(), result.Source)
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, base.Entries, 1)
	require.Len(t, base.Entries[0].Objects, 1, "a cut tail must replace the captured file")
	assert.Equal(t, info.Size(), base.Entries[0].Objects[0].Length)
}
