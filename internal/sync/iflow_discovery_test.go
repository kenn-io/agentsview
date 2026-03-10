package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/wesm/agentsview/internal/parser"
)

func TestDiscoverIflowProjects(t *testing.T) {
	// Create a temporary directory structure for testing
	tmpDir := t.TempDir()

	// Create project directories
	proj1 := filepath.Join(tmpDir, "project1")
	proj2 := filepath.Join(tmpDir, "project2")

	if err := os.MkdirAll(proj1, 0o755); err != nil {
		t.Fatalf("failed to create project1 directory: %v", err)
	}
	if err := os.MkdirAll(proj2, 0o755); err != nil {
		t.Fatalf("failed to create project2 directory: %v", err)
	}

	// Create session files in project1
	session1 := filepath.Join(proj1, "session-abc123.jsonl")
	session2 := filepath.Join(proj1, "session-def456.jsonl")

	if err := os.WriteFile(session1, []byte(`{"test":"data"}`), 0o644); err != nil {
		t.Fatalf("failed to create session1: %v", err)
	}
	if err := os.WriteFile(session2, []byte(`{"test":"data"}`), 0o644); err != nil {
		t.Fatalf("failed to create session2: %v", err)
	}

	// Create a session file in project2
	session3 := filepath.Join(proj2, "session-ghi789.jsonl")
	if err := os.WriteFile(session3, []byte(`{"test":"data"}`), 0o644); err != nil {
		t.Fatalf("failed to create session3: %v", err)
	}

	// Create a non-session file (should be ignored)
	otherFile := filepath.Join(proj1, "other.txt")
	if err := os.WriteFile(otherFile, []byte(`not a session`), 0o644); err != nil {
		t.Fatalf("failed to create other file: %v", err)
	}

	// Create a directory (should be ignored)
	subDir := filepath.Join(proj1, "subdir")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatalf("failed to create subdir: %v", err)
	}

	// Run discovery
	files := parser.DiscoverIflowProjects(tmpDir)

	// Verify results
	if len(files) != 3 {
		t.Errorf("expected 3 files, got %d", len(files))
	}

	// Verify file paths
	paths := make(map[string]bool)
	for _, f := range files {
		paths[f.Path] = true
	}

	if !paths[session1] {
		t.Errorf("session1 not found in results")
	}
	if !paths[session2] {
		t.Errorf("session2 not found in results")
	}
	if !paths[session3] {
		t.Errorf("session3 not found in results")
	}
	if paths[otherFile] {
		t.Errorf("other.txt should not be in results")
	}

	// Verify project names
	projects := make(map[string]bool)
	for _, f := range files {
		projects[f.Project] = true
	}

	if !projects["project1"] {
		t.Errorf("project1 not found in projects")
	}
	if !projects["project2"] {
		t.Errorf("project2 not found in projects")
	}

	// Verify agent type
	for _, f := range files {
		if f.Agent != "iflow" {
			t.Errorf("expected agent 'iflow', got '%s'", f.Agent)
		}
	}
}

func TestFindIflowSourceFile(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a project directory
	proj := filepath.Join(tmpDir, "test-project")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatalf("failed to create project directory: %v", err)
	}

	// Create a session file
	sessionID := "abc123-def456"
	sessionFile := filepath.Join(proj, "session-"+sessionID+".jsonl")
	if err := os.WriteFile(sessionFile, []byte(`{"test":"data"}`), 0o644); err != nil {
		t.Fatalf("failed to create session file: %v", err)
	}

	// Test finding the file
	found := parser.FindIflowSourceFile(tmpDir, sessionID)
	if found != sessionFile {
		t.Errorf("expected to find %s, got %s", sessionFile, found)
	}

	// Test finding a non-existent file
	notFound := parser.FindIflowSourceFile(tmpDir, "nonexistent")
	if notFound != "" {
		t.Errorf("expected empty string for non-existent file, got %s", notFound)
	}

	// Test finding a fork ID (should extract base session ID)
	// Fork IDs have format: <baseUUID>-<childUUID>
	// The file lookup should use only the base UUID
	baseSessionID := "96e6d875-92eb-40b9-b193-a9ba99f0f709"
	forkSessionID := baseSessionID + "-12345678-1234-5678-9abc-def012345678"
	forkSessionFile := filepath.Join(proj, "session-"+baseSessionID+".jsonl")
	if err := os.WriteFile(forkSessionFile, []byte(`{"test":"fork"}`), 0o644); err != nil {
		t.Fatalf("failed to create fork session file: %v", err)
	}

	// Test finding the fork session - should find the base file
	foundFork := parser.FindIflowSourceFile(tmpDir, forkSessionID)
	if foundFork != forkSessionFile {
		t.Errorf("expected to find %s for fork ID %s, got %s", forkSessionFile, forkSessionID, foundFork)
	}
}