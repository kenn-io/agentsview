package mcp

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/kit/safefileio"
)

type callRecord struct {
	At       time.Time `json:"at"`
	Tool     string    `json:"tool"`
	Outcome  string    `json:"outcome"`
	Error    string    `json:"error,omitempty"`
	Rejected []string  `json:"rejected,omitempty"`
	Bytes    int       `json:"bytes"`
	Ms       int       `json:"ms"`
}

type CallCount struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}
type CallSummary struct {
	Tool         string      `json:"tool"`
	Calls        int         `json:"calls"`
	ErrorPercent float64     `json:"error_percent"`
	TopError     *CallCount  `json:"top_error,omitempty"`
	EmptyPercent *float64    `json:"empty_percent,omitempty"`
	Rejected     []CallCount `json:"rejected,omitempty"`
	MedianBytes  int         `json:"median_bytes"`
	MedianMs     int         `json:"median_ms"`
}

var rejectedNames = regexp.MustCompile(`"([a-z][a-z0-9_]{0,63})"`)

func searchField(tool string) string {
	switch tool {
	case ToolSearchSessions, ToolSearchDocs:
		return "results"
	case ToolSearchContent:
		return "matches"
	case ToolQueryRecall:
		return "entries"
	}
	return ""
}

func knownTool(name string) bool {
	switch name {
	case ToolSearchSessions, ToolQueryRecall, ToolListRecall, ToolListSessions, ToolGetSessionOverview, ToolGetMessages, ToolSearchContent, ToolGetMemoryStatus, ToolGetUsageSummary, ToolGetSessionUsage, ToolGetToolCalls, ToolGetRecentEdits, ToolGetChildSessions, ToolCompareUsage, ToolGetActivityReport, ToolGetToolUsage, ToolGetQualitySignals, ToolGetTermTrends, ToolSearchDocs, ToolGetFrictionDigest, ToolListFrictionPatterns, ToolGetSessionFileReferences, ToolGetCompactionSummaries:
		return true
	}
	return false
}

func recordToolCall(dir string, req *mcp.CallToolRequest, result mcp.Result, callErr error, elapsed time.Duration) {
	if dir == "" || req == nil || req.Params == nil {
		return
	}
	name := req.Params.Name
	if !knownTool(name) {
		name = "unknown tool"
	}
	rec := callRecord{At: time.Now().UTC(), Tool: name, Outcome: "ok", Ms: int(elapsed.Milliseconds())}
	call, _ := result.(*mcp.CallToolResult)
	var message string
	if call != nil {
		for _, c := range call.Content {
			if text, ok := c.(*mcp.TextContent); ok {
				rec.Bytes += len(text.Text)
				if call.IsError {
					message += text.Text
				}
			}
		}
	}
	if callErr != nil || (call != nil && call.IsError) {
		rec.Outcome = "error"
		switch {
		case strings.HasPrefix(message, `validating "arguments":`):
			rec.Error = "invalid arguments"
			if _, rest, found := strings.Cut(message, "unexpected additional properties "); found {
				if end := strings.IndexByte(rest, ']'); end >= 0 {
					var args map[string]json.RawMessage
					_ = json.Unmarshal(req.Params.Arguments, &args)
					for _, match := range rejectedNames.FindAllStringSubmatch(rest[:end], -1) {
						if _, ok := args[match[1]]; ok {
							rec.Rejected = append(rec.Rejected, match[1])
						}
					}
				}
			}
		case strings.HasPrefix(message, "session not found:"):
			rec.Error = "session not found"
		default:
			rec.Error = "other error"
		}
	} else if field := searchField(name); field != "" && call != nil {
		b, err := json.Marshal(call.StructuredContent)
		if err == nil {
			var fields map[string]json.RawMessage
			if json.Unmarshal(b, &fields) == nil {
				var entries []json.RawMessage
				if value, ok := fields[field]; ok && json.Unmarshal(value, &entries) == nil && len(entries) == 0 {
					rec.Outcome = "empty"
				}
			}
		}
	}
	if safefileio.EnsurePrivateDir(dir) != nil {
		return
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, rec.At.Format("2006-01-02")+".jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n'))
}

type callAggregate struct {
	row              CallSummary
	errors, rejected map[string]int
	empty            int
	bytes, ms        []int
}

// SummarizeCalls reads recorded calls within inclusive UTC date bounds.
func SummarizeCalls(dir, from, to string) ([]CallSummary, error) {
	files, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return []CallSummary{}, nil
	}
	if err != nil {
		return nil, err
	}
	groups := map[string]*callAggregate{}
	for _, file := range files {
		name := file.Name()
		if file.IsDir() || len(name) != 16 || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		date := strings.TrimSuffix(name, ".jsonl")
		if _, err := time.Parse("2006-01-02", date); err != nil || !db.InDateRange(date, from, to) {
			continue
		}
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			var rec callRecord
			if json.Unmarshal(scanner.Bytes(), &rec) != nil || !db.InDateRange(rec.At.UTC().Format("2006-01-02"), from, to) {
				continue
			}
			g := groups[rec.Tool]
			if g == nil {
				g = &callAggregate{row: CallSummary{Tool: rec.Tool}, errors: map[string]int{}, rejected: map[string]int{}}
				groups[rec.Tool] = g
			}
			g.row.Calls++
			if rec.Outcome == "error" {
				g.errors[rec.Error]++
			}
			if rec.Outcome == "empty" {
				g.empty++
			}
			for _, name := range rec.Rejected {
				g.rejected[name]++
			}
			g.bytes = append(g.bytes, rec.Bytes)
			g.ms = append(g.ms, rec.Ms)
		}
		err = scanner.Err()
		closeErr := f.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
	}
	rows := make([]CallSummary, 0, len(groups))
	for _, g := range groups {
		var errors int
		for _, count := range g.errors {
			errors += count
		}
		g.row.ErrorPercent = 100 * float64(errors) / float64(g.row.Calls)
		if top := rankedCounts(g.errors); len(top) > 0 {
			g.row.TopError = &top[0]
		}
		if searchField(g.row.Tool) != "" {
			pct := 100 * float64(g.empty) / float64(g.row.Calls)
			g.row.EmptyPercent = &pct
		}
		g.row.Rejected = rankedCounts(g.rejected)
		if len(g.row.Rejected) > 3 {
			g.row.Rejected = g.row.Rejected[:3]
		}
		sort.Ints(g.bytes)
		sort.Ints(g.ms)
		g.row.MedianBytes = db.MedianInt(g.bytes, len(g.bytes))
		g.row.MedianMs = db.MedianInt(g.ms, len(g.ms))
		rows = append(rows, g.row)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Calls != rows[j].Calls {
			return rows[i].Calls > rows[j].Calls
		}
		return rows[i].Tool < rows[j].Tool
	})
	return rows, nil
}

func rankedCounts(counts map[string]int) []CallCount {
	var result []CallCount
	for name, count := range counts {
		result = append(result, CallCount{Name: name, Count: count})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Count != result[j].Count {
			return result[i].Count > result[j].Count
		}
		return result[i].Name < result[j].Name
	})
	return result
}
