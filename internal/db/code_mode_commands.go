package db

import (
	"strings"

	"github.com/tdewolff/parse/v2"
	"github.com/tdewolff/parse/v2/js"
	"go.kenn.io/agentsview/internal/parser"
)

type shellCall struct {
	command, workdir        string
	unknownWorkdir, unknown bool
	reach                   parser.Reach
}

type codeModeReader struct {
	calls  []shellCall
	locals map[string]string
	stops  int
}

func codeModeLiteral(expr js.IExpr) (string, bool) {
	if template, ok := expr.(*js.TemplateExpr); ok && template.Tag == nil && len(template.List) == 0 {
		return decodeCommandLiteral(string(template.Tail))
	}
	literal, ok := expr.(*js.LiteralExpr)
	if !ok || literal.TokenType != js.StringToken {
		return "", false
	}
	return decodeCommandLiteral(string(literal.Data))
}

// codeModeCalls reads the supported eager expressions in evaluation order.
func codeModeCalls(input string) ([]shellCall, bool) {
	ast, err := js.Parse(parse.NewInputString(input), js.Options{})
	if err != nil {
		return nil, false
	}
	r := codeModeReader{locals: make(map[string]string)}
	js.Walk(localFunctionVisitor(r.locals), ast)
	reach := parser.Reached
	for _, stmt := range ast.List {
		reach = r.stmt(stmt, reach)
	}
	return r.calls, true
}

// localFunctionVisitor indexes declarations without resolving scopes or calls.
type localFunctionVisitor map[string]string

func (v localFunctionVisitor) Enter(node js.INode) js.IVisitor {
	if fn, ok := node.(*js.FuncDecl); ok && fn.Name != nil {
		v[string(fn.Name.Name())] = fn.String()
	}
	return v
}
func (localFunctionVisitor) Exit(js.INode) {}

func (r *codeModeReader) stmt(stmt js.IStmt, reach parser.Reach) parser.Reach {
	if stmt == nil || reach == parser.Dead {
		return reach
	}
	switch n := stmt.(type) {
	case *js.BlockStmt:
		for _, s := range n.List {
			reach = r.stmt(s, reach)
		}
		return reach
	case *js.EmptyStmt, *js.FuncDecl:
		return reach
	case *js.ExprStmt:
		return r.expr(n.Value, reach)
	case *js.VarDecl:
		for _, item := range n.List {
			if item.Default != nil {
				if isCodeModeExec(item.Default) {
					if name, ok := item.Binding.(*js.Var); ok {
						r.locals[string(name.Name())] = item.Default.String()
					}
					continue
				}
				switch item.Default.(type) {
				case *js.FuncDecl, *js.ArrowFunc:
					if name, ok := item.Binding.(*js.Var); ok {
						r.locals[string(name.Name())] = item.Default.String()
					}
					continue
				}
				reach = r.expr(item.Default, reach)
			}
		}
		return reach
	case *js.ThrowStmt:
		r.expr(n.Value, reach)
		r.stops++
		return parser.Dead
	case *js.IfStmt:
		reach = r.expr(n.Cond, reach)
		if reach == parser.Dead {
			return reach
		}
		if value, ok := codeModeBool(n.Cond); ok {
			if value {
				return r.stmt(n.Body, reach)
			}
			if n.Else != nil {
				return r.stmt(n.Else, reach)
			}
			return reach
		}
		before := r.stops
		yes := r.stmt(n.Body, parser.Possible)
		no := parser.Possible
		if n.Else != nil {
			no = r.stmt(n.Else, parser.Possible)
		}
		if yes == parser.Dead && no == parser.Dead {
			return parser.Dead
		}
		if yes == parser.Dead || no == parser.Dead || r.stops > before {
			return parser.Possible
		}
		return reach
	default:
		return r.unsupported(stmt, reach)
	}
}

func (r *codeModeReader) expr(expr js.IExpr, reach parser.Reach) parser.Reach {
	if expr == nil || reach == parser.Dead {
		return reach
	}
	switch n := expr.(type) {
	case *js.GroupExpr:
		return r.expr(n.X, reach)
	case *js.UnaryExpr:
		if n.Op == js.AwaitToken || n.Op == js.NotToken {
			return r.expr(n.X, reach)
		}
		return r.unsupported(n, reach)
	case *js.BinaryExpr:
		if n.Op != js.AndToken && n.Op != js.OrToken {
			return r.unsupported(n, reach)
		}
		reach = r.expr(n.X, reach)
		if reach == parser.Dead {
			return reach
		}
		if value, ok := codeModeBool(n.X); ok {
			if n.Op == js.AndToken && value || n.Op == js.OrToken && !value {
				return r.expr(n.Y, reach)
			}
			return reach
		}
		before := r.stops
		after := r.expr(n.Y, parser.Possible)
		if after == parser.Dead || r.stops > before {
			return parser.Possible
		}
		return reach
	case *js.CallExpr:
		if name, ok := n.X.(*js.Var); ok {
			if body, exists := r.locals[string(name.Name())]; exists {
				r.stops++
				r.calls = append(r.calls, shellCall{command: body + " " + n.String(), unknown: true, reach: parser.Possible})
				return parser.Possible
			}
			if string(name.Name()) == "exit" {
				r.stops++
				return parser.Dead
			}
		}
		if access, ok := n.X.(*js.IndexExpr); ok && access.X.String() == "tools" {
			if _, literal := codeModeLiteral(access.Y); !literal {
				return r.unsupported(n, reach)
			}
		}
		if isCodeModeExec(n.X) {
			r.calls = append(r.calls, codeModeCall(n, reach))
			return reach
		}
		if isEagerBatch(n.X) {
			if len(n.Args.List) != 1 || n.Args.List[0].Rest {
				return r.unsupported(n, reach)
			}
			array, ok := n.Args.List[0].Value.(*js.ArrayExpr)
			if !ok {
				return r.unsupported(n, reach)
			}
			for _, item := range array.List {
				if item.Spread || item.Value == nil {
					return r.unsupported(n, reach)
				}
			}
			for _, item := range array.List {
				reach = r.expr(item.Value, reach)
			}
			return reach
		}
		if dot, ok := n.X.(*js.DotExpr); ok && (dot.Y.String() == "then" || dot.Y.String() == "map") {
			return r.unsupported(n, reach)
		}
		if isCodeModeFunction(n.X) {
			return r.unsupported(n, reach)
		}
		for _, arg := range n.Args.List {
			if arg.Rest {
				return r.unsupported(n, reach)
			}
			reach = r.expr(arg.Value, reach)
		}
		return reach
	case *js.LiteralExpr, *js.Var, *js.TemplateExpr:
		return reach
	default:
		return r.unsupported(expr, reach)
	}
}

