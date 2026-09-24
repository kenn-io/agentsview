package nanoclaw

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/nanoclaw/nanoclawtest"
)

type resolved struct {
	persona, channel string
	excluded, ok     bool
}

func resolveCell(t *testing.T, r *Resolver, data string) map[string]resolved {
	t.Helper()
	out := map[string]resolved{}
	for _, cs := range nanoclawtest.CellSessions {
		p, c, ex, ok := r.Resolve(t.Context(), nanoclawtest.SessionPath(data, cs[0], cs[1]))
		out[cs[1]] = resolved{p, c, ex, ok}
	}
	return out
}

func TestResolver(t *testing.T) {
	excl := resolved{excluded: true, ok: true}
	tests := []struct {
		name     string
		removeDB bool
		filter   Filter
		want     map[string]resolved
	}{
		{
			name: "nanoclaw_discovers_and_maps_persona_channel",
			want: map[string]resolved{
				"s-1": {"helper", "general", false, true},
				"s-2": {"reviewer", "events team", false, true},
				"s-3": {"helper", "steering committee", false, true},
				// Unknown agent dir: persona falls back to the dir name, no channel.
				"s-4": {"ag-unknown", "", false, true},
			},
		},
		{
			name:   "nanoclaw_exclude_matches_persona_and_folder",
			filter: Filter{Exclude: []string{"reviewer", "steering"}},
			// reviewer (persona), steering (folder), unmapped (fail closed).
			want: map[string]resolved{
				"s-1": {"helper", "general", false, true},
				"s-2": excl, "s-3": excl, "s-4": excl,
			},
		},
		{
			name:   "nanoclaw_include_is_explicit_allowlist",
			filter: Filter{Include: []string{"helper"}},
			want: map[string]resolved{
				"s-1": {"helper", "general", false, true},
				"s-2": excl,
				"s-3": {"helper", "steering committee", false, true},
				"s-4": excl,
			},
		},
		{
			name:   "nanoclaw_include_is_explicit_allowlist/exclude_wins",
			filter: Filter{Include: []string{"helper"}, Exclude: []string{"steering"}},
			want: map[string]resolved{
				"s-1": {"helper", "general", false, true},
				"s-2": excl, "s-3": excl, "s-4": excl,
			},
		},
		{
			name:     "nanoclaw_missing_db_defaults_personas_to_dir_names",
			removeDB: true,
			want: map[string]resolved{
				"s-1": {"ag-1", "", false, true},
				"s-2": {"ag-2", "", false, true},
				"s-3": {"ag-3", "", false, true},
				"s-4": {"ag-unknown", "", false, true},
			},
		},
		{
			name:     "nanoclaw_missing_db_defaults_personas_to_dir_names/include_admits_nothing",
			removeDB: true,
			filter:   Filter{Include: []string{"helper"}},
			want:     map[string]resolved{"s-1": excl, "s-2": excl, "s-3": excl, "s-4": excl},
		},
		{
			name:     "nanoclaw_exclude_only_fails_closed_without_db",
			removeDB: true,
			filter:   Filter{Exclude: []string{"reviewer"}},
			want:     map[string]resolved{"s-1": excl, "s-2": excl, "s-3": excl, "s-4": excl},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := t.TempDir()
			nanoclawtest.WriteCell(t, data)
			if tt.removeDB {
				require.NoError(t, os.Remove(filepath.Join(data, "v2.db")))
			}
			assert.Equal(t, tt.want, resolveCell(t, NewResolver(data, "", tt.filter), data))
		})
	}
}

func TestResolverIgnoresNonCellPaths(t *testing.T) {
	data := t.TempDir()
	nanoclawtest.WriteCell(t, data)
	r := NewResolver(data, "", Filter{Exclude: []string{"reviewer"}})
	for _, path := range []string{
		filepath.Join(t.TempDir(), "projects", "-w", "plain.jsonl"),
		filepath.Join(data, "v2-sessions-old", "ag-1", ".claude-shared", "projects", "-w", "s.jsonl"),
		"host:" + nanoclawtest.SessionPath(data, "ag-1", "s-1"),
		"",
	} {
		persona, channel, excluded, ok := r.Resolve(t.Context(), path)
		assert.False(t, ok, path)
		assert.False(t, excluded, path)
		assert.Empty(t, persona+channel, path)
	}
}

func TestResolverCustomDBPath(t *testing.T) {
	data := t.TempDir()
	nanoclawtest.WriteCell(t, data)
	elsewhere := filepath.Join(t.TempDir(), "routing.db")
	require.NoError(t, os.Rename(filepath.Join(data, "v2.db"), elsewhere))
	p, c, ex, ok := NewResolver(data, elsewhere, Filter{}).
		Resolve(t.Context(), nanoclawtest.SessionPath(data, "ag-2", "s-2"))
	assert.Equal(t, resolved{"reviewer", "events team", false, true}, resolved{p, c, ex, ok})
}

