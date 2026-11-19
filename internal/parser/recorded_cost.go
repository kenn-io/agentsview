package parser

import (
	"bytes"
	"encoding/json/jsontext"
	"strconv"

	"github.com/tidwall/gjson"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/usagefacts"
)

// SetRecordedCost keeps a cost the harness computed and wrote for this
// message, such as OpenCode's per-message cost. It is stored in TokenUsage
// under usagefacts.RecordedCostKey so it travels with the token counters to
// every backend, where it prices the message only when the catalog has no
// rate for its model (usagefacts.ReportedCostIsPrice). A negative cost is
// ignored. Call it after TokenUsage is final; it replaces an earlier value.
func (m *ParsedMessage) SetRecordedCost(cost money.Money) {
	if cost.Microdollars < 0 {
		return
	}
	m.TokenUsage = withRecordedCost(m.TokenUsage, cost.Microdollars)
}

// RecordedCost returns the cost SetRecordedCost stored, if any.
func (m *ParsedMessage) RecordedCost() (money.Money, bool) {
	cost := usagefacts.ParseTokenUsage(string(m.TokenUsage)).RecordedCostMicrodollars
	if cost == nil {
		return money.Money{}, false
	}
	return money.Money{Microdollars: *cost}, true
}

// withRecordedCost returns usage with RecordedCostKey set to microdollars.
// usage is a parser-built JSON object or empty.
func withRecordedCost(usage jsontext.Value, microdollars int64) jsontext.Value {
	member := `"` + usagefacts.RecordedCostKey + `":` +
		strconv.FormatInt(microdollars, 10)
	object := bytes.TrimSpace(usage)
	if len(object) < 2 || object[0] != '{' || object[len(object)-1] != '}' {
		return jsontext.Value("{" + member + "}")
	}
	var out bytes.Buffer
	out.WriteByte('{')
	dec := jsontext.NewDecoder(bytes.NewReader(object))
	if _, err := dec.ReadToken(); err != nil {
		return jsontext.Value("{" + member + "}")
	}
	wrote := false
	for dec.PeekKind() != '}' {
		token, err := dec.ReadToken()
		if err != nil {
			return usage
		}
		// A token is valid only until the next read, so copy the name first.
		name := token.String()
		value, err := dec.ReadValue()
		if err != nil {
			return usage
		}
		if name == usagefacts.RecordedCostKey {
			continue
		}
		if wrote {
			out.WriteByte(',')
		}
		quoted, err := jsontext.AppendQuote(nil, name)
		if err != nil {
			return usage
		}
		out.Write(quoted)
		out.WriteByte(':')
		out.Write(value)
		wrote = true
	}
	if wrote {
		out.WriteByte(',')
	}
	out.WriteString(member)
	out.WriteByte('}')
	return jsontext.Value(out.Bytes())
}

// recordedCostDollars reads a non-negative USD amount a harness wrote as a
// JSON number. It reports false for a missing, non-numeric, or negative
// value.
func recordedCostDollars(value gjson.Result) (money.Money, bool) {
	if value.Type != gjson.Number || value.Num < 0 {
		return money.Money{}, false
	}
	cost, err := money.ParseDollars(value.Raw)
	if err != nil {
		return money.Money{}, false
	}
	return cost, true
}
