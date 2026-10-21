package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/ingest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/kit/atomicfile"
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
			return state, errors.New("committing upload: destination is not a regular file")
		}
		backupPath, err := createUploadBackupPath(upload.finalPath)
		if err != nil {
			return state, err
		}
		state.backupPath = backupPath
		state.hadPrevious = true
		if err := atomicfile.Replace(upload.finalPath, backupPath); err != nil {
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

	if err := atomicfile.Replace(upload.tempPath, upload.finalPath); err != nil {
		if state.hadPrevious {
			if rbErr := atomicfile.Replace(state.backupPath, upload.finalPath); rbErr != nil {
				return state, fmt.Errorf(
					"committing upload: %w (restore previous upload failed: %w)",
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
		if err := atomicfile.Replace(upload.backupPath, upload.finalPath); err != nil {
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
	ctx context.Context, sess parser.ParsedSession, msgs []parser.ParsedMessage,
	blockedCategories map[string]bool,
) (db.SessionBatchWrite, error) {
	candidate, err := ingest.PrepareCandidate(ctx, parser.ParseResult{Session: sess, Messages: msgs}, ingest.ContentOptions{
		BlockedResultCategories: blockedCategories,
	})
	// Uploads omit the sync engine's derived signals and findings.
	return db.SessionBatchWrite{
		Session: candidate.Session, Messages: candidate.Messages, ReplaceMessages: true,
	}, err
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
