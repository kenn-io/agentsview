package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/remotesync"
)

func TestRemoteSyncArchiveEncoding(t *testing.T) {
	_, handler, sessionPath := newRemoteSyncServer(t)
	payload, err := json.Marshal(remotesync.ArchiveRequest{
		Dirs:       map[parser.AgentType][]string{parser.AgentClaude: {filepath.Dir(sessionPath)}},
		DeltaFiles: []string{sessionPath},
	})
	require.NoError(t, err)
	for _, tc := range []struct {
		name     string
		accept   string
		encoding string
	}{
		{"zstd", "zstd, gzip", "zstd"},
		{"server preference", "gzip, zstd", "zstd"},
		{"gzip", "gzip", "gzip"},
		{"no encoding", "", ""},
		{"zstd disabled", "zstd;q=0, gzip", "gzip"},
		{"gzip preferred", "zstd;q=0.5, gzip;q=1", "gzip"},
		{"all disabled", "zstd;q=0, gzip;q=0", ""},
		{"invalid zstd weight", "zstd;q=invalid, gzip", "gzip"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for i := range 2 {
				content := fmt.Sprintf("session content %d\n", i)
				require.NoError(t, os.WriteFile(sessionPath, []byte(content), 0o600))
				req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/remote-sync/archive", bytes.NewReader(payload))
				req.Header.Set("Authorization", "Bearer remote-token")
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Accept-Encoding", tc.accept)
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, req)
				require.Equal(t, http.StatusOK, w.Code)
				require.Equal(t, tc.encoding, w.Header().Get("Content-Encoding"))
				assert.Contains(t, w.Header().Values("Vary"), "Accept-Encoding")
				var reader io.Reader = bytes.NewReader(w.Body.Bytes())
				switch tc.encoding {
				case "zstd":
					dec, err := zstd.NewReader(reader)
					require.NoError(t, err)
					defer dec.Close()
					reader = dec
				case "gzip":
					dec, err := gzip.NewReader(reader)
					require.NoError(t, err)
					defer dec.Close()
					reader = dec
				}
				tr := tar.NewReader(reader)
				hdr, err := tr.Next()
				require.NoError(t, err)
				assert.Equal(t, "session.jsonl", filepath.Base(hdr.Name))
				got, err := io.ReadAll(tr)
				require.NoError(t, err)
				assert.Equal(t, content, string(got))
				_, err = tr.Next()
				require.ErrorIs(t, err, io.EOF)
				_, err = io.Copy(io.Discard, reader)
				require.NoError(t, err)
			}
		})
	}
}
