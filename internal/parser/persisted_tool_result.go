package parser

import (
	"io"

	"go.kenn.io/agentsview/internal/stringutil"
)

func readPersistedToolResultBytes(reader io.Reader) ([]byte, error) {
	return io.ReadAll(io.LimitReader(reader, maxPersistedToolResultSize+1))
}

func persistedToolResultPrefix(b []byte) string {
	if len(b) > maxPersistedToolResultSize {
		return stringutil.SafeTruncate(string(b), maxPersistedToolResultSize)
	}
	return string(b)
}

func persistedToolResultText(b []byte) string {
	if len(b) > maxPersistedToolResultSize {
		return persistedToolResultPrefix(b) +
			"\n\n[agentsview: persisted tool result truncated at 16 MiB]"
	}
	return string(b)
}
