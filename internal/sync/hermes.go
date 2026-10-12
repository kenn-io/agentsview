package sync

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"go.kenn.io/agentsview/internal/parser"
)

func (e *Engine) validateHermesSnapshotReplacement(ctx context.Context, session parser.ParsedSession) error {
	if session.Agent != parser.AgentHermes && session.Agent != parser.AgentAugureDesktop {
		return nil
	}
	if filepath.Ext(session.File.Path) != ".json" {
		return nil
	}

	id := applyIDPrefixToID(e.idPrefix, session.ID)
	stored, err := e.db.GetSessionFull(ctx, id)
	if err != nil {
		return err
	}
	if stored == nil || stored.FilePath == nil || filepath.Ext(*stored.FilePath) != ".jsonl" {
		return nil
	}

	incoming := session.File.Path
	if e.pathRewriter != nil {
		incoming = e.pathRewriter(incoming)
	}

	preferred := filepath.Join(filepath.Dir(incoming), strings.TrimPrefix(filepath.Base(incoming), "session_"))
	preferred = strings.TrimSuffix(preferred, ".json") + ".jsonl"
	if filepath.Clean(*stored.FilePath) != filepath.Clean(preferred) {
		return nil
	}

	return fmt.Errorf("hermes JSON snapshot cannot replace archived JSONL history for session %s", id)
}
