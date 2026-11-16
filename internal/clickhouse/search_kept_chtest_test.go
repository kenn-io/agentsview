//go:build chtest

package clickhouse

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/docbank/document/ocr"
)

func restartSearchTestMerges(t *testing.T, s *Store, tables ...string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Second)
		defer cancel()
		for _, table := range tables {
			_, err := s.conn.ExecContext(ctx, "SYSTEM START MERGES "+table)
			assert.NoError(t, err, "restart merges for %s", table)
		}
	})
}

func TestSearchKeptRestart(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	first, syncer, local := newPushedStore(t)
	ctx := t.Context()
	image := inlineImage("kept")
	writeImageSession(t, local, image, inlineImage("other"), "2026-01-12T00:00:00.000Z")
	require.NoError(t, local.BackfillImageTextOccurrences(ctx))
	hash := db.RetainedToolResultImages(image)[0].SHA256
	identity := ocr.Identity{Fingerprint: "proc-a", Provider: "tesseract", Model: "eng", Revision: "r1"}
	require.NoError(t, local.SaveImageText(ctx, hash, identity, "invoice 4827", false))
	_, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	require.NoError(t, first.openActivityReportDisk(syncer.target))
	filter := db.SearchFilter{Query: "alpha"}
	read := func(s *Store) db.SearchPage {
		t.Helper()
		page, err := s.Search(t.Context(), filter)
		require.NoError(t, err)
		return page
	}
	count := func(s *Store) uint64 {
		t.Helper()
		n, _ := usageQueryCountsOf(t, s, "startsWith(trimLeft(query, ' \\n\\t\\r'), 'WITH msg_grouped AS')")
		return n
	}
	want := read(first)
	require.NotEmpty(t, want.Results)
	require.Positive(t, count(first))
	imageFilter := db.SearchFilter{Query: "invoice"}
	beforeImage := count(first)
	imagePage, err := first.Search(ctx, imageFilter)
	require.NoError(t, err)
	require.Len(t, imagePage.Results, 1)
	require.Greater(t, count(first), beforeImage)
	reopen := func() *Store {
		s := newTestStore(t, syncer.conn, syncer.target)
		require.NoError(t, s.openActivityReportDisk(syncer.target))
		return s
	}
	s := reopen()
	before := count(s)
	require.Equal(t, want, read(s))
	require.Equal(t, before, count(s))
	other, err := s.Search(t.Context(), db.SearchFilter{Query: "unmatched-token"})
	require.NoError(t, err)
	require.Empty(t, other.Results)
	require.Greater(t, count(s), before)
	parts, err := first.tablePartsFingerprint(t.Context(), []string{"sessions", "messages"})
	require.NoError(t, err)
	restartSearchTestMerges(t, first, "sessions", "messages", "image_text")
	execPreparedUsage(t, first, "SYSTEM STOP MERGES sessions", "SYSTEM STOP MERGES messages", "INSERT INTO sessions SELECT * FROM sessions", "INSERT INTO messages SELECT * FROM messages", "SYSTEM START MERGES sessions", "SYSTEM START MERGES messages", "OPTIMIZE TABLE sessions FINAL", "OPTIMIZE TABLE messages FINAL", "SYSTEM STOP MERGES image_text", "INSERT INTO image_text SELECT * FROM image_text", "SYSTEM START MERGES image_text", "OPTIMIZE TABLE image_text FINAL")
	after, err := first.tablePartsFingerprint(t.Context(), []string{"sessions", "messages"})
	require.NoError(t, err)
	require.NotEqual(t, parts, after)
	s = reopen()
	before = count(s)
	require.Equal(t, want, read(s))
	require.Equal(t, before, count(s))
	before = count(s)
	require.Positive(t, before)
	got, err := s.Search(ctx, imageFilter)
	require.NoError(t, err)
	require.Equal(t, imagePage, got)
	require.Equal(t, before, count(s))
	require.NoError(t, local.SaveImageText(ctx, hash, identity, "receipt 4827", false))
	_, err = syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	s = reopen()
	got, err = s.Search(ctx, imageFilter)
	require.NoError(t, err)
	require.Empty(t, got.Results)
	got, err = s.Search(ctx, db.SearchFilter{Query: "receipt"})
	require.NoError(t, err)
	require.Len(t, got.Results, 1)
	appendMessage(t, local, fixtureAlphaID, "alpha changed", "2026-01-12T05:00:00Z")
	_, err = syncer.Push(t.Context(), false, nil)
	require.NoError(t, err)
	s = reopen()
	before = count(s)
	read(s)
	require.Greater(t, count(s), before)
}
