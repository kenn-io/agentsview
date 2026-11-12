package secrets

import (
	"bytes"
	"encoding/json/jsontext"
	"errors"
	"io"
	"strings"
)

// jsonScanOptions accept JSON that agents really write: duplicate keys, which
// tool arguments can carry, and invalid UTF-8 inside strings.
var jsonScanOptions = []jsontext.Options{
	jsontext.AllowDuplicateNames(true),
	jsontext.AllowInvalidUTF8(true),
}

// RedactJSON masks secrets inside the decoded strings of a JSON document and
// returns the document with only the changed strings re-encoded. Scanning the
// serialized form misses secrets that escapes hide: the "n" of a "\n" escape
// removes a key's leading word boundary, and a key block's "\n" escapes fail
// its body check. A string that itself holds a JSON object or array, as tool
// input often does, is masked the same way.
//
// Text that stops being valid JSON partway, such as a record cut off
// mid-write, keeps the masking of every complete string before the break, and
// the rest is masked with Redact. Text that is not JSON at all is masked with
// Redact.
func RedactJSON(text string) string {
	dec := jsontext.NewDecoder(strings.NewReader(text), jsonScanOptions...)
	var out []byte
	copied := 0 // bytes of text already written to out
	for {
		prevEnd := dec.InputOffset()
		tok, err := dec.ReadToken()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return string(out) + Redact(text[copied:])
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
		quoted, err := jsontext.AppendQuote(nil, masked)
		if err != nil {
			return string(out) + Redact(text[copied:])
		}
		out = append(append(out, text[copied:start]...), quoted...)
		copied = int(end)
	}
	if out == nil {
		return text
	}
	return string(append(out, text[copied:]...))
}

func redactJSONString(value string) string {
	trimmed := strings.TrimSpace(value)
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		return RedactJSON(value)
	}
	return Redact(value)
}

// isJSONValue reports whether text is exactly one JSON value.
func isJSONValue(text string) bool {
	dec := jsontext.NewDecoder(strings.NewReader(text), jsonScanOptions...)
	if _, err := dec.ReadValue(); err != nil {
		return false
	}
	_, err := dec.ReadToken()
	return errors.Is(err, io.EOF)
}

// RedactJSONLines masks a JSON Lines document line by line with RedactJSON.
// It applies when the first non-blank line is a complete JSON value, which is
// how provider session logs are written. A document that starts with "{" or
// "[" but spans lines, such as pretty-printed JSON, is masked whole with
// RedactJSON. Other text, such as a Markdown history whose key blocks span
// lines, is masked whole with Redact.
func RedactJSONLines(text string) string {
	first := strings.TrimSpace(text)
	if i := strings.IndexByte(first, '\n'); i >= 0 {
		first = strings.TrimSpace(first[:i])
	}
	if !strings.HasPrefix(first, "{") && !strings.HasPrefix(first, "[") {
		return Redact(text)
	}
	if !isJSONValue(first) {
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
