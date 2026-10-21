package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/fileevidence"
)

func TestFileEvidenceFixture(t *testing.T) {
	d, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "sessions.db"))
	require.NoError(t, err)
	defer d.Close()
	d.SetAssetsDir(filepath.Join(t.TempDir(), "assets"))
	require.NoError(t, createFileEvidenceFixture(t.Context(), d, time.Now()))
	page, err := d.ReadFileEvidenceSnapshot(t.Context(), "test-session-file-references", fileevidence.Window{})
	require.NoError(t, err)
	require.Len(t, page.Rows, 4)
	assert.Equal(t, "complete", page.Coverage.State)
	assert.Equal(t, "image/png", page.Rows[0].Record.MediaType)
	assert.NotEmpty(t, page.Rows[0].Record.AssetRef)
	assert.Equal(t, []int{0, 1, 1, 2}, []int{page.Rows[0].MessageOrdinal, page.Rows[1].MessageOrdinal, page.Rows[2].MessageOrdinal, page.Rows[3].MessageOrdinal})
	assert.Equal(t, 0, page.Rows[1].Record.Position.Call)
	assert.Equal(t, 1, page.Rows[2].Record.Position.Call)
	for _, row := range page.Rows {
		assert.Len(t, row.Record.SHA256, 64)
	}
}
