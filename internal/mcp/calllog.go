package mcp

import (
	"bufio"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/agentsview/internal/ctxio"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/kit/safefileio"
)

const (
	callRecordVersion    = 1
	maxRecordedNameBytes = 128
	maxRejectedNames     = 16
)

type callRecord struct {
	Version      int       `json:"version"`
	At           time.Time `json:"at"`
	Tool         string    `json:"tool"`
	Outcome      string    `json:"outcome"`
	Error        string    `json:"error,omitempty"`
	Rejected     []string  `json:"rejected,omitempty"`
	Bytes        int       `json:"bytes"`
	Ms           int       `json:"ms"`
	Continuation bool      `json:"continuation,omitzero"`
}

type CallCount struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}
type CallSummary struct {
	Tool             string      `json:"tool"`
	Calls            int         `json:"calls"`
	ErrorPercent     float64     `json:"error_percent"`
	TruncatedPercent float64     `json:"truncated_percent"`
	TopError         *CallCount  `json:"top_error,omitempty"`
	EmptyPercent     *float64    `json:"empty_percent,omitempty"`
	Rejected         []CallCount `json:"rejected,omitempty"`
	MedianBytes      int         `json:"median_bytes"`
	MedianMs         int         `json:"median_ms"`
}

func searchField(tool string) string {
	switch tool {
	case ToolSearchSessions, ToolSearchDocs:
		return "results"
	case ToolListSessions:
		return "sessions"
	case ToolSearchContent:
		return "matches"
	case ToolQueryRecall:
		return "entries"
	}
	return ""
}

func recordToolCall(ctx context.Context, dir string, req *mcp.CallToolRequest, result mcp.Result, callErr error, elapsed time.Duration) error {
	if dir == "" || req == nil || req.Params == nil {
		return nil
	}
	// Record the outcome even when the request has already been canceled.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	name := req.Params.Name
	rec := callRecord{Version: callRecordVersion, At: time.Now().UTC(), Tool: name, Outcome: "ok", Ms: int(elapsed.Milliseconds())}
	if name == ToolSearchSessions || name == ToolSearchContent {
		var args struct {
			Cursor int `json:"cursor"`
		}
		if json.Unmarshal(req.Params.Arguments, &args) == nil {
			rec.Continuation = args.Cursor > 0
		}
	}
	if name == ToolListSessions {
		var args struct {
			Cursor string `json:"cursor"`
		}
		if json.Unmarshal(req.Params.Arguments, &args) == nil {
			rec.Continuation = args.Cursor != ""
		}
	}
	if len(name) == 0 || len(name) > maxRecordedNameBytes || strings.ContainsFunc(name, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.')
	}) {
		rec.Tool = "<invalid tool name>"
	}
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
			rec.Rejected = rejectedProperties(message)
			if len(rec.Rejected) == 0 {
				const prefix = `validating "arguments": validating root: validating /properties/`
				if rest, ok := strings.CutPrefix(message, prefix); ok {
					if path, _, ok := strings.Cut(rest, ": type:"); ok && !strings.Contains(path, "/") {
						name := strings.ReplaceAll(strings.ReplaceAll(path, "~1", "/"), "~0", "~")
						if len(name) <= maxRecordedNameBytes {
							rec.Rejected = []string{name}
						}
					}
				}
			}
		case strings.HasPrefix(message, "session not found:"):
			rec.Error = "session not found"
		default:
			rec.Error = "other error"
		}
	} else if call != nil {
		b, err := json.Marshal(call.StructuredContent)
		if err == nil {
			var content any
			if json.Unmarshal(b, &content) == nil && hasTruncated(content) {
				rec.Outcome = "truncated"
			} else if fields, ok := content.(map[string]any); ok && searchField(name) != "" {
				if value, exists := fields[searchField(name)]; exists {
					if entries, ok := value.([]any); ok && len(entries) == 0 || value == nil {
						rec.Outcome = "empty"
					}
				}
			}
		}
	}
	if err := safefileio.EnsurePrivateDir(dir); err != nil {
		return err
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, rec.At.Format("2006-01-02")+".jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	// A leading newline separates this record from a previous partial write.
	line := append(append([]byte{'\n'}, b...), '\n')
	_, writeErr := (ctxio.Writer{Context: ctx, Writer: f}).Write(line)
	return errors.Join(writeErr, f.Close())
}

