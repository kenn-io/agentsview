package db

import (
	"errors"
	"strings"
)

// FTSExpr is a parsed FTS query. A term node holds a literal word or phrase in
// Term and its source text, quotes and prefix marker included, in Raw; an AND,
// OR, or NOT node combines Left and Right, where NOT means Left and not Right.
// Implicit AND between adjacent operands binds tighter than NOT, which binds
// tighter than AND, which binds tighter than OR.
type FTSExpr struct {
	Op          string
	Term, Raw   string
	Left, Right *FTSExpr
}

// ParseFTSExpression parses the PrepareFTSQuery form of pattern into its
// Boolean structure. Unquoted input becomes an implicit AND of literal words.
// It returns nil when the query has no non-blank operand, and a
// SearchInputError when the quotes or Boolean expression are malformed.
func ParseFTSExpression(pattern string) (*FTSExpr, error) {
	tokens, err := ftsTokens(pattern)
	if err != nil {
		return nil, err
	}
	if len(tokens) == 0 {
		return nil, nil
	}
	p := ftsParser{tokens: tokens}
	expr, err := p.expression(1)
	if err != nil || p.pos != len(tokens) {
		return nil, &SearchInputError{Msg: "search: invalid FTS query: invalid Boolean expression"}
	}
	return expr, nil
}

func ftsTokens(pattern string) ([]ftsToken, error) {
	input := PrepareFTSQuery(pattern)
	var tokens []ftsToken
	for i := 0; i < len(input); {
		if strings.ContainsRune(" \t\n\r", rune(input[i])) {
			i++
			continue
		}
		if input[i] == '(' || input[i] == ')' {
			tokens = append(tokens, ftsToken{kind: string(input[i])})
			i++
			continue
		}
		if input[i] != '"' {
			start := i
			for i < len(input) && !strings.ContainsRune(" \t\n\r()\"", rune(input[i])) {
				i++
			}
			word := input[start:i]
			if word != "AND" && word != "OR" && word != "NOT" {
				tokens = append(tokens, ftsToken{kind: "term", text: word, raw: word})
			} else {
				tokens = append(tokens, ftsToken{kind: word})
			}
			continue
		}
		start := i
		i++
		var term strings.Builder
		closed := false
		for i < len(input) {
			if input[i] == '"' {
				i++
				if i < len(input) && input[i] == '"' {
					term.WriteByte('"')
					i++
					continue
				}
				closed = true
				break
			}
			term.WriteByte(input[i])
			i++
		}
		if !closed {
			return nil, &SearchInputError{Msg: "search: invalid FTS query: invalid quoted term"}
		}
		// Keep prefix markers literal, as in the other substring search paths.
		for i < len(input) && input[i] == '*' {
			term.WriteByte(input[i])
			i++
		}
		if strings.TrimSpace(term.String()) != "" {
			tokens = append(tokens, ftsToken{kind: "term", text: term.String(), raw: input[start:i]})
		}
	}
	return tokens, nil
}

type ftsToken struct{ kind, text, raw string }

type ftsParser struct {
	tokens []ftsToken
	pos    int
}

func (p *ftsParser) atom() (*FTSExpr, error) {
	if p.pos >= len(p.tokens) {
		return nil, errors.New("missing term")
	}
	tok := p.tokens[p.pos]
	p.pos++
	switch tok.kind {
	case "term":
		return &FTSExpr{Op: "term", Term: tok.text, Raw: tok.raw}, nil
	case "(":
		expr, err := p.expression(1)
		if err != nil || p.pos >= len(p.tokens) || p.tokens[p.pos].kind != ")" {
			return nil, errors.New("unclosed group")
		}
		p.pos++
		return expr, nil
	default:
		return nil, errors.New("expected term")
	}
}

func (p *ftsParser) expression(minPrec int) (*FTSExpr, error) {
	left, err := p.atom()
	if err != nil {
		return nil, err
	}
	for p.pos < len(p.tokens) {
		op := p.tokens[p.pos].kind
		prec, implicit := 0, false
		switch op {
		case "OR":
			prec = 1
		case "AND":
			prec = 2
		case "NOT":
			prec = 3
		case "term":
			prec = 4
			implicit = true
			op = "AND"
		}
		if prec < minPrec {
			break
		}
		if !implicit {
			p.pos++
		}
		right, err := p.expression(prec + 1)
		if err != nil {
			return nil, err
		}
		left = &FTSExpr{Op: op, Left: left, Right: right}
	}
	return left, nil
}

// matchedTerms returns the positive terms through which e holds, in query
// order, given whether each term holds on its own. It returns nil when e does
// not hold. Every term of an OR side that holds is included, so the caller can
// skip a term it cannot locate in the text.
func (e *FTSExpr) matchedTerms(holds func(*FTSExpr) bool) []*FTSExpr {
	switch e.Op {
	case "term":
		if holds(e) {
			return []*FTSExpr{e}
		}
		return nil
	case "OR":
		return append(e.Left.matchedTerms(holds), e.Right.matchedTerms(holds)...)
	}
	left := e.Left.matchedTerms(holds)
	if left == nil {
		return nil
	}
	right := e.Right.matchedTerms(holds)
	if e.Op == "NOT" {
		if right != nil {
			return nil
		}
		return left
	}
	if right == nil {
		return nil
	}
	return append(left, right...)
}

// positiveTerms returns e's terms in query order, omitting the negated side
// of every NOT.
func (e *FTSExpr) positiveTerms() []*FTSExpr {
	switch e.Op {
	case "term":
		return []*FTSExpr{e}
	case "NOT":
		return e.Left.positiveTerms()
	}
	return append(e.Left.positiveTerms(), e.Right.positiveTerms()...)
}

// terms returns every term of e in query order, including negated ones.
func (e *FTSExpr) terms() []*FTSExpr {
	if e.Op == "term" {
		return []*FTSExpr{e}
	}
	return append(e.Left.terms(), e.Right.terms()...)
}

// hasOrNot reports whether e uses OR or NOT, the operators that let a row
// match without every term.
func (e *FTSExpr) hasOrNot() bool {
	if e.Op == "term" {
		return false
	}
	return e.Op == "OR" || e.Op == "NOT" || e.Left.hasOrNot() || e.Right.hasOrNot()
}

// sqliteProbeable reports whether SQLite can match each term of e on its own
// by its source text. That holds for quoted phrases and plain FTS5 barewords,
// each with an optional prefix marker. Other syntax, such as column filters,
// '+' phrase joins, and '^' anchors, is not modeled by this parser.
func (e *FTSExpr) sqliteProbeable() bool {
	for _, t := range e.terms() {
		if strings.HasPrefix(t.Raw, `"`) {
			continue
		}
		word := strings.TrimSuffix(t.Raw, "*")
		if word == "" || strings.ContainsFunc(word, func(r rune) bool {
			return r < 0x80 && r != '_' && r != 0x1A &&
				(r < '0' || r > '9') && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z')
		}) {
			return false
		}
	}
	return true
}
