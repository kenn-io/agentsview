package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

type source struct {
	Path, ID string
	Agent    parser.AgentType
	Turns    int
	Start    time.Time
	Store    *sql.DB
}

func corpus(dir string, o options) ([]source, map[parser.AgentType][]string, error) {
	if o.SourceFormat == "opencode" {
		return openCodeCorpus(dir, o)
	}
	roots := map[parser.AgentType][]string{
		parser.AgentClaude: {filepath.Join(dir, "claude")}, parser.AgentCodex: {filepath.Join(dir, "codex")},
	}
	var sources []source
	for i := 0; i < o.Sessions+o.Empty; i++ {
		agent := parser.AgentCodex
		if i >= o.Sessions || i%2 == 0 {
			agent = parser.AgentClaude
		}
		id := fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1)
		start := time.Date(2026, 6, 1+i%28, 10, 0, 0, 0, time.UTC)
		path := filepath.Join(roots[agent][0], fmt.Sprintf("project-%02d", i%20), id+".jsonl")
		if agent == parser.AgentCodex {
			path = filepath.Join(roots[agent][0], "2026", "06", start.Format("02"), "rollout-"+start.Format("2006-01-02T15-04-05")+"-"+id+".jsonl")
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, nil, err
		}
		s := source{Path: path, ID: id, Agent: agent, Start: start}
		initial := ""
		if agent == parser.AgentCodex {
			initial = testjsonl.CodexSessionMetaJSON(id, "/workspace/project-a", "codex_cli_rs", start.Format(time.RFC3339)) + "\n" + testjsonl.CodexTurnContextJSON("gpt-5.4", start.Format(time.RFC3339)) + "\n"
		}
		if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
			return nil, nil, err
		}
		if i < o.Sessions {
			turns := o.Turns
			if i < o.Active && o.ActiveTurns > 0 {
				turns = o.ActiveTurns
			}
			if err := s.appendTurns(turns, o.ContentBytes); err != nil {
				return nil, nil, err
			}
			sources = append(sources, s)
		}
	}
	return sources, roots, nil
}

func (s *source) appendTurns(n, contentBytes int) error {
	if s.Store != nil {
		tx, err := s.Store.Begin()
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if err := s.writeSQLiteTurns(tx, n, contentBytes); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		s.Turns += n
		return nil
	}
	b := testjsonl.NewSessionBuilder()
	for j := range n {
		turn := s.Turns + j
		ts := s.Start.Add(time.Duration(turn) * time.Minute).Format(time.RFC3339)
		text := fmt.Sprintf("Investigate query latency in module %d. ", turn) + strings.Repeat("sample code and context ", contentBytes/24)
		if s.Agent == parser.AgentClaude {
			b.AddClaudeUserWithSessionID(ts, text, s.ID)
			b.AddClaudeAssistantUsage(ts, "Implemented and checked the query. "+text, testjsonl.ClaudeAssistantUsage{
				MessageID: fmt.Sprintf("msg-%s-%d", s.ID, turn), RequestID: fmt.Sprintf("req-%s-%d", s.ID, turn), Model: "claude-sonnet-4-20250514", InputTokens: 1000 + turn, OutputTokens: 200,
			})
		} else {
			b.AddCodexMessage(ts, "user", text).AddCodexMessage(ts, "assistant", "Implemented and checked the query. "+text).
				AddRaw(testjsonl.CodexTokenCountJSON(ts, 1000+turn, 200, 100))
		}
	}
	f, err := os.OpenFile(s.Path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = f.WriteString(b.String())
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	s.Turns += n
	return nil
}
