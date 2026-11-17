package remotesync

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// countingSource records the encoded size of each archive it serves.
type countingSource struct {
	dirSource
	wire *[]int
}

func (s countingSource) Archive(ctx context.Context, request ArchiveRequest) (*ArchiveStream, error) {
	stream, err := s.dirSource.Archive(ctx, request)
	if err != nil {
		return nil, err
	}
	body, err := io.ReadAll(stream.Body)
	if err != nil {
		return nil, err
	}
	*s.wire = append(*s.wire, len(body))
	stream.Body = io.NopCloser(bytes.NewReader(body))
	return stream, nil
}

func claudeTranscript(messages int, text string) string {
	builder := testjsonl.NewSessionBuilder()
	for i := range messages {
		builder.AddClaudeUser(
			time.Date(2024, 1, 1, 0, 0, i, 0, time.UTC).Format(time.RFC3339),
			text+strings.Repeat(" padding", 40),
		)
	}
	return builder.String()
}

// A growing transcript is sent as only its new bytes, and the mirror ends
// up byte-for-byte equal to the remote copy whichever way the server answers.
func TestRemoteSyncAppendsGrowingTranscripts(t *testing.T) {
	tests := []struct {
		name          string
		ignoreAppends bool
		second        func(first string) string
		messages      int
		wantSmall     bool
	}{
		{
			name:      "appended transcript",
			second:    func(first string) string { return first + claudeTranscript(1, "later") },
			messages:  301,
			wantSmall: true,
		},
		{
			name: "rewritten transcript",
			second: func(first string) string {
				return claudeTranscript(400, "rewritten")
			},
			messages: 400,
		},
		{
			name:          "server without appends",
			ignoreAppends: true,
			second:        func(first string) string { return first + claudeTranscript(1, "later") },
			messages:      301,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			remoteDir := filepath.Join(t.TempDir(), "claude-projects")
			session := filepath.Join(remoteDir, "project-a", "session.jsonl")
			require.NoError(t, os.MkdirAll(filepath.Dir(session), 0o755))
			first := claudeTranscript(300, "first")
			require.NoError(t, os.WriteFile(session, []byte(first), 0o644))
			// An unchanged file keeps the second sync a delta request: when
			// every file changed, the sync asks for a full archive instead.
			require.NoError(t, os.WriteFile(
				filepath.Join(remoteDir, "project-a", "other.jsonl"),
				[]byte(claudeTranscript(1, "other")), 0o644,
			))
			database, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "test.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, database.Close()) })
			var wire []int
			sync := RemoteSync{
				Host: "devbox", DataDir: t.TempDir(), DB: database,
				Source: countingSource{
					dirSource: dirSource{
						ignoreAppends: tc.ignoreAppends,
						targets: TargetSet{Dirs: map[parser.AgentType][]string{
							parser.AgentClaude: {remoteDir},
						}},
					},
					wire: &wire,
				},
			}
			_, err = sync.Run(t.Context())
			require.NoError(t, err)

			second := tc.second(first)
			require.NoError(t, os.WriteFile(session, []byte(second), 0o644))
			later := time.Now().Add(time.Minute)
			require.NoError(t, os.Chtimes(session, later, later))
			_, err = sync.Run(t.Context())
			require.NoError(t, err)

			mirrored, err := os.ReadFile(remappedRemotePath(
				MirrorDir(sync.DataDir, sync.Host), session,
			))
			require.NoError(t, err)
			assert.Equal(t, second, string(mirrored))
			info, err := os.Stat(remappedRemotePath(
				MirrorDir(sync.DataDir, sync.Host), session,
			))
			require.NoError(t, err)
			assert.Equal(t, later.UnixNano(), info.ModTime().UnixNano())
			messages, err := database.GetAllMessages(t.Context(), "devbox~session")
			require.NoError(t, err)
			assert.Len(t, messages, tc.messages)
			require.Len(t, wire, 2)
			if tc.wantSmall {
				assert.Less(t, wire[1], len(second)/10,
					"only the appended bytes cross the wire")
			} else {
				assert.Greater(t, wire[1], len(second)/2)
			}
		})
	}
}

// A database grows at its end but also rewrites earlier bytes, such as the
// header SQLite updates on every commit. Its old tail can survive that, so a
// tail match must not let it skip the bytes that changed.
func TestArchiveAppendSendsOnlyTranscriptsAsAppends(t *testing.T) {
	for _, tc := range []struct {
		name   string
		append bool
	}{
		{name: "session.jsonl", append: true},
		{name: "state.vscdb"},
		{name: "store.db"},
		{name: "session.json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), tc.name)
			// Longer than the tail the append check compares, so a change
			// at the start leaves that tail intact.
			old := bytes.Repeat([]byte("page "), 2*appendTailBytes/5)
			require.NoError(t, os.WriteFile(path, old, 0o644))
			base, ok := mirrorAppendBase(path)
			require.True(t, ok)
			grown := append([]byte("HEAD!"), old[5:]...)
			grown = append(grown, "new page"...)
			require.NoError(t, os.WriteFile(path, grown, 0o644))

			var archive bytes.Buffer
			tw := tar.NewWriter(&archive)
			appended, err := writeArchiveAppend(tw, path, base)
			require.NoError(t, err)
			require.NoError(t, tw.Close())

			assert.Equal(t, tc.append, appended)
			_, err = tar.NewReader(&archive).Next()
			if tc.append {
				assert.NoError(t, err, "a transcript gets an append entry")
			} else {
				assert.ErrorIs(t, err, io.EOF,
					"a file sent whole is left to the caller, so no entry is written")
			}
		})
	}
}

// An appended entry cut off mid-body leaves the mirror copy at its old
// length, so the next comparison fetches the file again.
func TestExtractTruncatedAppendRestoresMirrorCopy(t *testing.T) {
	dst := t.TempDir()
	remote := "/remote/claude/projects/p/session.jsonl"
	local := remappedRemotePath(dst, remote)
	require.NoError(t, os.MkdirAll(filepath.Dir(local), 0o755))
	const old = "old line\n"
	require.NoError(t, os.WriteFile(local, []byte(old), 0o644))
	name, err := safeRemotePathArchiveName(remote)
	require.NoError(t, err)

	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: 1000,
		Format:     tar.FormatPAX,
		PAXRecords: map[string]string{appendOffsetPAXKey: "9"},
	}))
	_, err = tw.Write([]byte(strings.Repeat("x", 100)))
	require.NoError(t, err)

	_, err = ExtractTarStream(t.Context(), bytes.NewReader(archive.Bytes()), dst)
	require.Error(t, err)
	content, err := os.ReadFile(local)
	require.NoError(t, err)
	assert.Equal(t, old, string(content))
}
