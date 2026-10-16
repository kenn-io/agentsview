package sshremote

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/remotesync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// serveTestConfig returns a config whose only Claude directory is a fresh
// temp dir, and that directory.
func serveTestConfig(t *testing.T) (config.Config, string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	claudeDir := filepath.Join(root, "claude-projects")
	require.NoError(t, os.MkdirAll(claudeDir, 0o755))
	cfg := config.Config{AgentDirs: map[parser.AgentType][]string{
		parser.AgentClaude: {claudeDir},
	}}
	return cfg, claudeDir
}

// writeServeTestSession writes one Claude session under dir and returns its
// path.
func writeServeTestSession(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "project-a", "session.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(
		testjsonl.NewSessionBuilder().
			AddClaudeUser("2024-01-01T00:00:00Z", "serve test").
			String(),
	), 0o644))
	return path
}

func encodeServeRequest(t *testing.T, request serveRequest) *bytes.Buffer {
	t.Helper()
	var in bytes.Buffer
	require.NoError(t, json.MarshalWrite(&in, request))
	return &in
}

func decodeServeResponse(t *testing.T, out io.Reader) []byte {
	t.Helper()
	zr, err := remotesync.NewZstdReader(out)
	require.NoError(t, err)
	defer zr.Close()
	body, err := io.ReadAll(zr)
	require.NoError(t, err)
	return body
}

func TestServe(t *testing.T) {
	cfg, claudeDir := serveTestConfig(t)
	writeServeTestSession(t, claudeDir)
	allowed := remotesync.TargetSet{
		Dirs: map[parser.AgentType][]string{parser.AgentClaude: {claudeDir}},
	}
	outside := remotesync.TargetSet{
		Dirs: map[parser.AgentType][]string{parser.AgentClaude: {t.TempDir()}},
	}
	version := remotesync.ProtocolVersion
	archiveOf := func(targets remotesync.TargetSet) *remotesync.ArchiveRequest {
		return &remotesync.ArchiveRequest{TargetSet: targets}
	}

	tests := []struct {
		name     string
		op       Op
		request  serveRequest
		wantCode ErrorCode // empty means success
	}{
		{
			"wrong protocol", OpTargets,
			serveRequest{ProtocolVersion: version + 1},
			CodeIncompatibleProtocol,
		},
		{"manifest", OpManifest, serveRequest{ProtocolVersion: version, Targets: &allowed}, ""},
		{
			"manifest outside roots", OpManifest,
			serveRequest{ProtocolVersion: version, Targets: &outside},
			CodeForbidden,
		},
		{
			"manifest without targets", OpManifest,
			serveRequest{ProtocolVersion: version},
			CodeInternal,
		},
		{
			"archive outside roots", OpArchive,
			serveRequest{ProtocolVersion: version, Archive: archiveOf(outside)},
			CodeForbidden,
		},
		{"archive without body", OpArchive, serveRequest{ProtocolVersion: version}, CodeInternal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := encodeServeRequest(t, tt.request)
			var out bytes.Buffer

			err := Serve(t.Context(), tt.op, cfg, in, &out)

			if tt.wantCode != "" {
				serveErr, ok := errors.AsType[*ServeError](err)
				require.True(t, ok, "error %v is not a ServeError", err)
				assert.Equal(t, tt.wantCode, serveErr.Code)
				return
			}
			require.NoError(t, err)
			assert.NotEmpty(t, decodeServeResponse(t, &out))
		})
	}
}

func TestServeStopsReadingTheRequestWhenCancelled(t *testing.T) {
	cfg, _ := serveTestConfig(t)
	// A collector that stalls mid-request keeps stdin open. A signal must
	// still end the remote process instead of waiting for the rest.
	in, writer := io.Pipe()
	t.Cleanup(func() { _ = writer.Close() })
	go func() { _, _ = writer.Write([]byte(`{"protocol_version":`)) }()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, OpTargets, cfg, in, io.Discard) }()

	cancel()

	select {
	case err := <-done:
		serveErr, ok := errors.AsType[*ServeError](err)
		require.True(t, ok, "error %v is not a ServeError", err)
		assert.Equal(t, CodeInternal, serveErr.Code)
		assert.Contains(t, serveErr.Message, "interrupted")
	case <-time.After(60 * time.Second): // hang guard; the event is Serve returning
		require.FailNow(t, "Serve kept reading the request after cancellation")
	}
}

