package sync

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// codebuddyBlobPNG is a 1x1 PNG.
const codebuddyBlobPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="

// An image missing from the blob store when its conversation is first
// imported shows a placeholder. Once the image is restored, an ordinary sync
// replaces the placeholder with the image, though the transcript is unchanged,
// and saves the image's bytes in the asset store the link points to.
func TestSyncCodeBuddyRestoredBlobReplacesPlaceholder(t *testing.T) {
	home, project := cbcSyncHome(t)
	blob := strings.Repeat("cd", 32)
	writeCBCSync(t, filepath.Join(project, "s1.jsonl"),
		`{"id":"u1","type":"message","role":"user","timestamp":1767225600000,"content":[{"type":"input_text","text":"what is this"},{"type":"image_blob_ref","blob_id":"`+blob+`","mime":"image/png","original_filename":"shot.png"}]}`,
		`{"id":"a1","parentId":"u1","type":"message","role":"assistant","timestamp":1767225601000,"content":[{"type":"output_text","text":"a pixel"}]}`,
	)
	database := openTestDB(t)
	assetsDir := t.TempDir()
	database.SetAssetsDir(assetsDir)
	engine := newCBCEngine(t, database, home)
	prompt := func() string {
		t.Helper()
		msgs, err := database.GetMessages(t.Context(), "codebuddy:s1", 0, 100, true)
		require.NoError(t, err)
		require.NotEmpty(t, msgs)
		return msgs[0].Content
	}
	engine.SyncAll(t.Context(), nil)
	assert.Contains(t, prompt(), "[image unavailable: shot.png]")

	png, err := base64.StdEncoding.DecodeString(codebuddyBlobPNG)
	require.NoError(t, err)
	stored := filepath.Join(home, "blobs", blob[:2], blob+".png")
	require.NoError(t, os.MkdirAll(filepath.Dir(stored), 0o755))
	require.NoError(t, os.WriteFile(stored, png, 0o600))
	engine.SyncAll(t.Context(), nil)
	got := prompt()
	assert.Contains(t, got, "![image](asset://", "the restored image replaces the placeholder")
	assert.NotContains(t, got, "[image unavailable")
	link := regexp.MustCompile(`asset://([0-9a-f]{64}\.png)`).FindStringSubmatch(got)
	require.Len(t, link, 2, "the prompt links the image by its content address: %q", got)
	saved, err := os.ReadFile(filepath.Join(assetsDir, link[1]))
	require.NoError(t, err, "the restored image is saved in the asset store")
	assert.Equal(t, png, saved)
}
