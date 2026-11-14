//go:build fts5

package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnicode61FoldMatchesSQLiteTokenizer(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	_, err := d.getWriter().ExecContext(ctx, `
		CREATE VIRTUAL TABLE unicode61_probe USING fts5(token, tokenize='unicode61');
		CREATE VIRTUAL TABLE unicode61_probe_vocab USING fts5vocab(unicode61_probe, 'instance');
	`)
	require.NoError(t, err)

	for i, tc := range []struct{ input, want string }{
		{"café", "cafe"},
		{"teſt", "test"},
		{"ς", "σ"},
		{"µ", "μ"},
		{"ϐ", "β"},
		{"ǅ", "ǆ"},
		{"αρχή", "αρχή"},
		{"a\u0300", "a"},
	} {
		_, err := d.getWriter().ExecContext(ctx,
			`INSERT INTO unicode61_probe(rowid, token) VALUES (?, ?)`, i+1, tc.input)
		require.NoError(t, err)
		var token string
		err = d.getWriter().QueryRowContext(ctx,
			`SELECT term FROM unicode61_probe_vocab WHERE doc = ?`, i+1,
		).Scan(&token)
		require.NoError(t, err)
		assert.Equal(t, tc.want, token)
		assert.Equal(t, token, foldUnicode61(tc.input))
	}
}