func TestServeRefusesAnArchiveOnlyDatabase(t *testing.T) {
	cfg, claudeDir := serveTestConfig(t)
	writeServeTestSession(t, claudeDir)
	// An archive-only database holds another machine's sessions. HTTP refuses
	// to publish from it, and so must SSH.
	cfg.DBPath = filepath.Join(t.TempDir(), "sessions.db")
	archive, err := db.Open(t.Context(), cfg.DBPath)
	require.NoError(t, err)
	require.NoError(t, archive.EnableArchiveOnly(t.Context()))
	require.NoError(t, archive.Close())
	allowed := remotesync.TargetSet{
		Dirs: map[parser.AgentType][]string{parser.AgentClaude: {claudeDir}},
	}

	for _, tt := range []struct {
		op      Op
		request serveRequest
	}{
		{OpTargets, serveRequest{}},
		{OpManifest, serveRequest{Targets: &allowed}},
		{OpArchive, serveRequest{Archive: &remotesync.ArchiveRequest{TargetSet: allowed}}},
	} {
		t.Run(string(tt.op), func(t *testing.T) {
			tt.request.ProtocolVersion = remotesync.ProtocolVersion
			var out bytes.Buffer

			err := Serve(t.Context(), tt.op, cfg, encodeServeRequest(t, tt.request), &out)

			serveErr, ok := errors.AsType[*ServeError](err)
			require.True(t, ok, "error %v is not a ServeError", err)
			assert.Equal(t, CodeForbidden, serveErr.Code)
			assert.Contains(t, serveErr.Message, db.ErrArchiveOnly.Error())
			assert.Zero(t, out.Len(), "nothing is published")
		})
	}
}

func TestServeWithoutADatabaseCreatesNone(t *testing.T) {
	cfg, claudeDir := serveTestConfig(t)
	writeServeTestSession(t, claudeDir)
	cfg.DBPath = filepath.Join(t.TempDir(), "sessions.db")
	var out bytes.Buffer

	err := Serve(t.Context(), OpTargets,
		cfg, encodeServeRequest(t, serveRequest{ProtocolVersion: remotesync.ProtocolVersion}), &out)

	require.NoError(t, err)
	assert.NoFileExists(t, cfg.DBPath)
}

func TestServeRejectsMalformedRequest(t *testing.T) {
	cfg, _ := serveTestConfig(t)
	var out bytes.Buffer

	err := Serve(t.Context(), OpTargets, cfg, strings.NewReader("{not json"), &out)

	serveErr, ok := errors.AsType[*ServeError](err)
	require.True(t, ok, "error %v is not a ServeError", err)
	assert.Equal(t, CodeInternal, serveErr.Code)
	assert.Empty(t, out.Bytes(), "a refused request must not start a response")
}

func TestServeTargetsReturnsResolvedTargets(t *testing.T) {
	cfg, claudeDir := serveTestConfig(t)
	writeServeTestSession(t, claudeDir)
	in := encodeServeRequest(t, serveRequest{ProtocolVersion: remotesync.ProtocolVersion})
	var out bytes.Buffer

	require.NoError(t, Serve(t.Context(), OpTargets, cfg, in, &out))

	var targets remotesync.TargetSet
	require.NoError(t, json.Unmarshal(decodeServeResponse(t, &out), &targets))
	assert.Equal(t, []string{claudeDir}, targets.Dirs[parser.AgentClaude])
}

func TestServeArchiveStreamsSessionTar(t *testing.T) {
	cfg, claudeDir := serveTestConfig(t)
	sessionPath := writeServeTestSession(t, claudeDir)
	in := encodeServeRequest(t, serveRequest{
		ProtocolVersion: remotesync.ProtocolVersion,
		Archive: &remotesync.ArchiveRequest{
			Dirs: map[parser.AgentType][]string{parser.AgentClaude: {claudeDir}},
		},
	})
	var out bytes.Buffer

	require.NoError(t, Serve(t.Context(), OpArchive, cfg, in, &out))

	tr := tar.NewReader(bytes.NewReader(decodeServeResponse(t, &out)))
	var names []string
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		names = append(names, header.Name)
	}
	// Entry names are absolute remote paths in tar form: no leading slash on
	// Unix, a __drive_X prefix on Windows. The part under claudeDir is the
	// same on both.
	rel, err := filepath.Rel(filepath.Dir(claudeDir), sessionPath)
	require.NoError(t, err)
	want := "/" + filepath.ToSlash(rel)
	assert.True(t, slices.ContainsFunc(names, func(name string) bool {
		return strings.HasSuffix(name, want)
	}), "archive %v has no entry ending in %s", names, want)
}

func TestClassifyServeError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want ErrorCode
	}{
		{
			"target not allowed",
			fmt.Errorf("plan: %w", remotesync.ErrTargetNotAllowed), CodeForbidden,
		},
		{"file not allowed", fmt.Errorf("plan: %w", remotesync.ErrFileNotAllowed), CodeForbidden},
		{
			"manifest unsupported",
			fmt.Errorf("manifest: %w", remotesync.ErrManifestUnsupported), CodeUnsupported,
		},
		{"other failure", errors.New("disk full"), CodeInternal},
		{
			"serve error kept",
			&ServeError{Code: CodeIncompatibleProtocol, Message: "old"}, CodeIncompatibleProtocol,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, classifyServeError(tt.err).Code)
		})
	}
}
