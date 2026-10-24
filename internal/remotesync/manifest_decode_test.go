package remotesync

import (
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecodeManifestHandsOverEntriesBeforeTheReplyEnds(t *testing.T) {
	reader, writer := io.Pipe()
	got := make(chan ManifestEntry)
	done := make(chan error, 1)
	go func() {
		done <- DecodeManifest(reader, func(entry ManifestEntry) error {
			got <- entry
			return nil
		})
	}()

	_, err := io.WriteString(writer, `{"files":[{"path":"/a","size":1,"mtime_ns":2},`)
	require.NoError(t, err)
	// The reply is still open, so the entry can only arrive if the decoder
	// passes it on without waiting for the rest.
	assert.Equal(t, ManifestEntry{Path: "/a", Size: 1, MtimeNS: 2}, <-got)

	_, err = io.WriteString(writer, `{"path":"/b","size":3,"mtime_ns":4}]}`)
	require.NoError(t, err)
	assert.Equal(t, ManifestEntry{Path: "/b", Size: 3, MtimeNS: 4}, <-got)
	require.NoError(t, writer.Close())
	require.NoError(t, <-done)
}

func TestDecodeManifestBoundsEachEntry(t *testing.T) {
	// The longest Windows path is 32,767 UTF-16 units; a three-byte UTF-8
	// character per unit is the most a real path can take.
	longest := "/" + strings.Repeat("中", 32767)
	tests := []struct {
		name    string
		path    string
		wantErr error
	}{
		{"longest real path", longest, nil},
		{"entry past the cap", "/" + strings.Repeat("a", manifestEntryMaxBytes), errManifestEntryTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reply := `{"files":[{"path":"/first","size":1,"mtime_ns":1},` +
				`{"path":"` + tt.path + `","size":1,"mtime_ns":1}]}`
			var paths []string

			err := DecodeManifest(strings.NewReader(reply), func(entry ManifestEntry) error {
				paths = append(paths, entry.Path)
				return nil
			})

			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				assert.Equal(t, []string{"/first"}, paths)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, []string{"/first", tt.path}, paths)
		})
	}
}

func TestDecodeManifestRejectsMalformedReplies(t *testing.T) {
	for name, reply := range map[string]string{
		"data after the object":  `{"files":[]} {}`,
		"files is not an array":  `{"files":{}}`,
		"reply is not an object": `[]`,
		"reply ends early":       `{"files":[{"path":"/a"}`,
	} {
		t.Run(name, func(t *testing.T) {
			err := DecodeManifest(strings.NewReader(reply), func(ManifestEntry) error { return nil })

			require.Error(t, err)
		})
	}
}
