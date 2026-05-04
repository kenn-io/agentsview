package server

import (
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/wesm/agentsview/internal/db"
	"github.com/wesm/agentsview/internal/parser"
	"github.com/wesm/agentsview/internal/timeutil"
)

type uploadRequest struct {
	project  string
	machine  string
	file     multipart.File
	filename string
}

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

// parseUploadRequest extracts and validates query params and
// the multipart file from an upload request. The caller must
// close req.file when done.
func parseUploadRequest(
	r *http.Request,
) (*uploadRequest, string) {
	project := strings.TrimSpace(
		r.URL.Query().Get("project"),
	)
	if project == "" {
		return nil, "project required"
	}
	if !isSafeName(project) {
		return nil, "invalid project name"
	}

	machine := r.URL.Query().Get("machine")
	if machine == "" {
		machine = "remote"
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		return nil, "file field required"
	}

	if !strings.HasSuffix(header.Filename, ".jsonl") {
		file.Close()
		return nil, "file must be .jsonl"
	}

	safeName := filepath.Base(header.Filename)
	if safeName != header.Filename || !isSafeName(
		strings.TrimSuffix(safeName, ".jsonl"),
	) {
		file.Close()
		return nil, "invalid filename"
	}

	return &uploadRequest{
		project:  project,
		machine:  machine,
		file:     file,
		filename: safeName,
	}, ""
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

	return db.SessionBatchWrite{
		Session:         dbSess,
		Messages:        dbMsgs,
		ReplaceMessages: true,
	}
}

func (s *Server) handleUploadSession(
	w http.ResponseWriter, r *http.Request,
) {
	if s.db.ReadOnly() {
		writeError(w, http.StatusNotImplemented,
			"uploads are not available in read-only mode")
		return
	}

	req, errMsg := parseUploadRequest(r)
	if errMsg != "" {
		writeError(w, http.StatusBadRequest, errMsg)
		return
	}
	if req == nil {
		writeError(w, http.StatusBadRequest, "invalid upload request")
		return
	}
	defer req.file.Close()

	upload, err := s.stageUpload(
		req.project, req.filename, req.file,
	)
	if err != nil {
		log.Printf("Error saving upload: %v", err)
		writeError(w, http.StatusInternalServerError,
			"failed to save upload")
		return
	}
	defer func() {
		_ = os.RemoveAll(upload.tempDir)
	}()

	results, err := parser.ParseClaudeSession(
		upload.tempPath, req.project, req.machine,
	)
	if err != nil {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("parsing session: %v", err))
		return
	}
	if len(results) == 0 {
		writeError(w, http.StatusBadRequest,
			"no sessions parsed from upload")
		return
	}

	parser.InferRelationshipTypes(results)
	for i := range results {
		results[i].Session.File.Path = upload.finalPath
	}

	writes := make([]db.SessionBatchWrite, len(results))
	for i, pr := range results {
		writes[i] = sessionBatchWriteFromParsed(
			pr.Session, pr.Messages,
		)
	}
	var commitErr error
	var uploadCommit committedUpload
	_, err = s.db.WriteSessionBatchAtomic(writes, func() error {
		uploadCommit, commitErr = commitUpload(upload)
		return commitErr
	})
	if err != nil {
		if commitErr != nil {
			log.Printf("Error committing upload: %v", commitErr)
			writeError(w, http.StatusInternalServerError,
				"failed to save upload")
			return
		}
		if uploadCommit.movedFinal {
			if rbErr := rollbackCommittedUpload(uploadCommit); rbErr != nil {
				log.Printf(
					"Error rolling back upload after DB failure: %v",
					rbErr,
				)
				writeError(w, http.StatusInternalServerError,
					"failed to save upload")
				return
			}
			cleanupCommittedUpload(uploadCommit)
		}
		if handleReadOnly(w, err) {
			return
		}
		if errors.Is(err, db.ErrSessionExcluded) ||
			errors.Is(err, db.ErrSessionTrashed) {
			writeError(w, http.StatusConflict,
				"session upload rejected: session is excluded or trashed")
			return
		}
		log.Printf("Error saving session to DB: %v", err)
		writeError(w, http.StatusInternalServerError,
			"failed to save session to database")
		return
	}
	cleanupCommittedUpload(uploadCommit)

	main := results[0]
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id": main.Session.ID,
		"project":    req.project,
		"machine":    req.machine,
		"messages":   len(main.Messages),
		"sessions":   len(results),
	})
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
