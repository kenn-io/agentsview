package parser

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCopilotStoreFingerprintWorkScalesWithChangedUsage(t *testing.T) {
	for _, count := range []int{8, 800} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			root := t.TempDir()
			store := createCopilotUsageStore(t, root)
			tx, err := store.Begin()
			require.NoError(t, err)
			for i := range count {
				id := fmt.Sprintf("session-%04d", i)
				_, err := tx.Exec(`INSERT INTO sessions VALUES (?);
     INSERT INTO assistant_usage_events(session_id,model,input_tokens,output_tokens,created_at)
     VALUES (?, 'gpt-5.4', 100, 3, '2026-09-04T17:00:02Z')`, id, id)
				require.NoError(t, err)
				path := filepath.Join(root, copilotStateDir, id, "events.jsonl")
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
				text := fmt.Sprintf("{\"type\":\"session.start\",\"timestamp\":\"2026-09-04T17:00:00Z\",\"data\":{\"sessionId\":%q}}\n", id) +
					fmt.Sprintf("{\"type\":\"assistant.message\",\"timestamp\":\"2026-09-04T17:00:01Z\",\"data\":{\"content\":%q,\"outputTokens\":3}}\n", strings.Repeat("x", 8192))
				require.NoError(t, os.WriteFile(path, []byte(text), 0o644))
			}
			require.NoError(t, tx.Commit())
			provider := newCopilotTestProvider(t, root)
			sources, err := provider.Discover(t.Context())
			require.NoError(t, err)
			before := make(map[string]SourceFingerprint)
			for _, source := range sources {
				fp, err := provider.Fingerprint(t.Context(), source)
				require.NoError(t, err)
				before[source.Key] = fp
			}
			// A fresh provider restores transcript metadata from the archive's
			// fingerprint but independently rebuilds usage hashes from the store.
			cold := newCopilotTestProvider(t, root)
			for _, source := range sources {
				fp, err := cold.FingerprintWithStored(t.Context(), source, func(path string) (string, bool) {
					stored, ok := before[path]
					return stored.Hash, ok
				})
				require.NoError(t, err)
				assert.Equal(t, before[source.Key], fp)
			}
			assert.Zero(t, cold.sources.cache.transcriptBytes)
			provider = cold
			cache := provider.sources.cache
			bytesBefore, rowsBefore := cache.transcriptBytes, cache.usageRows
			_, err = store.Exec(`INSERT INTO assistant_usage_events(session_id,model,input_tokens,output_tokens,created_at)
    VALUES ('session-0000','gpt-5.4',100,7,'2026-09-04T17:00:03Z')`)
			require.NoError(t, err)
			changed, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{Path: filepath.Join(root, "session-store.db-wal"), EventKind: "write"})
			require.NoError(t, err)
			var changedKeys []string
			for _, source := range changed {
				fp, err := provider.Fingerprint(t.Context(), source)
				require.NoError(t, err)
				if fp.Hash != before[source.Key].Hash {
					changedKeys = append(changedKeys, source.Key)
				}
				assert.Equal(t, before[source.Key].MTimeNS, fp.MTimeNS)
			}
			assert.Equal(t, []string{filepath.Join(root, copilotStateDir, "session-0000", "events.jsonl")}, changedKeys)
			assert.Zero(t, cache.transcriptBytes-bytesBefore, "a store-only event must not reread transcript payloads")
			assert.Equal(t, int64(2), cache.usageRows-rowsBefore, "only the changed session's two usage rows are read")
		})
	}
}
