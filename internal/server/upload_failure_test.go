package server_test

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/wesm/agentsview/internal/testjsonl"
)

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
