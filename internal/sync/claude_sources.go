package sync

import (
	"context"
	"errors"
	"os"

	"go.kenn.io/agentsview/internal/parser"
)

func (e *Engine) localClaudeSubagentSources(pw pendingWrite) []string {
	// Remote materializations may use temporary paths or another machine's
	// source keys. Only local ingests can retain filesystem provenance.
	if pw.sess.Agent != parser.AgentClaude || isS3SourcePath(pw.sess.File.Path) ||
		e.pathRewriter != nil || e.idPrefix != "" ||
		!e.isLocalMachineAttribution(pw.sess.Machine) {
		return nil
	}
	return pw.sess.ClaudeSubagentSources
}

type claudeSubagentSourceReader interface {
	GetClaudeSubagentSources(context.Context, string) ([]string, error)
}

func (e *Engine) recordedClaudeSubagentSources(ctx context.Context, sessionID string) ([]string, error) {
	var reader claudeSubagentSourceReader = e.db
	if archived, ok := e.archiveStore.(claudeSubagentSourceReader); ok {
		reader = archived
	}
	paths, err := reader.GetClaudeSubagentSources(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 && e.archiveStore != nil {
		paths, err = e.db.GetClaudeSubagentSources(ctx, sessionID)
		if err != nil {
			return nil, err
		}
	}
	return paths, nil
}

func (e *Engine) claudeSubagentWriteSources(ctx context.Context, pw pendingWrite) ([]string, error) {
	paths := e.localClaudeSubagentSources(pw)
	if paths == nil || len(paths) > 1 {
		return paths, nil
	}
	stored, err := e.recordedClaudeSubagentSources(ctx, applyIDPrefixToID(e.idPrefix, pw.sess.ID))
	if err != nil || len(stored) == 0 {
		return nil, err
	}
	return paths, nil
}

func (e *Engine) missingClaudeSubagentSource(ctx context.Context, sessionID string) (bool, error) {
	paths, err := e.recordedClaudeSubagentSources(ctx, sessionID)
	if err != nil {
		return false, err
	}
	for _, path := range paths {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		if !info.Mode().IsRegular() {
			return true, nil
		}
	}
	return false, nil
}
