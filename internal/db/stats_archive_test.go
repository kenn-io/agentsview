package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFileBackedSessionCountForArchiveSource(t *testing.T) {
	database := testDB(t)
	write := archiveProvenanceWrite(t, database)
	write.Session.MessageCount = 1
	_, err := database.WriteSessionBatchAtomic(t.Context(), []SessionBatchWrite{write})
	require.NoError(t, err)
	insertSession(t, database, write.Session.Machine+"~unowned", "project", func(s *Session) {
		s.Machine = write.Session.Machine
	})

	for _, tc := range []struct {
		name   string
		change func(*ArchiveSourceProvenance)
		want   int
	}{
		{name: "selected source", want: 1},
		{name: "same source in another snapshot", change: func(p *ArchiveSourceProvenance) { p.Attachment.SnapshotID = "another-snapshot" }, want: 1},
		{name: "another archive", change: func(p *ArchiveSourceProvenance) { p.Source.ArchiveID = "another-archive" }},
		{name: "another device", change: func(p *ArchiveSourceProvenance) { p.Source.DeviceID = "cccccccccccccccccccccccccccccccc" }},
		{name: "another root", change: func(p *ArchiveSourceProvenance) { p.Source.RootID = "another-root" }},
		{name: "another source", change: func(p *ArchiveSourceProvenance) { p.Source.SourceKey = "another-source" }},
		{name: "another manifest", change: func(p *ArchiveSourceProvenance) { p.ManifestID = "another-manifest" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := write.ArchiveProvenance.ArchiveSourceProvenance
			if tc.change != nil {
				tc.change(&p)
			}
			count, err := database.FileBackedSessionCountForArchiveSource(t.Context(), p)
			require.NoError(t, err)
			assert.Equal(t, tc.want, count)
		})
	}

	require.NoError(t, database.SoftDeleteSession(t.Context(), write.Session.ID))
	count, err := database.FileBackedSessionCountForArchiveSource(t.Context(), write.ArchiveProvenance.ArchiveSourceProvenance)
	require.NoError(t, err)
	assert.Zero(t, count, "trashed history does not require source rediscovery")
}
