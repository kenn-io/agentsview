package server_test

import (
	"bytes"
	"context"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wesm/agentsview/internal/config"
	"github.com/wesm/agentsview/internal/db"
	"github.com/wesm/agentsview/internal/server"
	"github.com/wesm/agentsview/internal/testjsonl"
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
	if err := os.MkdirAll(filepath.Dir(projectPath), 0o755); err != nil {
		t.Fatalf("creating uploads dir: %v", err)
	}
	if err := os.WriteFile(projectPath, nil, 0o644); err != nil {
		t.Fatalf("creating conflict file: %v", err)
	}

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
	if err := os.MkdirAll(finalPath, 0o755); err != nil {
		t.Fatalf("creating final path directory: %v", err)
	}

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
	if err != nil {
		t.Fatalf("GetSessionFull: %v", err)
	}
	if sess != nil {
		t.Fatalf("session persisted despite upload commit failure: %+v", sess)
	}
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
	if err != nil {
		t.Fatalf("creating form file: %v", err)
	}
	if _, err := fw.Write([]byte(content)); err != nil {
		t.Fatalf("writing form file: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("closing multipart writer: %v", err)
	}

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
	if _, err := os.Stat(finalPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("upload file exists after DB commit failure: %v", err)
	}
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
	if err := os.MkdirAll(filepath.Dir(finalPath), 0o755); err != nil {
		t.Fatalf("creating upload dir: %v", err)
	}
	previous := []byte("previous file contents")
	if err := os.WriteFile(finalPath, previous, 0o644); err != nil {
		t.Fatalf("writing previous upload: %v", err)
	}

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
	if err != nil {
		t.Fatalf("creating form file: %v", err)
	}
	if _, err := fw.Write([]byte(content)); err != nil {
		t.Fatalf("writing form file: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("closing multipart writer: %v", err)
	}

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
	if err != nil {
		t.Fatalf("reading restored upload: %v", err)
	}
	if !bytes.Equal(got, previous) {
		t.Fatalf("restored file = %q, want %q", got, previous)
	}
}
