package sync

import (
	"os"
	"path/filepath"
	"testing"

	"go.kenn.io/agentsview/internal/parser"
)

func TestBoundedCoverageFileIdentityIgnoresMutableObservations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.db")
	if err := os.WriteFile(path, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	beforeInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	before := boundedCoverageFileIdentity(path, beforeInfo)
	if err := os.WriteFile(path, []byte("a larger database observation"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	afterInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	after := boundedCoverageFileIdentity(path, afterInfo)
	if before != after {
		t.Fatalf("mutable file observations revoked an unchanged physical lease: before=%+v after=%+v", before, after)
	}
}

func TestBoundedCoverageUsesEngineWriteOwner(t *testing.T) {
	engine := &Engine{}
	engine.syncMu.Lock()
	acquired := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		engine.syncMu.Lock()
		close(acquired)
		engine.syncMu.Unlock()
	}()
	select {
	case <-acquired:
		t.Fatal("bounded source operation bypassed the engine write owner")
	default:
	}
	engine.syncMu.Unlock()
	<-acquired
	<-done
}

func TestBoundedCoverageTransitionRetiresBeforeApply(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.db")
	if err := os.WriteFile(path, []byte("db"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	engine := &Engine{boundedCoverageGenerations: make(map[string]uint64)}
	lease := &BoundedCoverageLease{
		Binding: BoundedCoverageBinding{
			Agent: parser.AgentOpenCode, PhysicalDBPath: path,
			Scope: filepath.Dir(path), Generation: 1,
		},
		Provider: parser.AgentOpenCode, PhysicalDBPath: path,
		ExactProviderScope: filepath.Dir(path), Generation: 1,
		FileIdentity: boundedCoverageFileIdentity(path, info), fileInfo: info,
	}
	if _, err := engine.TransitionBoundedCoverageRequest(t.Context(), lease, nil, parser.OpenCodeCoverageCheckpoint{}, true); err != nil {
		t.Fatal(err)
	}
	retired := *lease
	retired.Generation = 2
	retired.Binding.Generation = 2
	if _, err := engine.TransitionBoundedCoverageRequest(t.Context(), &retired, nil, parser.OpenCodeCoverageCheckpoint{}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.TransitionBoundedCoverageRequest(t.Context(), lease, nil, parser.OpenCodeCoverageCheckpoint{}, false); err == nil {
		t.Fatal("retired generation was accepted for apply")
	}
}

func TestBoundedCoverageTransitionRejectsMismatchedSourceProvider(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "opencode.db")
	if err := os.WriteFile(path, []byte("db"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	engine := &Engine{boundedCoverageGenerations: make(map[string]uint64)}
	lease := &BoundedCoverageLease{
		Binding: BoundedCoverageBinding{
			Agent: parser.AgentOpenCode, PhysicalDBPath: path, Scope: root, Generation: 1,
		},
		Provider: parser.AgentOpenCode, PhysicalDBPath: path,
		ExactProviderScope: root, Generation: 1,
		FileIdentity: boundedCoverageFileIdentity(path, info), fileInfo: info,
	}
	source := parser.SourceRef{Provider: parser.AgentClaude, DisplayPath: path}
	if _, err := engine.TransitionBoundedCoverageRequest(
		t.Context(), lease, []parser.SourceRef{source}, parser.OpenCodeCoverageCheckpoint{}, false,
	); err == nil {
		t.Fatal("source from another provider crossed the bounded write boundary")
	}
}

func TestBoundedCoverageTransitionRequiresScopedGenerationBinding(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "opencode.db")
	if err := os.WriteFile(path, []byte("db"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	engine := &Engine{boundedCoverageGenerations: make(map[string]uint64)}
	lease := &BoundedCoverageLease{
		Binding: BoundedCoverageBinding{
			Agent: parser.AgentOpenCode, PhysicalDBPath: path, Generation: 1,
		},
		Provider: parser.AgentOpenCode, PhysicalDBPath: path, Generation: 1,
		FileIdentity: boundedCoverageFileIdentity(path, info), fileInfo: info,
	}
	if _, err := engine.TransitionBoundedCoverageRequest(
		t.Context(), lease, nil, parser.OpenCodeCoverageCheckpoint{}, true,
	); err == nil {
		t.Fatal("bounded transition accepted a lease without exact scope binding")
	}
}
