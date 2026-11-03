package rawcapture

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCaptureUsesCheckpointIdentityForReuseAndAppend(t *testing.T) {
	for _, appendContent := range []bool{false, true} {
		name := "reuse"
		if appendContent {
			name = "append"
		}
		t.Run(name, func(t *testing.T) {
			store, _ := openCapturerTestStore(t, 1<<20)
			provider, _, path := captureFileProvider(t, "one\n")
			capturer := New(store)
			scope, err := openCapturePlanScope(provider.plan)
			require.NoError(t, err)
			defer scope.Close()
			observed, _, err := capturer.observePlan(scope)
			require.NoError(t, err)
			base, _, err := capturer.captureFile(t.Context(), observed[0])
			require.NoError(t, err)
			// A saved volume identity differs from the live device number.
			base.FileIdentity = "volume-uuid:inode"
			if appendContent {
				require.NoError(t, appendFile(path, "two\n"))
			}
			current, _, err := capturer.observePlan(scope)
			require.NoError(t, err)
			current[0].checkpointIdentity = base.FileIdentity
			if appendContent {
				entry, _, err := capturer.captureAppendFile(t.Context(), current[0], base)
				require.NoError(t, err)
				assert.Equal(t, int64(8), entry.Length)
				require.Len(t, entry.Objects, 2)
				assert.Equal(t, base.Objects[0], entry.Objects[0])
				require.NoError(t, capturer.verifyCapturedFile(t.Context(), current[0], capturedFileState{
					length: entry.Length, modTimeNS: entry.ModTimeNS,
					fileIdentity: entry.FileIdentity, prefixSHA256: entry.PrefixSHA256,
				}))
			} else {
				entry, err := capturer.captureReusedFile(t.Context(), current[0], base)
				require.NoError(t, err)
				assert.Equal(t, base.Objects, entry.Objects)
				assert.Equal(t, base.PrefixSHA256, entry.PrefixSHA256)
			}
		})
	}
}
