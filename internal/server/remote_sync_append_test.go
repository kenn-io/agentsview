package server

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/remotesync"
)

// The HTTP archive route answers a delta append offer from the generated
// client with only the bytes past the offered length, and falls back to the
// whole file when the offered bytes do not match.
func TestRemoteSyncArchiveSendsAppendedBytes(t *testing.T) {
	t.Parallel()
	_, handler, sessionPath := newRemoteSyncServer(t)
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	base := strings.Repeat(`{"type":"user"}`+"\n", 1000)
	added := `{"type":"assistant"}` + "\n"
	require.NoError(t, os.WriteFile(sessionPath, []byte(base+added), 0o644))
	sum := sha256.Sum256([]byte(base))

	tests := []struct {
		name       string
		tail       string
		wantOffset string
		wantBody   string
	}{
		{
			name: "matching tail", tail: hex.EncodeToString(sum[:]),
			wantOffset: strconv.Itoa(len(base)), wantBody: added,
		},
		{name: "different tail", tail: strings.Repeat("0", 64), wantBody: base + added},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stream, err := remotesync.HTTPSource{URL: ts.URL, Token: "remote-token"}.Archive(
				t.Context(), remotesync.ArchiveRequest{
					Dirs: map[parser.AgentType][]string{
						parser.AgentClaude: {filepath.Dir(sessionPath)},
					},
					DeltaFiles: []string{sessionPath},
					DeltaAppends: []remotesync.DeltaAppend{{
						Path: sessionPath, Offset: int64(len(base)), TailSHA256: tc.tail,
					}},
				},
			)
			require.NoError(t, err)
			defer stream.Body.Close()
			var body io.Reader = stream.Body
			switch stream.Encoding {
			case remotesync.ArchiveEncodingNone:
			case remotesync.ArchiveEncodingZstd:
				decoder, err := remotesync.NewZstdReader(stream.Body)
				require.NoError(t, err)
				defer decoder.Close()
				body = decoder
			case remotesync.ArchiveEncodingGzip:
				decoder, err := gzip.NewReader(stream.Body)
				require.NoError(t, err)
				body = decoder
			}
			tr := tar.NewReader(body)
			hdr, err := tr.Next()
			require.NoError(t, err)
			assert.Equal(t, tc.wantOffset, hdr.PAXRecords["AGENTSVIEW.append_offset"])
			content, err := io.ReadAll(tr)
			require.NoError(t, err)
			assert.Equal(t, tc.wantBody, string(content))
			_, err = tr.Next()
			assert.ErrorIs(t, err, io.EOF)
		})
	}
}
