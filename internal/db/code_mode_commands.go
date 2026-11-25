package db

import (
	"strconv"
	"strings"

	"github.com/tdewolff/parse/v2"
	"github.com/tdewolff/parse/v2/js"
)

// codeModeCommand is one shell call embedded in an archived JavaScript cell.
type codeModeCommand struct {
	command, workdir          string
	uncertain, unknownWorkdir bool
}

type codeModeVisitor struct {
	calls       *[]codeModeCommand
	conditional bool
}

func (v *codeModeVisitor) Enter(node js.INode) js.IVisitor {
	switch n := node.(type) {
	case *js.IfStmt, *js.ForStmt, *js.ForInStmt, *js.ForOfStmt, *js.WhileStmt, *js.DoWhileStmt, *js.FuncDecl, *js.ArrowFunc, *js.MethodDecl, *js.CondExpr, *js.SwitchStmt, *js.CaseClause, *js.TryStmt:
		return &codeModeVisitor{calls: v.calls, conditional: true}
	case *js.BinaryExpr:
		if n.Op == js.AndToken || n.Op == js.OrToken || n.Op == js.NullishToken {
			return &codeModeVisitor{calls: v.calls, conditional: true}
		}
	case *js.CallExpr:
		dot, ok := n.X.(*js.DotExpr)
		if !ok || dot.X.String() != "tools" || dot.Y.String() != "exec_command" {
			return v
		}
		call := codeModeCommand{uncertain: v.conditional}
		if len(n.Args.List) != 1 || n.Args.List[0].Rest {
			call.uncertain = true
			*v.calls = append(*v.calls, call)
			return v
		}
		object, ok := n.Args.List[0].Value.(*js.ObjectExpr)
		if !ok {
			call.uncertain = true
			*v.calls = append(*v.calls, call)
			return v
		}
		for _, prop := range object.List {
			if prop.Name == nil || prop.Name.IsComputed() || prop.Spread {
				call.uncertain = true
				call.unknownWorkdir = true
				continue
			}
			name := strings.Trim(string(prop.Name.Literal.Data), `"'`)
			if name != "cmd" && name != "workdir" {
				continue
			}
			value, ok := codeModeLiteral(prop.Value)
			if name == "cmd" {
				if ok {
					call.command = value
				} else {
					call.command = ""
					call.uncertain = true
				}
			}
			if name == "workdir" {
				call.workdir = value
				call.unknownWorkdir = !ok
			}
		}
		*v.calls = append(*v.calls, call)
	}
	return v
}
func (*codeModeVisitor) Exit(js.INode) {}

func codeModeLiteral(expr js.IExpr) (string, bool) {
	if template, ok := expr.(*js.TemplateExpr); ok && template.Tag == nil && len(template.List) == 0 {
		raw := string(template.Tail)
		if len(raw) >= 2 && !strings.Contains(raw, `\`) {
			return raw[1 : len(raw)-1], true
		}
		return "", false
	}
	literal, ok := expr.(*js.LiteralExpr)
	if !ok || literal.TokenType != js.StringToken {
		return "", false
	}
	raw := string(literal.Data)
	if len(raw) < 2 {
		return "", false
	}
	var decoded strings.Builder
	for rest := raw[1 : len(raw)-1]; rest != ""; {
		r, _, tail, err := strconv.UnquoteChar(rest, raw[0])
		if err != nil {
			return "", false
		}
		decoded.WriteRune(r)
		rest = tail
	}
	return decoded.String(), true
}

// codeModeCommands returns each statically described exec_command call.
// A conditional or dynamically described call can block inference but cannot
// establish a parent.
func codeModeCommands(input string) ([]codeModeCommand, bool) {
	ast, err := js.Parse(parse.NewInputString(input), js.Options{})
	if err != nil {
		return nil, false
	}
	var calls []codeModeCommand
	js.Walk(&codeModeVisitor{calls: &calls}, ast)
	return calls, len(calls) > 0
}
