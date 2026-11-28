package sync

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// cbcImageSync lays out one CodeBuddy conversation whose prompt attaches a
// blob-store image, and returns the blob path, the image bytes, and a reader
// for the archived prompt. CodeBuddy names a blob by the SHA-256 of its bytes.
func cbcImageSync(t *testing.T, database *db.DB) (home, blobPath string, png []byte, prompt func() string) {
	t.Helper()
	home, project := cbcSyncHome(t)
	png, err := base64.StdEncoding.DecodeString(codebuddyBlobPNG)
	require.NoError(t, err)
	sum := sha256.Sum256(png)
	blob := hex.EncodeToString(sum[:])
	writeCBCSync(t, filepath.Join(project, "s1.jsonl"),
		`{"id":"u1","type":"message","role":"user","timestamp":1767225600000,"content":[{"type":"input_text","text":"what is this"},{"type":"image_blob_ref","blob_id":"`+blob+`","mime":"image/png","original_filename":"shot.png"}]}`,
		`{"id":"a1","parentId":"u1","type":"message","role":"assistant","timestamp":1767225601000,"content":[{"type":"output_text","text":"a pixel"}]}`,
	)
	blobPath = filepath.Join(home, "blobs", blob[:2], blob+".png")
	require.NoError(t, os.MkdirAll(filepath.Dir(blobPath), 0o755))
	require.NoError(t, os.WriteFile(blobPath, png, 0o600))
	prompt = func() string {
		t.Helper()
		msgs, err := database.GetMessages(t.Context(), "codebuddy:s1", 0, 100, true)
		require.NoError(t, err)
		require.NotEmpty(t, msgs)
		return msgs[0].Content
	}
	return home, blobPath, png, prompt
}

var cbcAssetLinkRe = regexp.MustCompile(`!\[image\]\(asset://([0-9a-f]{64}\.png)\)`)

// An image that could not be saved because the asset store was unwritable
// is saved by the next ordinary sync once the store is writable again, even
// though the transcript is unchanged.
func TestSyncCodeBuddyRetriesImageAfterAssetStoreRepair(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("directory permissions do not block writes here")
	}
	database := openTestDB(t)
	home, _, png, prompt := cbcImageSync(t, database)
	assetsDir := t.TempDir()
	require.NoError(t, os.Chmod(assetsDir, 0o555))
	t.Cleanup(func() { _ = os.Chmod(assetsDir, 0o755) })
	database.SetAssetsDir(assetsDir)
	engine := newCBCEngine(t, database, home)

	engine.SyncAll(t.Context(), nil)
	assert.NotContains(t, prompt(), "![image](asset://",
		"an image the store could not save is not linked")

	require.NoError(t, os.Chmod(assetsDir, 0o755))
	engine.SyncAll(t.Context(), nil)
	got := prompt()
	link := cbcAssetLinkRe.FindStringSubmatch(got)
	require.Len(t, link, 2, "the repaired store gets the image: %q", got)
	assert.NotContains(t, got, "[image unavailable")
	saved, err := os.ReadFile(filepath.Join(assetsDir, link[1]))
	require.NoError(t, err, "the retry saves the image in the asset store")
	assert.Equal(t, png, saved)
}

// Deleting an imported image's original keeps the archived image: the
// reparse that the deletion triggers finds the verified stored copy.
func TestSyncCodeBuddyKeepsArchivedImageAfterOriginalDeleted(t *testing.T) {
	database := openTestDB(t)
	home, blobPath, png, prompt := cbcImageSync(t, database)
	assetsDir := t.TempDir()
	database.SetAssetsDir(assetsDir)
	engine := newCBCEngine(t, database, home)

	engine.SyncAll(t.Context(), nil)
	first := cbcAssetLinkRe.FindStringSubmatch(prompt())
	require.Len(t, first, 2, "the first sync archives the image")

	require.NoError(t, os.Remove(blobPath))
	engine.SyncAll(t.Context(), nil)
	got := prompt()
	assert.NotContains(t, got, "[image unavailable", "the archived image survives")
	link := cbcAssetLinkRe.FindStringSubmatch(got)
	require.Len(t, link, 2, "the prompt still links the archived image: %q", got)
	assert.Equal(t, first[1], link[1])
	saved, err := os.ReadFile(filepath.Join(assetsDir, link[1]))
	require.NoError(t, err)
	assert.Equal(t, png, saved)
}
