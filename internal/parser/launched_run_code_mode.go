package parser

import (
	"encoding/json/v2"
	"strings"

	"github.com/tdewolff/parse/v2"
	"github.com/tdewolff/parse/v2/js"
)

func codeModeOutput(output string) string {
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal([]byte(output), &blocks) != nil || len(blocks) == 0 {
		return output
	}
	var text strings.Builder
	for _, block := range blocks {
		if block.Type != "input_text" && block.Type != "output_text" && block.Type != "text" {
			return output
		}
		text.WriteString(block.Text)
	}
	return text.String()
}

// codeModeCommand recognizes a single shell call whose output is displayed.
func codeModeCommand(input string) (string, bool) {
	if !strings.Contains(input, "tools.exec_command") {
		return "", false
	}
	tree, err := js.Parse(parse.NewInputString(input), js.Options{})
	if err != nil || len(tree.List) != 2 {
		return "", false
	}
	decl, ok := tree.List[0].(*js.VarDecl)
	if !ok || decl.TokenType != js.ConstToken || len(decl.List) != 1 {
		return "", false
	}
	variable, ok := decl.List[0].Binding.(*js.Var)
	if !ok || string(variable.Data) == "tools" || string(variable.Data) == "text" {
		return "", false
	}
	await, ok := decl.List[0].Default.(*js.UnaryExpr)
	if !ok || await.Op != js.AwaitToken {
		return "", false
	}
	call, ok := await.X.(*js.CallExpr)
	if !ok || call.Optional || !codeModeDot(call.X, "tools", "exec_command") || len(call.Args.List) != 1 || call.Args.List[0].Rest {
		return "", false
	}
	object, ok := call.Args.List[0].Value.(*js.ObjectExpr)
	if !ok {
		return "", false
	}
	var encoded strings.Builder
	if object.JSON(&encoded) != nil {
		return "", false
	}
	var args struct {
		Cmd string `json:"cmd"`
	}
	if json.Unmarshal([]byte(encoded.String()), &args) != nil || args.Cmd == "" {
		return "", false
	}
	statement, ok := tree.List[1].(*js.ExprStmt)
	if !ok {
		return "", false
	}
	print, ok := statement.Value.(*js.CallExpr)
	if !ok || print.Optional || len(print.Args.List) != 1 || print.Args.List[0].Rest {
		return "", false
	}
	name, ok := print.X.(*js.Var)
	if !ok || string(name.Data) != "text" || !codeModeDot(print.Args.List[0].Value, string(variable.Data), "output") {
		return "", false
	}
	return args.Cmd, true
}

func codeModeDot(expr js.IExpr, base, member string) bool {
	dot, ok := expr.(*js.DotExpr)
	if !ok || dot.Optional {
		return false
	}
	x, ok := dot.X.(*js.Var)
	if !ok || string(x.Data) != base {
		return false
	}
	return dot.Y.String() == member
}