func hasTruncated(value any) bool {
	switch value := value.(type) {
	case map[string]any:
		if value["truncated"] == true {
			return true
		}
		for _, child := range value {
			if hasTruncated(child) {
				return true
			}
		}
	case []any:
		for _, child := range value {
			if hasTruncated(child) {
				return true
			}
		}
	}
	return false
}

// rejectedProperties parses the SDK's Go-quoted property list.
func rejectedProperties(message string) []string {
	list, ok := strings.CutPrefix(message, `validating "arguments": validating root: unexpected additional properties [`)
	if !ok {
		return nil
	}
	var names []string
	for len(list) > 0 {
		list = strings.TrimLeft(list, " ")
		if len(list) == 0 || list[0] != '"' {
			break
		}
		quoted, err := strconv.QuotedPrefix(list)
		if err != nil {
			break
		}
		name, err := strconv.Unquote(quoted)
		if err == nil && len(name) <= maxRecordedNameBytes {
			i := sort.SearchStrings(names, name)
			if i < maxRejectedNames {
				names = append(names, "")
				copy(names[i+1:], names[i:])
				names[i] = name
				if len(names) > maxRejectedNames {
					names = names[:maxRejectedNames]
				}
			}
		}
		list = list[len(quoted):]
	}
	return names
}

type callAggregate struct {
	row                        CallSummary
	errors, rejected           map[string]int
	empty, searches, truncated int
	bytes, ms                  []int
}

// SummarizeCalls reads recorded calls within inclusive UTC date bounds.
func SummarizeCalls(ctx context.Context, dir, from, to string) ([]CallSummary, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	files, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return []CallSummary{}, nil
	}
	if err != nil {
		return nil, err
	}
	groups := map[string]*callAggregate{}
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
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
		scanner := bufio.NewScanner(ctxio.Reader{Context: ctx, Reader: f})
		for scanner.Scan() {
			line := scanner.Bytes()
			var header struct {
				Version int `json:"version"`
			}
			if json.Unmarshal(line, &header) != nil {
				continue
			}
			if header.Version != callRecordVersion {
				_ = f.Close()
				return nil, fmt.Errorf("unsupported MCP call record version %d", header.Version)
			}
			var rec callRecord
			if json.Unmarshal(line, &rec) != nil || !db.InDateRange(rec.At.UTC().Format("2006-01-02"), from, to) {
				continue
			}
			if rec.Outcome != "ok" && rec.Outcome != "empty" && rec.Outcome != "error" && rec.Outcome != "truncated" {
				_ = f.Close()
				return nil, fmt.Errorf("unknown MCP call outcome %q", rec.Outcome)
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
			if rec.Outcome == "truncated" {
				g.truncated++
			}
			if searchField(rec.Tool) != "" && !rec.Continuation {
				g.searches++
				if rec.Outcome == "empty" {
					g.empty++
				}
			}
			for _, name := range rec.Rejected {
				g.rejected[name]++
			}
			g.bytes = append(g.bytes, rec.Bytes)
			g.ms = append(g.ms, rec.Ms)
		}
		scanErr := scanner.Err()
		closeErr := f.Close()
		if err := errors.Join(scanErr, closeErr); err != nil {
			return nil, err
		}
	}
	rows := make([]CallSummary, 0, len(groups))
	for _, g := range groups {
		var errors int
		for _, count := range g.errors {
			errors += count
		}
		g.row.ErrorPercent = 100 * float64(errors) / float64(g.row.Calls)
		g.row.TruncatedPercent = 100 * float64(g.truncated) / float64(g.row.Calls)
		if top := rankedCounts(g.errors); len(top) > 0 {
			g.row.TopError = &top[0]
		}
		if searchField(g.row.Tool) != "" && g.searches > 0 {
			pct := 100 * float64(g.empty) / float64(g.searches)
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
