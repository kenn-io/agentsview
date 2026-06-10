package server

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/timeutil"
)

type stagedUpload struct {
	tempPath  string
	tempDir   string
	finalPath string
}

type committedUpload struct {
	finalPath   string
	backupPath  string
	hadPrevious bool
	movedFinal  bool
}

// stageUpload writes the uploaded file to a temporary path in
// <dataDir>/uploads/<project>. The caller must either commit
// or remove the staged file.
func (s *Server) stageUpload(
	project string, filename string, src io.Reader,
) (stagedUpload, error) {
	uploadDir := filepath.Join(
		s.cfg.DataDir, "uploads", project,
	)
	if err := os.MkdirAll(uploadDir, 0o755); err != nil {
		return stagedUpload{}, fmt.Errorf(
			"creating upload directory: %w", err,
		)
	}

	tempDir, err := os.MkdirTemp(
		uploadDir, "."+strings.TrimSuffix(filename, ".jsonl")+".*.tmp",
	)
	if err != nil {
		return stagedUpload{}, fmt.Errorf(
			"saving uploaded file: %w", err,
		)
	}
	finalPath := filepath.Join(uploadDir, filename)
	tempPath := filepath.Join(tempDir, filename)
	dest, err := os.Create(tempPath)
	if err != nil {
		_ = os.RemoveAll(tempDir)
		return stagedUpload{}, fmt.Errorf(
			"saving uploaded file: %w", err,
		)
	}

	if _, err := io.Copy(dest, src); err != nil {
		_ = dest.Close()
		_ = os.RemoveAll(tempDir)
		return stagedUpload{}, fmt.Errorf(
			"writing uploaded file: %w", err,
		)
	}
	if err := dest.Close(); err != nil {
		_ = os.RemoveAll(tempDir)
		return stagedUpload{}, fmt.Errorf(
			"closing uploaded file: %w", err,
		)
	}
	return stagedUpload{
		tempPath:  tempPath,
		tempDir:   tempDir,
		finalPath: finalPath,
	}, nil
}

func commitUpload(upload stagedUpload) (committedUpload, error) {
	state := committedUpload{finalPath: upload.finalPath}

	info, err := os.Lstat(upload.finalPath)
	switch {
	case err == nil:
		if !info.Mode().IsRegular() {
			return state, fmt.Errorf(
				"committing upload: destination is not a regular file",
			)
		}
		backupPath, err := createUploadBackupPath(upload.finalPath)
		if err != nil {
			return state, err
		}
		state.backupPath = backupPath
		state.hadPrevious = true
		if err := os.Rename(upload.finalPath, backupPath); err != nil {
			return state, fmt.Errorf(
				"backing up existing upload: %w", err,
			)
		}
	case errors.Is(err, os.ErrNotExist):
	default:
		return state, fmt.Errorf(
			"checking upload destination: %w", err,
		)
	}

	if err := os.Rename(upload.tempPath, upload.finalPath); err != nil {
		if state.hadPrevious {
			if rbErr := os.Rename(state.backupPath, upload.finalPath); rbErr != nil {
				return state, fmt.Errorf(
					"committing upload: %w (restore previous upload failed: %v)",
					err, rbErr,
				)
			}
			state.hadPrevious = false
			state.backupPath = ""
		}
		return state, fmt.Errorf("committing upload: %w", err)
	}
	state.movedFinal = true
	return state, nil
}

func createUploadBackupPath(finalPath string) (string, error) {
	f, err := os.CreateTemp(
		filepath.Dir(finalPath),
		"."+filepath.Base(finalPath)+".*.bak",
	)
	if err != nil {
		return "", fmt.Errorf("creating upload backup: %w", err)
	}
	path := f.Name()
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("closing upload backup: %w", err)
	}
	if err := os.Remove(path); err != nil {
		return "", fmt.Errorf("preparing upload backup: %w", err)
	}
	return path, nil
}

func rollbackCommittedUpload(upload committedUpload) error {
	if !upload.movedFinal {
		return nil
	}
	if err := os.Remove(upload.finalPath); err != nil &&
		!errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing committed upload: %w", err)
	}
	if upload.hadPrevious {
		if err := os.Rename(upload.backupPath, upload.finalPath); err != nil {
			return fmt.Errorf("restoring previous upload: %w", err)
		}
	}
	return nil
}

func cleanupCommittedUpload(upload committedUpload) {
	if upload.hadPrevious && upload.backupPath != "" {
		_ = os.Remove(upload.backupPath)
	}
}

// sessionBatchWriteFromParsed maps parsed session and messages
// to DB types for an upload transaction.
func sessionBatchWriteFromParsed(
	sess parser.ParsedSession,
	msgs []parser.ParsedMessage,
) db.SessionBatchWrite {
	hasTotal, hasPeak := sess.TokenCoverage(msgs)
	dbSess := db.Session{
		ID:                   sess.ID,
		Project:              sess.Project,
		Machine:              sess.Machine,
		Agent:                string(sess.Agent),
		MessageCount:         sess.MessageCount,
		UserMessageCount:     sess.UserMessageCount,
		ParentSessionID:      strPtr(sess.ParentSessionID),
		RelationshipType:     string(sess.RelationshipType),
		TotalOutputTokens:    sess.TotalOutputTokens,
		PeakContextTokens:    sess.PeakContextTokens,
		HasTotalOutputTokens: hasTotal,
		HasPeakContextTokens: hasPeak,
		FilePath:             strPtr(sess.File.Path),
		FileSize:             int64Ptr(sess.File.Size),
		FileMtime:            int64Ptr(sess.File.Mtime),
		FileHash:             strPtr(sess.File.Hash),
	}
	if sess.FirstMessage != "" {
		dbSess.FirstMessage = &sess.FirstMessage
	}
	dbSess.SessionName = db.ParsedSessionName(sess)
	if !sess.StartedAt.IsZero() {
		dbSess.StartedAt = timeutil.Ptr(sess.StartedAt)
	}
	if !sess.EndedAt.IsZero() {
		dbSess.EndedAt = timeutil.Ptr(sess.EndedAt)
	}

	dbMsgs := make([]db.Message, len(msgs))
	for i, m := range msgs {
		hasCtx, hasOut := m.TokenPresence()
		dbMsgs[i] = db.Message{
			SessionID:        sess.ID,
			Ordinal:          m.Ordinal,
			Role:             string(m.Role),
			Content:          m.Content,
			Timestamp:        timeutil.Format(m.Timestamp),
			HasThinking:      m.HasThinking,
			HasToolUse:       m.HasToolUse,
			ContentLength:    m.ContentLength,
			Model:            m.Model,
			TokenUsage:       m.TokenUsage,
			ContextTokens:    m.ContextTokens,
			OutputTokens:     m.OutputTokens,
			HasContextTokens: hasCtx,
			HasOutputTokens:  hasOut,
		}
	}

	// Signals and Findings are intentionally not computed for uploads:
	// the upload path does not run the sync engine's derived-data
	// pipeline, so zero-valued signal columns and no findings rows are
	// the expected state for freshly uploaded sessions.
	return db.SessionBatchWrite{
		Session:         dbSess,
		Messages:        dbMsgs,
		ReplaceMessages: true,
	}
}

// isSafeName rejects names containing path separators, "..",
// or starting with "." to prevent directory traversal.
func isSafeName(name string) bool {
	if name == "" {
		return false
	}
	if strings.ContainsAny(name, "/\\") {
		return false
	}
	if strings.HasPrefix(name, ".") {
		return false
	}
	return true
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func int64Ptr(n int64) *int64 {
	if n == 0 {
		return nil
	}
	return &n
}
