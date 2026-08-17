package server_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/server"
	"go.kenn.io/agentsview/internal/testjsonl"
)

type uploadCommitFailStore struct {
	db.Store
	err error
}

func (s uploadCommitFailStore) ReadOnly() bool { return false }

func (s uploadCommitFailStore) WriteSessionBatchAtomic(
	_ []db.SessionBatchWrite,
	beforeCommit ...func() error,
) (db.SessionBatchResult, error) {
	if len(beforeCommit) > 0 && beforeCommit[0] != nil {
		if err := beforeCommit[0](); err != nil {
			return db.SessionBatchResult{}, err
		}
	}
	return db.SessionBatchResult{}, s.err
}

func TestUploadSession_SaveFailure(t *testing.T) {
	te := setup(t)

	// Create a file where the project directory should be
	// to force os.MkdirAll to fail
	projectName := "failproj"
	projectPath := filepath.Join(te.dataDir, "uploads", projectName)
	require.NoError(t, os.MkdirAll(filepath.Dir(projectPath), 0o755))
	require.NoError(t, os.WriteFile(projectPath, nil, 0o644))

	w := te.upload(t, "test.jsonl", "{}", "project="+projectName)
	assertStatus(t, w, http.StatusInternalServerError)
	assertErrorResponse(t, w, "failed to save upload")
}

func TestUploadSession_DBFailure(t *testing.T) {
	te := setup(t)

	// Close DB to force saveSessionToDB to fail
	te.db.Close()

	content := `{"type":"user","timestamp":"2024-01-01T10:00:00Z","message":{"content":"Hello"}}`
	w := te.upload(t, "test.jsonl", content, "project=myproj")
	assertStatus(t, w, http.StatusInternalServerError)
	assertErrorResponse(t, w, "failed to save session to database")
}

func TestUploadSession_CommitFailureDoesNotWriteDB(t *testing.T) {
	te := setup(t)

	project := "myproj"
	filename := "rename-fail.jsonl"
	finalPath := filepath.Join(
		te.dataDir, "uploads", project, filename,
	)
	require.NoError(t, os.MkdirAll(finalPath, 0o755))

	content := testjsonl.NewSessionBuilder().
		AddClaudeUser("2024-01-01T10:00:00Z", "hello").
		AddClaudeAssistant("2024-01-01T10:00:05Z", "hi").
		String()
	w := te.upload(t, filename, content, "project="+project)
	assertStatus(t, w, http.StatusInternalServerError)
	assertErrorResponse(t, w, "failed to save upload")

	sess, err := te.db.GetSessionFull(
		context.Background(), "rename-fail",
	)
	require.NoError(t, err)
	assert.Nil(t, sess, "session persisted despite upload commit failure")
}

func TestUploadSession_DBCommitFailureAfterFileCommitRollsBackUpload(
	t *testing.T,
) {
	dir := t.TempDir()
	const (
		project  = "myproj"
		filename = "commit-fails-after-rename.jsonl"
	)
	finalPath := filepath.Join(dir, "uploads", project, filename)

	srv := server.New(config.Config{
		Host:         "127.0.0.1",
		Port:         0,
		DataDir:      dir,
		WriteTimeout: 30 * time.Second,
	}, uploadCommitFailStore{err: errors.New("commit failed")}, nil)

	content := testjsonl.NewSessionBuilder().
		AddClaudeUser("2024-01-01T10:00:00Z", "hello").
		AddClaudeAssistant("2024-01-01T10:00:05Z", "hi").
		String()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", filename)
	require.NoError(t, err)
	_, err = fw.Write([]byte(content))
	require.NoError(t, err)
	require.NoError(t, mw.Close())

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/sessions/upload?project="+project+"&machine=remote", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Origin", "http://127.0.0.1:0")
	req.Host = "127.0.0.1:0"
	req.RemoteAddr = "127.0.0.1:1234"
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	assertStatus(t, w, http.StatusInternalServerError)
	assertErrorResponse(t, w, "failed to save session to database")
	_, statErr := os.Stat(finalPath)
	require.ErrorIs(t, statErr, os.ErrNotExist, "upload file exists after DB commit failure")
}

