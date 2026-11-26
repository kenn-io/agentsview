package db

import (
	"strings"

	"github.com/tdewolff/parse/v2"
	"github.com/tdewolff/parse/v2/js"
)

type shellCall struct {
	command, workdir        string
	unknownWorkdir, unknown bool
	possible                bool
}

type codeModeReader struct {
	calls             []shellCall
	locals            map[string]string
	possible, stopped bool
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
	for _, stmt := range ast.List {
		if r.stopped {
			break
		}
		r.stmt(stmt)
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

func (r *codeModeReader) stmt(stmt js.IStmt) {
	if stmt == nil {
		return
	}
	switch n := stmt.(type) {
	case *js.BlockStmt:
		for _, s := range n.List {
			if r.stopped {
				break
			}
			r.stmt(s)
		}
	case *js.EmptyStmt, *js.FuncDecl:
	case *js.ExprStmt:
		r.expr(n.Value)
	case *js.VarDecl:
		for _, item := range n.List {
			if item.Default == nil {
				continue
			}
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
			r.expr(item.Default)
		}
	case *js.ThrowStmt:
		r.expr(n.Value)
		r.stopped = true
	case *js.IfStmt:
		r.expr(n.Cond)
		if r.stopped {
			return
		}
		value, known := codeModeCondition(n.Cond)
		old := r.possible
		r.possible = old || !known
		if !known || value {
			r.stmt(n.Body)
		}
		stopped := r.stopped
		r.stopped = false
		if !known || !value {
			r.stmt(n.Else)
		}
		stopped = stopped || r.stopped
		r.stopped = known && stopped
		r.possible = old || !known && stopped
	default:
		r.unsupported(stmt)
	}
}

func (r *codeModeReader) expr(expr js.IExpr) {
	if expr == nil {
		return
	}
	switch n := expr.(type) {
	case *js.GroupExpr:
		r.expr(n.X)
	case *js.UnaryExpr:
		if n.Op == js.AwaitToken || n.Op == js.NotToken {
			r.expr(n.X)
		} else {
			r.unsupported(n)
		}
	case *js.BinaryExpr:
		if n.Op == js.AndToken || n.Op == js.OrToken {
			r.expr(n.X)
			if r.stopped {
				return
			}
			value, known := codeModeCondition(n.X)
			if !known || n.Op == js.AndToken && value || n.Op == js.OrToken && !value {
				old := r.possible
				r.possible = old || !known
				r.expr(n.Y)
				r.possible = old || !known && r.stopped
				if !known {
					r.stopped = false
				}
			}
		} else {
			r.unsupported(n)
		}
	case *js.CallExpr:
		if name, ok := n.X.(*js.Var); ok {
			if string(name.Name()) == "exit" {
				r.stopped = true
				return
			}
			if body, exists := r.locals[string(name.Name())]; exists {
				r.calls = append(r.calls, shellCall{command: body + " " + n.String(), unknown: true})
				return
			}
		}
		if access, ok := n.X.(*js.IndexExpr); ok && access.X.String() == "tools" {
			if _, literal := codeModeLiteral(access.Y); !literal {
				r.unsupported(n)
				return
			}
		}
		if isCodeModeExec(n.X) {
			call := codeModeCall(n)
			call.possible = r.possible
			r.calls = append(r.calls, call)
			return
		}
		if isEagerBatch(n.X) {
			if len(n.Args.List) != 1 || n.Args.List[0].Rest {
				r.unsupported(n)
				return
			}
			array, ok := n.Args.List[0].Value.(*js.ArrayExpr)
			if !ok {
				r.unsupported(n)
				return
			}
			for _, item := range array.List {
				if item.Spread || item.Value == nil {
					r.unsupported(n)
					return
				}
			}
			for _, item := range array.List {
				r.expr(item.Value)
			}
			return
		}
		if dot, ok := n.X.(*js.DotExpr); ok && (dot.Y.String() == "then" || dot.Y.String() == "map") {
			r.unsupported(n)
			return
		}
		if isCodeModeFunction(n.X) {
			r.unsupported(n)
			return
		}
		for _, arg := range n.Args.List {
			if arg.Rest {
				r.unsupported(n)
				return
			}
			r.expr(arg.Value)
		}
	case *js.LiteralExpr, *js.Var, *js.TemplateExpr:
	default:
		r.unsupported(expr)
	}
}

func codeModeCondition(expr js.IExpr) (bool, bool) {
	switch expr.String() {
	case "true":
		return true, true
	case "false":
		return false, true
	}
	return false, false
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

func codeModeCall(call *js.CallExpr) shellCall {
	result := shellCall{}
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

func (r *codeModeReader) unsupported(node js.INode) {
	v := &unsupportedVisitor{locals: r.locals}
	js.Walk(v, node)
	if v.risky {
		r.calls = append(r.calls, shellCall{command: node.String() + " " + v.text.String(), unknown: true})
	}
}
