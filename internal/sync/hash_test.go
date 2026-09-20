package sync

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func createTempFile(t *testing.T, content []byte) string {
	t.Helper()
	cleanName := strings.ReplaceAll(t.Name(), "/", "_")
	path := filepath.Join(t.TempDir(), cleanName+".txt")
	require.NoError(t, os.WriteFile(path, content, 0o644))
	return path
}

func TestComputeHash(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "hello world",
			input: "hello world\n",
			want:  helloWorldHash,
		},
		{
			name:  "empty input",
			input: "",
			want:  emptyInputHash,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ComputeHash(strings.NewReader(tt.input))
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestComputeFileHash(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(t *testing.T) string
		want    string
		wantErr bool
	}{
		{
			name: "hello world",
			setup: func(t *testing.T) string {
				t.Helper()

				return createTempFile(t, []byte("hello world\n"))
			},
			want: helloWorldHash,
		},
		{
			name: "empty file",
			setup: func(t *testing.T) string {
				t.Helper()

				return createTempFile(t, []byte(""))
			},
			want: emptyInputHash,
		},
		{
			name: "missing file",
			setup: func(t *testing.T) string {
				t.Helper()

				return filepath.Join(t.TempDir(), "nonexistent.txt")
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := tt.setup(t)

			got, err := ComputeFileHash(path)
			if tt.wantErr {
				require.Error(t, err)
				requirePathError(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestComputeHash_ReaderError(t *testing.T) {
	errInjected := errors.New("injected error")
	reader := &failingReader{err: errInjected}
	_, err := ComputeHash(reader)
	require.Error(t, err)
	require.ErrorIs(t, err, errInjected)
}

func TestComputeFileHash_ReadError(t *testing.T) {
	// Use a directory to simulate a read error after open
	dir := t.TempDir()
	_, err := ComputeFileHash(dir)
	require.Error(t, err)
	// On most systems, reading a directory fails.
	requirePathError(t, err)
}
