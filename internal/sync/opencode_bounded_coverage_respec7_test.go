package sync

import (
	"os"
	"path/filepath"
	"testing"
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