func TestResolverReload(t *testing.T) {
	s1 := func(data string) string { return nanoclawtest.SessionPath(data, "ag-1", "s-1") }

	t.Run("rewrite_reloads", func(t *testing.T) {
		data := t.TempDir()
		dbPath := nanoclawtest.WriteV2DB(t, data)
		r := NewResolver(data, "", Filter{})
		persona, _, _, _ := r.Resolve(t.Context(), s1(data))
		require.Equal(t, "helper", persona)
		nanoclawtest.Exec(t, dbPath, `UPDATE agent_groups SET name = 'assistant' WHERE id = 'ag-1'`)
		later := time.Now().Add(2 * time.Second)
		require.NoError(t, os.Chtimes(dbPath, later, later))
		persona, _, _, _ = r.Resolve(t.Context(), s1(data))
		assert.Equal(t, "assistant", persona)
	})

	t.Run("wal_write_reloads", func(t *testing.T) {
		data := t.TempDir()
		dbPath := nanoclawtest.WriteV2DB(t, data)
		writer, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL")
		require.NoError(t, err)
		t.Cleanup(func() { _ = writer.Close() })
		writer.SetMaxOpenConns(1)
		_, err = writer.ExecContext(t.Context(), `PRAGMA wal_autocheckpoint=0`)
		require.NoError(t, err)

		r := NewResolver(data, "", Filter{})
		persona, _, _, _ := r.Resolve(t.Context(), s1(data))
		require.Equal(t, "helper", persona)
		// The write lands in v2.db-wal; v2.db itself is not checkpointed.
		_, err = writer.ExecContext(t.Context(), `UPDATE agent_groups SET name = 'assistant' WHERE id = 'ag-1'`)
		require.NoError(t, err)
		persona, _, _, _ = r.Resolve(t.Context(), s1(data))
		assert.Equal(t, "assistant", persona)
	})

	t.Run("missing_then_restored_db_reloads", func(t *testing.T) {
		data := t.TempDir()
		r := NewResolver(data, "", Filter{Exclude: []string{"reviewer"}})
		_, _, excluded, _ := r.Resolve(t.Context(), s1(data))
		require.True(t, excluded, "fail closed while v2.db is missing")
		nanoclawtest.WriteV2DB(t, data)
		persona, channel, excluded, ok := r.Resolve(t.Context(), s1(data))
		assert.Equal(t, resolved{"helper", "general", false, true}, resolved{persona, channel, excluded, ok})
	})

	t.Run("cancelled_load_is_not_cached", func(t *testing.T) {
		data := t.TempDir()
		nanoclawtest.WriteV2DB(t, data)
		r := NewResolver(data, "", Filter{Include: []string{"helper"}})
		cancelled, cancel := context.WithCancel(t.Context())
		cancel()
		_, _, excluded, _ := r.Resolve(cancelled, s1(data))
		assert.True(t, excluded, "a failed load under a filter fails closed for this call")
		persona, _, excluded, _ := r.Resolve(t.Context(), s1(data))
		assert.False(t, excluded)
		assert.Equal(t, "helper", persona)
	})
}

func TestResolverSymlinkedDataDir(t *testing.T) {
	dataDir := t.TempDir()
	nanoclawtest.WriteCell(t, dataDir)
	link := filepath.Join(t.TempDir(), "cell")
	if err := os.Symlink(dataDir, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	resolvedReal, err := filepath.EvalSymlinks(dataDir)
	require.NoError(t, err)
	r := NewResolver(link, "", Filter{})
	for _, dir := range []string{dataDir, resolvedReal, link} {
		persona, _, _, ok := r.Resolve(t.Context(), nanoclawtest.SessionPath(dir, "ag-1", "s-1"))
		assert.True(t, ok, dir)
		assert.Equal(t, "helper", persona, dir)
	}
	assert.Contains(t, r.SessionRoots(), filepath.Join(resolvedReal, "v2-sessions"))
}

func TestResolverFingerprint(t *testing.T) {
	data := t.TempDir()
	r := NewResolver(data, "", Filter{Exclude: []string{"reviewer"}})
	missing := r.Fingerprint(t.Context())
	dbPath := nanoclawtest.WriteV2DB(t, data)
	loaded := r.Fingerprint(t.Context())
	assert.NotEqual(t, missing, loaded, "the DB appearing changes the fingerprint")
	assert.Equal(t, loaded, r.Fingerprint(t.Context()), "stable while nothing changes")

	nanoclawtest.Exec(t, dbPath, `UPDATE messaging_groups SET name = 'renamed' WHERE id = 'mg-1'`)
	later := time.Now().Add(2 * time.Second)
	require.NoError(t, os.Chtimes(dbPath, later, later))
	assert.NotEqual(t, loaded, r.Fingerprint(t.Context()), "a channel rename changes it")

	other := NewResolver(data, "", Filter{})
	assert.NotEqual(t, r.Fingerprint(t.Context()), other.Fingerprint(t.Context()), "the filter is part of it")
}
