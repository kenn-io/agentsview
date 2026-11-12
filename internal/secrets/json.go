package secrets

import (
	"bytes"
	"encoding/json/jsontext"
	"strings"
)

// RedactJSON masks secrets inside the decoded strings of a JSON document and
// returns the document with only the changed strings re-encoded. Scanning the
// serialized form misses secrets that escapes hide: the "n" of a "\n" escape
// removes a key's leading word boundary, and a key block's "\n" escapes fail
// its body check. A string that itself holds a JSON object or array, as tool
// input often does, is masked the same way. Text that is not a single valid
// JSON value is masked with Redact.
func RedactJSON(text string) string {
	if masked, ok := redactJSONValue(text); ok {
		return masked
	}
	return Redact(text)
}

// redactJSONValue reports false when text is not one valid JSON value.
func redactJSONValue(text string) (string, bool) {
	dec := jsontext.NewDecoder(strings.NewReader(text))
	var out []byte
	copied := 0 // bytes of text already written to out
	for {
		prevEnd := dec.InputOffset()
		tok, err := dec.ReadToken()
		if err != nil {
			// io.EOF after one complete value; anything else is not JSON.
			if dec.StackDepth() == 0 && prevEnd > 0 && strings.TrimSpace(text[prevEnd:]) == "" {
				break
			}
			return "", false
		}
		if tok.Kind() != '"' {
			continue
		}
		value := tok.String()
		masked := redactJSONString(value)
		if masked == value {
			continue
		}
		// Only whitespace, ',' and ':' separate the previous token from this
		// string, so its opening quote is the first '"' after prevEnd.
		end := dec.InputOffset()
		start := prevEnd + int64(strings.IndexByte(text[prevEnd:end], '"'))
		out = append(out, text[copied:start]...)
		out, err = jsontext.AppendQuote(out, masked)
		if err != nil {
			return "", false
		}
		copied = int(end)
	}
	if out == nil {
		return text, true
	}
	return string(append(out, text[copied:]...)), true
}

func redactJSONString(value string) string {
	trimmed := strings.TrimSpace(value)
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		if masked, ok := redactJSONValue(value); ok {
			return masked
		}
	}
	return Redact(value)
}

// RedactJSONLines masks a JSON Lines document line by line with RedactJSON.
// It applies when the first non-blank line is a JSON object or array, which
// is how provider session logs are written; other text, such as a Markdown
// history whose key blocks span lines, is masked whole with Redact.
func RedactJSONLines(text string) string {
	first := strings.TrimSpace(text)
	if i := strings.IndexByte(first, '\n'); i >= 0 {
		first = strings.TrimSpace(first[:i])
	}
	if !strings.HasPrefix(first, "{") && !strings.HasPrefix(first, "[") {
		return Redact(text)
	}
	if _, ok := redactJSONValue(first); !ok {
		return RedactJSON(text)
	}
	var b bytes.Buffer
	b.Grow(len(text))
	for line := range strings.SplitAfterSeq(text, "\n") {
		body := strings.TrimSuffix(line, "\n")
		b.WriteString(RedactJSON(body))
		if len(body) < len(line) {
			b.WriteByte('\n')
		}
	}
	return b.String()
}
