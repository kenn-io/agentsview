package parser

import (
	"path"
	"regexp"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// OneShotRun reports whether argv starts a one-shot agent run.
func OneShotRun(agent string, args []string) bool {
	found, _ := oneShotRunState(agent, args)
	return found
}

func oneShotRunState(agent string, args []string) (bool, bool) {
	for i := 1; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return false, true
		}
		if agent == "codex" && (arg == "exec" || arg == "e") || agent == "claude" && (arg == "-p" || arg == "--print") {
			return true, false
		}
		if !strings.HasPrefix(arg, "-") {
			if agent == "codex" {
				return false, true
			}
			continue
		}
		if !strings.Contains(arg, "=") {
			if agent == "codex" {
				switch arg {
				case "-C", "--cd", "-c", "--config", "-m", "--model", "-p", "--profile", "-s", "--sandbox", "-a", "--ask-for-approval", "-i", "--image", "--add-dir", "--enable", "--disable", "--remote", "--remote-auth-token-env", "--local-provider":
					i++
				}
			} else {
				switch arg {
				case "--model", "--add-dir", "--append-system-prompt", "--append-system-prompt-file", "--system-prompt", "--system-prompt-file", "--agent", "--agents", "--settings", "--setting-sources", "--session-id", "--permission-mode", "--output-format", "--input-format", "--json-schema", "--effort", "--fallback-model", "--name", "-n", "--max-budget-usd", "--mcp-config", "--plugin-dir", "--tools", "--allowedTools", "--allowed-tools", "--disallowedTools", "--disallowed-tools":
					i++
				}
			}
		}
	}
	return false, false
}

// LaunchCommand records a possible or certain launch and its working directory.
type LaunchCommand struct {
	Agent, Dir string
	Certain    bool
}

