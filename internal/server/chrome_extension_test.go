package server

import (
	"archive/zip"
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChromeExtensionDownload(t *testing.T) {
	s := newSPATestServer(t, WithBasePath("/viewer"))
	s.spaFS = fstest.MapFS{
		"chrome-extension/manifest.json": {Data: []byte(`{"manifest_version":3}`)},
		"chrome-extension/worker.js":     {Data: []byte("chrome.action.onClicked.addListener(() => {});")},
		"index.html":                     {Data: []byte(testSPAIndex)},
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/viewer/chrome-extension.zip", nil))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "application/zip", w.Header().Get("Content-Type"))
	assert.Equal(t, `attachment; filename="chrome-extension.zip"`, w.Header().Get("Content-Disposition"))
	archive, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	require.NoError(t, err)
	files := make(map[string]string)
	for _, file := range archive.File {
		reader, err := file.Open()
		require.NoError(t, err)
		data, err := io.ReadAll(reader)
		require.NoError(t, err)
		require.NoError(t, reader.Close())
		files[file.Name] = string(data)
	}
	assert.Equal(t, map[string]string{
		"manifest.json": `{"manifest_version":3}`,
		"worker.js":     "chrome.action.onClicked.addListener(() => {});",
	}, files)
}

func TestChromeExtensionDownloadWithoutBuild(t *testing.T) {
	s := newSPATestServer(t)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/chrome-extension.zip", nil))
	assert.Equal(t, http.StatusNotFound, w.Code)
}
