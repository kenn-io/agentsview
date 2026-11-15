//go:build fts5

package db

import (
	"strings"
	"testing"
	"unicode"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnicode61FoldMatchesSQLiteLettersAndMarks(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	_, err := d.getWriter().ExecContext(ctx, `
		CREATE VIRTUAL TABLE unicode61_letters USING fts5(token, tokenize='unicode61');
		CREATE VIRTUAL TABLE unicode61_letters_vocab USING fts5vocab(unicode61_letters, 'instance');
	`)
	require.NoError(t, err)
	tx, err := d.getWriter().BeginTx(ctx, nil)
	require.NoError(t, err)
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO unicode61_letters(rowid, token) VALUES (?, ?)`)
	require.NoError(t, err)
	defer stmt.Close()
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if unicode.IsLetter(r) {
			_, err = stmt.ExecContext(ctx, int(r)+1, string(r))
			require.NoError(t, err)
		}
		if unicode.IsMark(r) {
			_, err = stmt.ExecContext(ctx, int(unicode.MaxRune)+int(r)+2, "a"+string(r)+"b")
			require.NoError(t, err)
		}
	}
	require.NoError(t, stmt.Close())
	require.NoError(t, tx.Commit())
	rows, err := d.getWriter().QueryContext(ctx, `SELECT doc, term FROM unicode61_letters_vocab`)
	require.NoError(t, err)
	defer rows.Close()
	var letters, marks, mismatches int
	for rows.Next() {
		var doc int
		var token string
		require.NoError(t, rows.Scan(&doc, &token))
		input := string(rune(doc - 1))
		if doc > unicode.MaxRune+1 {
			// Marks that split the text into two tokens are separators, not folds.
			if !strings.HasPrefix(token, "a") || !strings.HasSuffix(token, "b") {
				continue
			}
			input = "a" + string(rune(doc-int(unicode.MaxRune)-2)) + "b"
			marks++
		} else {
			letters++
		}
		if got := strings.ToLower(foldUnicode61(input)); got != strings.ToLower(token) {
			mismatches++
			if mismatches <= 20 {
				t.Logf("%q: SQLite %q, fold %q", input, token, got)
			}
		}
	}
	require.NoError(t, rows.Err())
	assert.Positive(t, letters)
	assert.Positive(t, marks)
	assert.Zero(t, mismatches, "SQLite tokenizer and fold disagree on %d inputs", mismatches)
}
