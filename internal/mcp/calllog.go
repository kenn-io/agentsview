package mcp

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/kit/safefileio"
)

type callRecord struct {
	Tool         string   `json:"tool"`
	Outcome      string   `json:"outcome"`
	Args         []string `json:"args,omitempty"`
	Empty        bool     `json:"empty,omitzero"`
	Truncated    bool     `json:"truncated,omitzero"`
	Rejected     []string `json:"rejected,omitempty"`
	Bytes        int      `json:"bytes"`
	Ms           int      `json:"ms"`
	Continuation bool     `json:"continuation,omitzero"`
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

var callLogWarning sync.Once

// warnCallLogFailure tells the user once per process that calls are not being
// recorded; the tool call itself still succeeds.
func warnCallLogFailure(err error) {
	if err != nil {
		callLogWarning.Do(func() {
			fmt.Fprintf(os.Stderr, "agentsview: MCP call log is not recording calls: %v\n", err)
		})
	}
}

func recordToolCall(dir string, req *mcp.CallToolRequest, result mcp.Result, callErr error, elapsed time.Duration) error {
	if dir == "" || req == nil || req.Params == nil {
		return nil
	}
	name := req.Params.Name
	rec := callRecord{Tool: name, Outcome: "ok", Ms: int(elapsed.Milliseconds())}
	var args map[string]any
	if json.Unmarshal(req.Params.Arguments, &args) == nil {
		for key := range args {
			rec.Args = append(rec.Args, key)
		}
		switch cursor := args["cursor"].(type) {
		case string:
			rec.Continuation = cursor != ""
		case float64:
			rec.Continuation = cursor > 0
		}
	}
	call, _ := result.(*mcp.CallToolResult)
	if call != nil {
		for _, c := range call.Content {
			if text, ok := c.(*mcp.TextContent); ok {
				rec.Bytes += len(text.Text)
				if call.IsError {
					rec.Rejected = append(rec.Rejected, rejectedProperties(text.Text, args)...)
				}
			}
		}
	}
	if callErr != nil || (call != nil && call.IsError) {
		rec.Outcome = "error"
	} else if call != nil {
		b, err := json.Marshal(call.StructuredContent)
		if err == nil {
			var content any
			if json.Unmarshal(b, &content) == nil {
				rec.Truncated = hasTruncated(content)
				if fields, ok := content.(map[string]any); ok && searchField(name) != "" {
					if value, exists := fields[searchField(name)]; exists {
						if entries, ok := value.([]any); ok && len(entries) == 0 || value == nil {
							rec.Empty = true
						}
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
	f, err := os.OpenFile(filepath.Join(dir, time.Now().UTC().Format("2006-01-02")+".jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	// A leading newline separates this record from a previous partial write.
	line := append(append([]byte{'\n'}, b...), '\n')
	_, writeErr := f.Write(line)
	return errors.Join(writeErr, f.Close())
}

func hasTruncated(value any) bool {
	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			if strings.Contains(key, "truncated") && child == true {
				return true
			}
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

func rejectedProperties(message string, args map[string]any) []string {
	list, ok := strings.CutPrefix(message, `validating "arguments": validating root: unexpected additional properties [`)
	if !ok {
		return nil
	}
	var names []string
	for {
		list = strings.TrimLeft(list, " ")
		quoted, err := strconv.QuotedPrefix(list)
		if err != nil {
			break
		}
		name, err := strconv.Unquote(quoted)
		if _, sent := args[name]; err == nil && sent {
			names = append(names, name)
		}
		list = list[len(quoted):]
	}
	return names
}

type callAggregate struct {
	row                        CallSummary
	rejected                   map[string]int
	errors                     int
	empty, searches, truncated int
	bytes, ms                  []int
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
		date, ok := strings.CutSuffix(file.Name(), ".jsonl")
		if file.IsDir() || !ok || !db.InDateRange(date, from, to) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, file.Name()))
		if err != nil {
			return nil, err
		}
		for line := range bytes.Lines(data) {
			var rec callRecord
			if json.Unmarshal(line, &rec) != nil {
				continue
			}
			g := groups[rec.Tool]
			if g == nil {
				g = &callAggregate{row: CallSummary{Tool: rec.Tool}, rejected: map[string]int{}}
				groups[rec.Tool] = g
			}
			g.row.Calls++
			if rec.Outcome == "error" {
				g.errors++
			}
			if rec.Truncated {
				g.truncated++
			}
			if searchField(rec.Tool) != "" && !rec.Continuation {
				g.searches++
				if rec.Empty {
					g.empty++
				}
			}
			for _, name := range rec.Rejected {
				g.rejected[name]++
			}
			g.bytes = append(g.bytes, rec.Bytes)
			g.ms = append(g.ms, rec.Ms)
		}
	}
	rows := make([]CallSummary, 0, len(groups))
	for _, g := range groups {
		g.row.ErrorPercent = 100 * float64(g.errors) / float64(g.row.Calls)
		g.row.TruncatedPercent = 100 * float64(g.truncated) / float64(g.row.Calls)
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