func isCodeModeFunction(expr js.IExpr) bool {
	for {
		group, ok := expr.(*js.GroupExpr)
		if !ok {
			break
		}
		expr = group.X
	}
	switch expr.(type) {
	case *js.FuncDecl, *js.ArrowFunc:
		return true
	}
	return false
}

func isCodeModeExec(expr js.IExpr) bool {
	switch x := expr.(type) {
	case *js.DotExpr:
		return x.X.String() == "tools" && x.Y.String() == "exec_command"
	case *js.IndexExpr:
		name, ok := codeModeLiteral(x.Y)
		return x.X.String() == "tools" && ok && name == "exec_command"
	}
	return false
}
func isEagerBatch(expr js.IExpr) bool {
	dot, ok := expr.(*js.DotExpr)
	return ok && dot.X.String() == "Promise" && (dot.Y.String() == "all" || dot.Y.String() == "allSettled")
}
func codeModeCall(call *js.CallExpr, reach parser.Reach) shellCall {
	result := shellCall{reach: reach}
	if len(call.Args.List) != 1 || call.Args.List[0].Rest {
		result.unknown = true
		result.command = call.String()
		return result
	}
	obj, ok := call.Args.List[0].Value.(*js.ObjectExpr)
	if !ok {
		result.unknown = true
		result.command = call.String()
		return result
	}
	for _, prop := range obj.List {
		if prop.Name == nil || prop.Name.IsComputed() || prop.Spread {
			result.unknown = true
			result.unknownWorkdir = true
			result.command = call.String()
			continue
		}
		name := strings.Trim(string(prop.Name.Literal.Data), `"'`)
		if name != "cmd" && name != "workdir" {
			continue
		}
		value, ok := codeModeLiteral(prop.Value)
		if name == "cmd" {
			if ok {
				result.command = value
			} else {
				result.unknown = true
				result.command = call.String()
			}
		}
		if name == "workdir" {
			result.workdir = value
			result.unknownWorkdir = !ok
		}
	}
	return result
}
func codeModeBool(expr js.IExpr) (bool, bool) {
	switch n := expr.(type) {
	case *js.GroupExpr:
		return codeModeBool(n.X)
	case *js.UnaryExpr:
		if n.Op == js.NotToken {
			value, ok := codeModeBool(n.X)
			return !value, ok
		}
	case *js.Var:
		if string(n.Name()) == "undefined" {
			return false, true
		}
	case *js.LiteralExpr:
		switch n.TokenType {
		case js.TrueToken:
			return true, true
		case js.FalseToken, js.NullToken:
			return false, true
		case js.IntegerToken, js.DecimalToken:
			if string(n.Data) == "0" {
				return false, true
			}
			if string(n.Data) == "1" {
				return true, true
			}
		case js.StringToken:
			value, ok := codeModeLiteral(n)
			return value != "", ok
		}
	}
	return false, false
}

// unsupportedVisitor finds only constructs that may hide a launch or stop.
type unsupportedVisitor struct {
	risky  bool
	locals map[string]string
	text   strings.Builder
}

func (v *unsupportedVisitor) Enter(node js.INode) js.IVisitor {
	switch n := node.(type) {
	case *js.FuncDecl:
		return nil
	case *js.ForStmt, *js.ForInStmt, *js.ForOfStmt, *js.WhileStmt, *js.DoWhileStmt, *js.TryStmt, *js.SwitchStmt, *js.ThrowStmt:
		v.risky = true
	case *js.CallExpr:
		if isCodeModeFunction(n.X) {
			v.risky = true
		}
		if isCodeModeExec(n.X) {
			v.risky = true
		}
		if name, ok := n.X.(*js.Var); ok {
			if string(name.Name()) == "exit" {
				v.risky = true
			}
			if body, ok := v.locals[string(name.Name())]; ok {
				v.risky = true
				v.text.WriteString(body)
			}
		}
		if index, ok := n.X.(*js.IndexExpr); ok && index.X.String() == "tools" {
			if _, ok := codeModeLiteral(index.Y); !ok {
				v.risky = true
			}
		}
	}
	return v
}
func (*unsupportedVisitor) Exit(js.INode) {}
func (r *codeModeReader) unsupported(node js.INode, reach parser.Reach) parser.Reach {
	v := &unsupportedVisitor{locals: r.locals}
	js.Walk(v, node)
	if !v.risky {
		return reach
	}
	r.calls = append(r.calls, shellCall{command: node.String() + " " + v.text.String(), unknown: true, reach: parser.Possible})
	r.stops++
	return parser.Possible
}
