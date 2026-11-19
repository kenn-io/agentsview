// Package xaiusage reads account usage exports from xAI.
package xaiusage

import (
	"encoding/csv"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/money"
)

// ReadCSV validates an xAI account usage export before returning any events.
// Billing item IDs deduplicate overlapping exports. Models may be absent.
func ReadCSV(r io.Reader) ([]db.CursorUsageEvent, error) {
	reader := csv.NewReader(r)
	header, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("reading xAI CSV header: %w", err)
	}
	columns := make(map[string]int, len(header))
	for i, name := range header {
		columns[strings.TrimPrefix(name, "\uFEFF")] = i
	}
	for _, name := range []string{
		"Billing Item ID", "Event Time", "Product", "Charge Category",
		"Usage Type", "Principal ID", "Usage", "Pricing Currency",
		"Pricing Contracted", "Model", "User Email",
	} {
		if _, ok := columns[name]; !ok {
			return nil, fmt.Errorf("xAI CSV missing column %q", name)
		}
	}
	var events []db.CursorUsageEvent
	for {
		record, err := reader.Read()
		if errors.Is(err, io.EOF) {
			return events, nil
		}
		if err != nil {
			return nil, fmt.Errorf("reading xAI CSV: %w", err)
		}
		line, _ := reader.FieldPos(0)
		get := func(name string) string { return record[columns[name]] }
		ev, err := parseRecord(get)
		if err != nil {
			return nil, fmt.Errorf("xAI CSV line %d: %w", line, err)
		}
		events = append(events, ev)
	}
}

func parseRecord(get func(string) string) (db.CursorUsageEvent, error) {
	var ev db.CursorUsageEvent
	id := get("Billing Item ID")
	if strings.TrimSpace(id) == "" {
		return ev, errors.New("missing Billing Item ID")
	}
	switch product := get("Product"); product {
	case "grok-build":
		ev.Agent = "grok"
	case "grok-bot", "cursor":
		ev.Agent = product
	default:
		return ev, fmt.Errorf("unsupported Product %q", product)
	}
	when, err := time.Parse(time.RFC3339Nano, get("Event Time"))
	if err != nil {
		return ev, fmt.Errorf("invalid Event Time: %w", err)
	}
	ev.OccurredAt = when.UTC().Format(time.RFC3339Nano)
	if get("Pricing Currency") != "USD" {
		return ev, fmt.Errorf("unsupported Pricing Currency %q", get("Pricing Currency"))
	}
	ev.Charged, err = money.ParseDollars(get("Pricing Contracted"))
	if err != nil {
		return ev, fmt.Errorf("invalid Pricing Contracted: %w", err)
	}
	var usage map[string]string
	if err := json.Unmarshal([]byte(get("Usage")), &usage); err != nil || usage == nil {
		return ev, errors.New("usage must be a JSON object of token counters")
	}
	seen := make(map[*int]bool)
	for key, value := range usage {
		var target *int
		switch key {
		case "Prompt text tokens", "prompt-text-tokens":
			target = &ev.InputTokens
		case "Completion text tokens", "completion-text-tokens":
			target = &ev.OutputTokens
		case "Cached prompt text tokens", "cached-prompt-text-tokens":
			target = &ev.CacheReadTokens
		case "cache-write-text-tokens":
			target = &ev.CacheWriteTokens
		default:
			return ev, fmt.Errorf("unsupported Usage counter %q", key)
		}
		if seen[target] {
			return ev, fmt.Errorf("duplicate Usage counter %q", key)
		}
		count, err := strconv.Atoi(value)
		if err != nil || count < 0 {
			return ev, fmt.Errorf("invalid token count for %q", key)
		}
		*target = count
		seen[target] = true
	}
	ev.Model = get("Model")
	ev.Kind = get("Product") + ":" + get("Charge Category") + ":" + get("Usage Type")
	ev.UserID = get("Principal ID")
	ev.UserEmail = get("User Email")
	ev.DedupKey = "xai:" + id
	return ev, nil
}
