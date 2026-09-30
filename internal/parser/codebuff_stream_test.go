package parser

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecodeCodebuffMessagesSkipsNonObjectElements(t *testing.T) {
	sessionDate := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	transcript := `[1, "x", {"id":"u1","variant":"user","content":"hello","timestamp":"10:00 AM"}]`
	out, err := decodeCodebuffMessages(
		strings.NewReader(transcript), sessionDate, "codebuff:p:s")
	require.NoError(t, err)
	require.Len(t, out.Messages, 1)
	assert.Equal(t, "hello", out.Messages[0].Content)
	assert.Equal(t, 0, out.Messages[0].Ordinal)
}

func TestDecodeCodebuffMessagesInvalidJSON(t *testing.T) {
	sessionDate := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	const user = `{"id":"u1","variant":"user","content":"hello","timestamp":"10:00 AM"}`
	tests := []struct {
		name       string
		transcript string
	}{
		{"empty file", ``},
		{"object root", `{"messages":[]}`},
		{"malformed first element", `[not json`},
		{"cut off after a message", `[` + user + `,{"id":"u2","vari`},
		{"missing closing bracket", `[` + user},
		{"garbage between messages", `[` + user + `, oops, ` + user + `]`},
		{"trailing data after array", `[` + user + `] oops`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeCodebuffMessages(
				strings.NewReader(tc.transcript), sessionDate, "codebuff:p:s")
			require.Error(t, err)
		})
	}
}

func TestDecodeCodebuffMessagesReaderError(t *testing.T) {
	sessionDate := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	boom := errors.New("disk exploded")
	r := io.MultiReader(
		strings.NewReader(`[{"id":"u1","variant":"user","content":"hello"},`),
		&errReader{err: boom},
	)
	_, err := decodeCodebuffMessages(r, sessionDate, "codebuff:p:s")
	require.ErrorIs(t, err, boom)
}

type errReader struct{ err error }

func (r *errReader) Read([]byte) (int, error) { return 0, r.err }
