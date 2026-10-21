package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/v2"
	"fmt"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/fileevidence"
)

const fileEvidenceSession = "test-session-file-references"

var fileEvidenceNames = []string{"pasted.png", "written.txt", "archived.txt", "absent.txt"}

func fileEvidenceBytes(index int) []byte {
	if index == 0 {
		body, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
		return body
	}
	return []byte(fileEvidenceNames[index] + " exact example bytes\n")
}

func fileEvidenceDigest(index int) string {
	return fmt.Sprintf("%x", sha256.Sum256(fileEvidenceBytes(index)))
}

func createFileEvidenceFixture(ctx context.Context, database *db.DB, start time.Time) error {
	messages := []db.Message{
		{Role: "user", Content: "Find the pasted image and files from these tool calls."},
		{Role: "assistant", Content: "Write two example files."},
		{Role: "assistant", Content: "Write one file absent from Docbank."},
	}
	for i := range messages {
		m := &messages[i]
		m.SessionID, m.Ordinal, m.SourceUUID = fileEvidenceSession, i, fmt.Sprintf("file-evidence-%d", i)
		m.Timestamp = start.Add(time.Duration(i) * time.Minute).Format(time.RFC3339Nano)
		m.FileEvidenceCoverage = fileevidence.Coverage{Version: 1, State: "complete"}
	}
	for i, ordinal := range []int{0, 1, 1, 2} {
		m, body := &messages[ordinal], fileEvidenceBytes(i)
		candidate := fileevidence.Candidate{
			Payload: body, Support: "message", SupportingText: m.Content, Call: -1, Result: -1,
			Record: fileevidence.Record{Position: fileevidence.Position{Entry: m.SourceUUID, Call: -1, Result: -1, Block: 0}, Kind: "image", MediaType: "image/png", DisplayName: fileEvidenceNames[i], Size: new(int64(len(body))), SHA256: fileEvidenceDigest(i), ProofMethod: fileevidence.ProofInlinePayloadV1},
		}
		if i > 0 {
			input, err := json.Marshal(map[string]string{"path": fileEvidenceNames[i], "content": string(body)})
			if err != nil {
				return err
			}
			candidate.Call = len(m.ToolCalls)
			candidate.Support, candidate.SupportingText = "input", string(input)
			candidate.Record.Kind, candidate.Record.MediaType = "write", "text/plain"
			candidate.Record.Retention, candidate.Record.ProofMethod = "source", fileevidence.ProofPiWriteUTF8V1
			candidate.Record.Position.Call = candidate.Call
			m.ToolCalls = append(m.ToolCalls, db.ToolCall{ToolName: "write", ToolUseID: "reused-fixture-id", Category: "Write", InputJSON: string(input), ResultContent: "Successfully wrote file"})
		}
		m.FileEvidenceCandidates = append(m.FileEvidenceCandidates, candidate)
	}
	if err := database.UpsertSession(ctx, db.Session{ID: fileEvidenceSession, Project: "project-files", Agent: "pi", Machine: "test-machine", MessageCount: len(messages), UserMessageCount: 1, FirstMessage: new(messages[0].Content), StartedAt: new(messages[0].Timestamp), EndedAt: new(messages[2].Timestamp)}); err != nil {
		return err
	}
	return database.ReplaceSessionMessages(ctx, fileEvidenceSession, messages)
}