// LaunchesFromCommand finds possible agent launches at executable positions.
func LaunchesFromCommand(command, baseDir string) []LaunchCommand {
	file, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil {
		var out []LaunchCommand
		for _, agent := range []string{"codex", "claude"} {
			if regexp.MustCompile(`(?i)(^|[^a-z0-9_])` + agent + `([^a-z0-9_]|$)`).MatchString(command) {
				out = append(out, LaunchCommand{Agent: agent})
			}
		}
		return out
	}
	var out []LaunchCommand
	dir := baseDir
	var visit func(*syntax.Stmt, bool)
	visit = func(stmt *syntax.Stmt, trusted bool) {
		before := dir
		if stmt.Background {
			defer func() { dir = before }()
		}
		switch call := stmt.Cmd.(type) {
		case *syntax.BinaryCmd:
			switch call.Op {
			case syntax.AndStmt:
				visit(call.X, true)
				start := len(out)
				visit(call.Y, true)
				if !isLiteralCD(call.X) {
					for i := start; i < len(out); i++ {
						out[i].Certain = false
					}
				}
				if !trusted && dir != before {
					dir = ""
				}
			case syntax.OrStmt:
				visit(call.X, false)
				dir = ""
				visit(call.Y, false)
				dir = ""
			case syntax.Pipe, syntax.PipeAll:
				visit(call.X, false)
				dir = before
				visit(call.Y, false)
				dir = before
			}
		case *syntax.CallExpr:
			args := make([]string, len(call.Args))
			dynamic := make([]bool, len(call.Args))
			for i, w := range call.Args {
				var ok bool
				args[i], ok = launchWord(w.Parts)
				dynamic[i] = !ok
				if !ok && len(w.Parts) > 0 {
					if lit, yes := w.Parts[0].(*syntax.Lit); yes && lit.Value == "--cd=" {
						args[i] = "--cd="
					}
				}
				if !ok {
					syntax.Walk(w, func(n syntax.Node) bool {
						if sub, ok := n.(*syntax.CmdSubst); ok {
							saved, start := dir, len(out)
							dir = ""
							for _, st := range sub.Stmts {
								visit(st, false)
							}
							for i := start; i < len(out); i++ {
								out[i].Dir = ""
								out[i].Certain = false
							}
							dir = saved
							return false
						}
						return true
					})
				}
			}
			if len(args) == 0 {
				return
			}
			program := launchProgram(args[0])
			if program == "cd" || program == "set-location" {
				if !trusted || len(args) != 2 || dynamic[1] || args[1] == "-" {
					dir = ""
				} else {
					dir = launchWorkdir(dir, args[1])
				}
				return
			}
			if (program == "bash" || program == "sh" || program == "zsh") && len(args) >= 3 && (args[1] == "-c" || args[1] == "-lc") && !dynamic[2] {
				out = append(out, LaunchesFromCommand(args[2], dir)...)
				return
			}
			wrappers := map[string]bool{"sudo": true, "doas": true, "env": true, "command": true, "exec": true, "nohup": true, "time": true, "nice": true, "timeout": true, "stdbuf": true, "setsid": true, "xargs": true, "eval": true, "bash": true, "sh": true, "zsh": true}
			if wrappers[program] {
				if program == "eval" && len(args) == 2 && !dynamic[1] {
					for _, nested := range LaunchesFromCommand(args[1], "") {
						nested.Certain = false
						out = append(out, nested)
					}
				}
				for i := 1; i < len(args); i++ {
					for _, agent := range []string{"codex", "claude"} {
						if launchProgram(args[i]) == agent || dynamic[i] && (i == 1 || i+1 < len(args) || program == "eval" || (program == "bash" || program == "sh" || program == "zsh") && (args[i-1] == "-c" || args[i-1] == "-lc")) {
							out = append(out, LaunchCommand{Agent: agent})
						}
					}
				}
				return
			}
			if dynamic[0] {
				if len(args) > 1 {
					for _, agent := range []string{"codex", "claude"} {
						out = append(out, LaunchCommand{Agent: agent})
					}
				}
				return
			}
			if program != "codex" && program != "claude" {
				return
			}
			workdir := dir
			modeAt := -1
			uncertain := false
			for i := 1; i < len(args); i++ {
				if program == "codex" && (args[i] == "-C" || args[i] == "--cd") && i+1 < len(args) {
					i++
					if dynamic[i] {
						workdir = ""
					} else {
						workdir = launchWorkdir(workdir, args[i])
					}
					continue
				}
				if program == "codex" && strings.HasPrefix(args[i], "--cd=") {
					if dynamic[i] {
						workdir = ""
					} else {
						workdir = launchWorkdir(workdir, strings.TrimPrefix(args[i], "--cd="))
					}
					continue
				}
				if dynamic[i] {
					if modeAt < 0 {
						uncertain = true
					}
					continue
				}
				if modeAt < 0 {
					found, nonLaunch := oneShotRunState(program, args[:i+1])
					if found {
						modeAt = i
					} else if nonLaunch && !uncertain {
						return
					}
				}
			}
			if modeAt >= 0 {
				out = append(out, LaunchCommand{Agent: program, Dir: workdir, Certain: !uncertain})
				return
			}
			if uncertain {
				out = append(out, LaunchCommand{Agent: program})
			}
		default:
			syntax.Walk(stmt.Cmd, func(n syntax.Node) bool {
				if st, ok := n.(*syntax.Stmt); ok {
					saved, start := dir, len(out)
					dir = ""
					visit(st, false)
					for i := start; i < len(out); i++ {
						out[i].Dir = ""
						out[i].Certain = false
					}
					dir = saved
					return false
				}
				return true
			})
		}
	}
	for _, stmt := range file.Stmts {
		visit(stmt, false)
	}
	return out
}

func isLiteralCD(stmt *syntax.Stmt) bool {
	call, ok := stmt.Cmd.(*syntax.CallExpr)
	if !ok || len(call.Args) != 2 {
		return false
	}
	name, ok := launchWord(call.Args[0].Parts)
	if !ok || (name != "cd" && !strings.EqualFold(name, "set-location")) {
		return false
	}
	dir, ok := launchWord(call.Args[1].Parts)
	return ok && dir != "" && dir != "-" && !strings.HasPrefix(dir, "~")
}

func launchProgram(name string) string {
	name = strings.ToLower(path.Base(strings.ReplaceAll(name, `\`, "/")))
	return strings.TrimSuffix(strings.TrimSuffix(name, ".exe"), ".cmd")
}

func launchWord(parts []syntax.WordPart) (string, bool) {
	var out strings.Builder
	for _, part := range parts {
		switch p := part.(type) {
		case *syntax.Lit:
			out.WriteString(p.Value)
		case *syntax.SglQuoted:
			if p.Dollar {
				return "", false
			}
			out.WriteString(p.Value)
		case *syntax.DblQuoted:
			s, ok := launchWord(p.Parts)
			if !ok || p.Dollar {
				return "", false
			}
			out.WriteString(s)
		default:
			return "", false
		}
	}
	return out.String(), true
}

func launchWorkdir(base, dir string) string {
	if strings.HasPrefix(dir, "~") {
		return ""
	}
	if base == "" && !strings.HasPrefix(dir, "/") && !(len(dir) >= 3 && dir[1] == ':' && (dir[2] == '/' || dir[2] == '\\')) {
		return ""
	}
	return ShellWorkdir(base, dir)
}
