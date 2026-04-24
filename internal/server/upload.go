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

// saveUpload writes the uploaded file to disk under
// <dataDir>/uploads/<project>/<filename> and returns the
// destination path.
func (s *Server) saveUpload(
	project string, filename string, src io.Reader,
) (string, error) {
	uploadDir := filepath.Join(
		s.cfg.DataDir, "uploads", project,
	)
	if err := os.MkdirAll(uploadDir, 0o755); err != nil {
		return "", fmt.Errorf(
			"creating upload directory: %w", err,
		)
	}

	destPath := filepath.Join(uploadDir, filename)
	dest, err := os.Create(destPath)
	if err != nil {
		return "", fmt.Errorf(
			"saving uploaded file: %w", err,
		)
	}
	defer dest.Close()

	if _, err := io.Copy(dest, src); err != nil {
		return "", fmt.Errorf(
			"writing uploaded file: %w", err,
		)
	}
	return destPath, nil
}

// saveSessionToDB maps parsed session and messages to DB types
// and persists them.
func (s *Server) saveSessionToDB(
	sess parser.ParsedSession,
	msgs []parser.ParsedMessage,
) error {
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

	if err := s.db.UpsertSession(dbSess); err != nil {
		if errors.Is(err, db.ErrSessionExcluded) {
			return nil // silently skip excluded sessions
		}
		return fmt.Errorf("storing session: %w", err)
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

	if err := s.db.ReplaceSessionMessages(
		sess.ID, dbMsgs,
	); err != nil {
		return fmt.Errorf("storing messages: %w", err)
	}
	return nil
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

	destPath, err := s.saveUpload(
		req.project, req.filename, req.file,
	)
	if err != nil {
		log.Printf("Error saving upload: %v", err)
		writeError(w, http.StatusInternalServerError,
			"failed to save upload")
		return
	}

	results, err := parser.ParseClaudeSession(
		destPath, req.project, req.machine,
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

	for _, pr := range results {
		if err := s.saveSessionToDB(pr.Session, pr.Messages); err != nil {
			if handleReadOnly(w, err) {
				return
			}
			log.Printf("Error saving session to DB: %v", err)
			writeError(w, http.StatusInternalServerError,
				"failed to save session to database")
			return
		}
	}

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