func TestUploadSession_DBCommitFailureAfterReplacingFileRestoresPrevious(
	t *testing.T,
) {
	dir := t.TempDir()
	const (
		project  = "myproj"
		filename = "replace-then-commit-fails.jsonl"
	)
	finalPath := filepath.Join(dir, "uploads", project, filename)
	require.NoError(t, os.MkdirAll(filepath.Dir(finalPath), 0o755))
	previous := []byte("previous file contents")
	require.NoError(t, os.WriteFile(finalPath, previous, 0o644))

	srv := server.New(config.Config{
		Host:         "127.0.0.1",
		Port:         0,
		DataDir:      dir,
		WriteTimeout: 30 * time.Second,
	}, uploadCommitFailStore{err: errors.New("commit failed")}, nil)

	content := testjsonl.NewSessionBuilder().
		AddClaudeUser("2024-01-01T10:00:00Z", "hello").
		AddClaudeAssistant("2024-01-01T10:00:05Z", "hi").
		String()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", filename)
	require.NoError(t, err)
	_, err = fw.Write([]byte(content))
	require.NoError(t, err)
	require.NoError(t, mw.Close())

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/sessions/upload?project="+project+"&machine=remote", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Origin", "http://127.0.0.1:0")
	req.Host = "127.0.0.1:0"
	req.RemoteAddr = "127.0.0.1:1234"
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	assertStatus(t, w, http.StatusInternalServerError)
	assertErrorResponse(t, w, "failed to save session to database")
	got, err := os.ReadFile(finalPath)
	require.NoError(t, err)
	assert.Equal(t, previous, got, "restored file differs")
}

func TestUploadSession_RetargetedCommitFailureRestoresPrevious(t *testing.T) {
	dir := t.TempDir()
	const (
		project  = "myproj"
		filename = "transport.jsonl"
		embedded = "embedded-target"
	)
	finalPath := filepath.Join(dir, "uploads", project, embedded+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(finalPath), 0o755))
	previous := []byte("previous target bytes")
	require.NoError(t, os.WriteFile(finalPath, previous, 0o644))
	srv := server.New(config.Config{
		Host: "127.0.0.1", Port: 0, DataDir: dir, WriteTimeout: 30 * time.Second,
	}, uploadCommitFailStore{err: errors.New("commit failed")}, nil)
	content := fmt.Sprintf(
		`{"type":"user","uuid":"u1","timestamp":"2026-01-01T00:00:00Z","sessionId":"%s","isSidechain":false,"message":{"content":"hello"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","timestamp":"2026-01-01T00:00:01Z","sessionId":"%s","isSidechain":false,"message":{"content":[{"type":"text","text":"hi"}]}}`+"\n", embedded, embedded)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", filename)
	require.NoError(t, err)
	_, err = fw.Write([]byte(content))
	require.NoError(t, err)
	require.NoError(t, mw.Close())
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/upload?project="+project, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Origin", "http://127.0.0.1:0")
	req.Host = "127.0.0.1:0"
	req.RemoteAddr = "127.0.0.1:1234"
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	assertStatus(t, w, http.StatusInternalServerError)
	got, err := os.ReadFile(finalPath)
	require.NoError(t, err)
	assert.Equal(t, previous, got)
}

func TestUploadSession_ExplicitIdentityRetargetsCollision(t *testing.T) {
	te := setup(t)
	transcript := func(id, text string) string {
		return fmt.Sprintf(
			`{"type":"user","uuid":"u1","timestamp":"2026-01-01T00:00:00Z","sessionId":%q,"isSidechain":false,"message":{"content":%q}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","timestamp":"2026-01-01T00:00:01Z","sessionId":%q,"isSidechain":false,"message":{"content":[{"type":"text","text":"answer"}]}}`+"\n",
			id, text, id,
		)
	}
	oldContent := transcript("archived-old", "old")
	w := te.upload(t, "collision.jsonl", oldContent, "project=myproj&machine=remote")
	assertStatus(t, w, http.StatusOK)
	oldBefore, err := te.db.GetSessionFull(context.Background(), "archived-old")
	require.NoError(t, err)
	require.NotNil(t, oldBefore)
	newContent := transcript("archived-new", "new")
	w = te.upload(t, "collision.jsonl", newContent, "project=myproj&machine=remote")
	assertStatus(t, w, http.StatusOK)
	response := decode[struct {
		SessionID string `json:"session_id"`
	}](t, w)
	assert.Equal(t, "archived-new", response.SessionID)
	oldPath := filepath.Join(te.dataDir, "uploads", "myproj", "archived-old.jsonl")
	newPath := filepath.Join(te.dataDir, "uploads", "myproj", "archived-new.jsonl")
	gotOld, err := os.ReadFile(oldPath)
	require.NoError(t, err)
	assert.Equal(t, []byte(oldContent), gotOld)
	gotNew, err := os.ReadFile(newPath)
	require.NoError(t, err)
	assert.Equal(t, []byte(newContent), gotNew)
	old, err := te.db.GetSessionFull(context.Background(), "archived-old")
	require.NoError(t, err)
	require.NotNil(t, old)
	assert.Equal(t, oldBefore, old)
	newSession, err := te.db.GetSessionFull(context.Background(), "archived-new")
	require.NoError(t, err)
	require.NotNil(t, newSession)
	require.NotNil(t, newSession.FilePath)
	assert.Equal(t, newPath, *newSession.FilePath)
}
