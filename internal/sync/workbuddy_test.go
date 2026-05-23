package sync

import (
	"os"
	"path/filepath"
	"testing"

	"go.kenn.io/agentsview/internal/parser"
)

func TestWorkBuddyRegistryUsesRecursiveWatch(t *testing.T) {
	def, ok := parser.AgentByType(parser.AgentWorkBuddy)
	if !ok {
		t.Fatal("AgentWorkBuddy missing from Registry")
	}
	if def.ShallowWatch {
		t.Fatal("WorkBuddy should use recursive watch for nested sessions")
	}
}

func TestEngineClassifyWorkBuddyPaths(t *testing.T) {
	db := openTestDB(t)
	root := t.TempDir()
	engine := NewEngine(db, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentWorkBuddy: {root},
		},
		Machine: "local",
	})

	mainPath := filepath.Join(root, "proj", "11111111-1111-4111-8111-111111111111.jsonl")
	subPath := filepath.Join(root, "proj", "11111111-1111-4111-8111-111111111111", "subagents", "agent-123.jsonl")
	toolPath := filepath.Join(root, "proj", "11111111-1111-4111-8111-111111111111", "tool-results", "tool_123.txt")
	for _, path := range []string{mainPath, subPath, toolPath} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("MkdirAll(%q): %v", path, err)
		}
		if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
			t.Fatalf("WriteFile(%q): %v", path, err)
		}
	}

	got, ok := engine.classifyOnePath(mainPath, nil)
	if !ok {
		t.Fatal("main path did not classify")
	}
	if got.Path != mainPath || got.Project != "proj" || got.Agent != parser.AgentWorkBuddy {
		t.Fatalf("main classified as %+v", got)
	}

	got, ok = engine.classifyOnePath(subPath, nil)
	if !ok {
		t.Fatal("subagent path did not classify")
	}
	if got.Path != subPath || got.Project != "proj" || got.Agent != parser.AgentWorkBuddy {
		t.Fatalf("subagent classified as %+v", got)
	}

	if got, ok = engine.classifyOnePath(toolPath, nil); ok {
		t.Fatalf("tool result classified as %+v", got)
	}
}

func TestEngineClassifyWorkBuddyProjectNamedSubagentsAsMainSession(t *testing.T) {
	db := openTestDB(t)
	root := t.TempDir()
	engine := NewEngine(db, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentWorkBuddy: {root},
		},
		Machine: "local",
	})

	path := filepath.Join(root, "subagents", "11111111-1111-4111-8111-111111111111.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, ok := engine.classifyOnePath(path, nil)
	if !ok {
		t.Fatal("path did not classify")
	}
	if got.Path != path || got.Project != "subagents" || got.Agent != parser.AgentWorkBuddy {
		t.Fatalf("classified as %+v", got)
	}
}
