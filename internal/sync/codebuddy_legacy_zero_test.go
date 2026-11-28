package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// A 4.x request whose reply the transcript lacks and whose total was
// corrected to zero input and output tokens shows zero, even while another
// part of the 4.x folder no longer reads and the archive still holds the old
// total.
func TestSyncCodeBuddyConvertedKeepsLegacyTotalCorrectedToZero(t *testing.T) {
	for _, tc := range []struct {
		name        string
		breakFolder func(t *testing.T, convDir string)
	}{
		{"message file deleted", func(t *testing.T, convDir string) {
			require.NoError(t, os.Remove(filepath.Join(convDir, "messages", "a1.json")))
		}},
		{"image deleted", func(t *testing.T, convDir string) {
			require.NoError(t, os.RemoveAll(filepath.Join(convDir, "assets")))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := openTestDB(t)
			convDir, _, engine, _ := cbcConvertedImageSync(t, database, map[string]int{"a1": 40, "a2": 5})
			require.Equal(t, 45, cbcDailyTotals(t, database).OutputTokens,
				"a2 has no reply in the transcript and still counts")

			require.NoError(t, os.WriteFile(filepath.Join(convDir, "messages", "a2.json"),
				[]byte(`{"role":"assistant","createdAt":"2026-01-01T00:00:02Z","extra":{"requestId":"req-a2","modelId":"glm-x","statsSnapshot":{"inputTokens":0,"outputTokens":0}}}`), 0o600))
			tc.breakFolder(t, convDir)
			engine.SyncAll(t.Context(), nil)
			assert.Equal(t, 40, cbcDailyTotals(t, database).OutputTokens,
				"a2 now reads 0 and its archived total stays gone")
			msgs, err := database.GetMessages(t.Context(), "codebuddy:conv1", 0, 100, true)
			require.NoError(t, err)
			var zero []db.Message
			for _, m := range msgs {
				if m.SourceUUID == "cb4:req-a2:a2" {
					zero = append(zero, m)
				}
			}
			require.Len(t, zero, 1, "the archive keeps a2's request identity")
			assert.True(t, zero[0].IsSystem)
			assert.Zero(t, zero[0].OutputTokens)

			engine.SyncAllForceParse(t.Context(), nil)
			assert.Equal(t, 40, cbcDailyTotals(t, database).OutputTokens,
				"a forced parse keeps a2 at 0")
		})
	}
}
