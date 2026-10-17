package codexcloud

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
)

const redacted = "[redacted]"

// Token holds a ChatGPT access token. Every formatting path prints
// "[redacted]"; only transports call Reveal.
type Token struct{ value string }

// NewToken wraps a raw token.
func NewToken(value string) Token { return Token{value: value} }

// Reveal returns the raw token for a request header or subprotocol.
func (t Token) Reveal() string { return t.value }

// Empty reports whether no token is held.
func (t Token) Empty() bool { return strings.TrimSpace(t.value) == "" }

func (Token) String() string   { return redacted }
func (Token) GoString() string { return redacted }

// Format covers verbs that skip String, such as %d and %t, which would
// otherwise print the struct's field.
func (Token) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, redacted) }

// MarshalJSON keeps the token out of any JSON encoding.
func (Token) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }

// MarshalText keeps the token out of text encodings such as TOML.
func (Token) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// Redact replaces every raw, query-escaped, and path-escaped form of each
// token in err's message. The result does not wrap err, because the
// original chain still carries the token.
func Redact(err error, tokens ...Token) error {
	if err == nil {
		return nil
	}
	return errors.New(RedactString(err.Error(), tokens...))
}

// RedactString replaces every form of each token in text.
func RedactString(text string, tokens ...Token) string {
	for _, tok := range tokens {
		if tok.Empty() {
			continue
		}
		for _, form := range []string{tok.value, url.QueryEscape(tok.value), url.PathEscape(tok.value)} {
			text = strings.ReplaceAll(text, form, redacted)
		}
	}
	return text
}
