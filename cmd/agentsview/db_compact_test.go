package main

import (
	"bytes"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.kenn.io/agentsview/internal/db"

	"github.com/stretchr/testify/require"
)

func TestDecideCompactRoute(t *testing.T) {
	tests := []struct {
		name       string
		tr         transport
		stagingDir string
		delegate   bool
		wantErr    string
	}{
		{
			name:     "writable daemon delegates",
			tr:       transport{Mode: transportHTTP},
			delegate: true,
		},
		{
			name:       "writable daemon rejects staging dir",
			tr:         transport{Mode: transportHTTP},
			stagingDir: "/tmp/staging",
			wantErr:    "--staging-dir requires direct archive access",
		},
		{
			name: "read-only pg or duckdb server compacts directly",
			tr:   transport{Mode: transportHTTP, ReadOnly: true},
		},
		{
			name: "read-only server allows staging dir",
			tr:   transport{Mode: transportHTTP, ReadOnly: true},
			// A read-only server does not own the SQLite archive, so the
			// direct-only staging flag stays usable without stopping it.
			stagingDir: "/tmp/staging",
		},
		{
			name:    "unreachable writable daemon refuses direct",
			tr:      transport{Mode: transportDirect, DirectReadOnly: true},
			wantErr: "cannot compact directly",
		},
		{
			name: "no daemon compacts directly",
			tr:   transport{Mode: transportDirect},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			delegate, err := decideCompactRoute(tt.tr, tt.stagingDir)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.delegate, delegate)
		})
	}
}

func TestDBCompactJSONRequiresYes(t *testing.T) {
	cmd := newDBCompactCommand()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--format", "json"})

	err := cmd.Execute()
	require.EqualError(t, err, "--format json requires --yes for db compact")
	require.Empty(t, stdout.String(), "JSON mode must not mix a prompt into stdout")
}

func TestDBCompactResultJSONNewline(t *testing.T) {
	var output bytes.Buffer
	require.NoError(t, writeDBCompactResult(&output, db.CompactResult{}, true))
	var result db.CompactResult
	require.NoError(t, json.Unmarshal(output.Bytes(), &result))
	require.Equal(t, byte('\n'), output.Bytes()[output.Len()-1])
}

func TestRequestDBCompactRejectsEmptySuccess(t *testing.T) {
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer endpoint.Close()

	_, err := requestDBCompact(t.Context(), transport{URL: endpoint.URL}, "", db.CompactOptions{})

	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
}
