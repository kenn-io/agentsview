package parser

import (
	"path"
	"regexp"
	"slices"
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
		if launchOptionValue(agent, arg) {
			i++
		}
	}
	return false, false
}

func launchOptionValue(agent, arg string) bool {
	switch agent {
	case "codex":
		switch arg {
		case "-C", "--cd", "-c", "--config", "-m", "--model", "-p", "--profile", "-s", "--sandbox", "-a", "--ask-for-approval", "-i", "--image", "--add-dir", "--enable", "--disable", "--remote", "--remote-auth-token-env", "--local-provider":
			return true
		}
	case "claude":
		switch arg {
		case "--model", "--add-dir", "--append-system-prompt", "--append-system-prompt-file", "--system-prompt", "--system-prompt-file", "--agent", "--agents", "--settings", "--setting-sources", "--session-id", "--permission-mode", "--output-format", "--input-format", "--json-schema", "--effort", "--fallback-model", "--name", "-n", "--max-budget-usd", "--mcp-config", "--plugin-dir", "--tools", "--allowedTools", "--allowed-tools", "--disallowedTools", "--disallowed-tools":
			return true
		}
	}
	return false
}

// LaunchCommand describes one possible agent launch.
type LaunchCommand struct {
	Agent   string
	Dirs    []string
	Unknown bool
}
type shellFlow struct {
	dirs       []string
	unknown    bool
	cwdChanged bool
}

func mergeShellFlows(a, b shellFlow) shellFlow {
	for _, dir := range b.dirs {
		if !slices.Contains(a.dirs, dir) {
			a.dirs = append(a.dirs, dir)
		}
	}
	a.unknown = a.unknown || b.unknown
	a.cwdChanged = a.cwdChanged || b.cwdChanged
	return a
}

// LaunchesFromCommand reads launch and directory evidence from shell syntax.
func LaunchesFromCommand(command, baseDir string) []LaunchCommand {
	file, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil {
		var out []LaunchCommand
		for _, agent := range []string{"codex", "claude"} {
			if regexp.MustCompile(`(?i)(^|[^a-z0-9_])` + agent + `([^a-z0-9_]|$)`).MatchString(command) {
				out = append(out, LaunchCommand{Agent: agent, Unknown: true})
			}
		}
		return out
	}
	initial := shellFlow{}
	if baseDir == "" {
		initial.unknown = true
	} else {
		initial.dirs = []string{baseDir}
	}
	var out []LaunchCommand
	var run func(*syntax.Stmt, shellFlow) shellFlow
	list := func(stmts []*syntax.Stmt, f shellFlow) shellFlow {
		for _, stmt := range stmts {
			f = run(stmt, f)
		}
		return f
	}
	emit := func(agent string, f shellFlow, uncertain bool) {
		if uncertain {
			f.unknown = true
			f.dirs = nil
		}
		out = append(out, LaunchCommand{Agent: agent, Dirs: slices.Clone(f.dirs), Unknown: f.unknown})
	}
	run = func(stmt *syntax.Stmt, f shellFlow) (result shellFlow) {
		f.dirs = slices.Clone(f.dirs)
		before := f
		before.dirs = slices.Clone(f.dirs)
		if stmt.Background {
			defer func() { result = before }()
		}
		switch c := stmt.Cmd.(type) {
		case *syntax.BinaryCmd:
			switch c.Op {
			case syntax.AndStmt, syntax.OrStmt:
				left := run(c.X, f)
				rhs := left
				if c.Op == syntax.AndStmt {
					if narrowed, ok := guardedLaunchDir(c.X, before); ok {
						rhs = narrowed
					}
				}
				right := run(c.Y, rhs)
				return mergeShellFlows(left, right)
			case syntax.Pipe, syntax.PipeAll:
				run(c.X, f)
				run(c.Y, f)
				return before
			}
		case *syntax.Subshell:
			list(c.Stmts, f)
			return f
		case *syntax.IfClause:
			cond := list(c.Cond, f)
			yes := list(c.Then, cond)
			no := cond
			if c.Else != nil {
				no = run(&syntax.Stmt{Cmd: c.Else}, cond)
			}
			return mergeShellFlows(yes, no)
		case *syntax.CallExpr:
			for _, assign := range c.Assigns {
				syntax.Walk(assign, func(n syntax.Node) bool {
					if sub, ok := n.(*syntax.CmdSubst); ok {
						list(sub.Stmts, f)
						return false
					}
					return true
				})
			}
			args := make([]string, len(c.Args))
			dynamic := make([]bool, len(c.Args))
			for i, w := range c.Args {
				var ok bool
				args[i], ok = launchWord(w.Parts)
				dynamic[i] = !ok
				if !ok && len(w.Parts) > 0 {
					if lit, yes := w.Parts[0].(*syntax.Lit); yes && strings.HasSuffix(lit.Value, "=") {
						args[i] = lit.Value
					}
				}
				if !ok {
					syntax.Walk(w, func(n syntax.Node) bool {
						if sub, yes := n.(*syntax.CmdSubst); yes {
							nested := f
							list(sub.Stmts, nested)
							return false
						}
						return true
					})
				}
			}
			if len(args) == 0 {
				return f
			}
			program := launchProgram(args[0])
			switch program {
			case "source", ".":
				fallthrough
			case "pushd", "popd":
				f.unknown = true
				f.cwdChanged = true
				return f
			case "cd", "set-location":
				f.cwdChanged = true
				if len(args) != 2 || dynamic[1] || args[1] == "-" || strings.HasPrefix(args[1], "~") {
					f.unknown = true
					return f
				}
				targets := resolveLaunchDirs(before.dirs, args[1])
				if len(targets) == 0 {
					f.unknown = true
				}
				for _, target := range targets {
					if !slices.Contains(f.dirs, target) {
						f.dirs = append(f.dirs, target)
					}
				}
				return f
			}
			if (program == "bash" || program == "sh" || program == "zsh") && len(args) >= 3 && (args[1] == "-c" || args[1] == "-lc") && !dynamic[2] {
				nested, err := syntax.NewParser().Parse(strings.NewReader(args[2]), "")
				if err == nil {
					list(nested.Stmts, f)
				} else {
					for _, agent := range []string{"codex", "claude"} {
						if strings.Contains(strings.ToLower(args[2]), agent) {
							emit(agent, f, true)
						}
					}
				}
				return f
			}
			wrappers := map[string]bool{"sudo": true, "doas": true, "env": true, "command": true, "exec": true, "nohup": true, "time": true, "nice": true, "timeout": true, "stdbuf": true, "setsid": true, "xargs": true, "eval": true, "bash": true, "sh": true, "zsh": true}
			if wrappers[program] {
				if program == "eval" && len(args) == 2 && !dynamic[1] {
					for _, nested := range LaunchesFromCommand(args[1], "") {
						u := f
						u.unknown = true
						u.dirs = nil
						emit(nested.Agent, u, true)
					}
				}
				for i := 1; i < len(args); i++ {
					if program != "eval" && program != "xargs" && program != "bash" && program != "sh" && program != "zsh" && !dynamic[i] && (launchProgram(args[i]) == "codex" || launchProgram(args[i]) == "claude") {
						run(&syntax.Stmt{Cmd: &syntax.CallExpr{Args: c.Args[i:]}}, f)
						return f
					}
					for _, agent := range []string{"codex", "claude"} {
						if launchProgram(args[i]) == agent || dynamic[i] && (i == 1 || i+1 < len(args) || program == "eval" || (program == "bash" || program == "sh" || program == "zsh") && (args[i-1] == "-c" || args[i-1] == "-lc")) {
							u := f
							u.unknown = true
							u.dirs = nil
							emit(agent, u, true)
						}
					}
				}
				if program == "eval" {
					f.unknown = true
					f.cwdChanged = true
				}
				return f
			}
			if dynamic[0] {
				if len(args) > 1 {
					u := f
					u.unknown = true
					u.dirs = nil
					emit("codex", u, true)
					emit("claude", u, true)
				}
				return f
			}
			if program != "codex" && program != "claude" {
				return f
			}
			launch := f
			modeAt := -1
			uncertain := false
			for i := 1; i < len(args); i++ {
				if program == "codex" && (args[i] == "-C" || args[i] == "--cd") && i+1 < len(args) {
					i++
					if dynamic[i] {
						launch.dirs = nil
						launch.unknown = true
					} else {
						launch = setLaunchDir(launch, args[i])
					}
					continue
				}
				if program == "codex" && strings.HasPrefix(args[i], "--cd=") {
					if dynamic[i] {
						launch.dirs = nil
						launch.unknown = true
					} else {
						launch = setLaunchDir(launch, strings.TrimPrefix(args[i], "--cd="))
					}
					continue
				}
				if modeAt < 0 && launchOptionValue(program, args[i]) && i+1 < len(args) {
					i++
					continue
				}
				if dynamic[i] && modeAt < 0 && strings.HasSuffix(args[i], "=") && launchOptionValue(program, strings.TrimSuffix(args[i], "=")) {
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
						return f
					}
				}
			}
			if modeAt >= 0 {
				emit(program, launch, uncertain)
			} else if uncertain {
				u := launch
				u.unknown = true
				u.dirs = nil
				emit(program, u, true)
			}
			return f
		default:
			cwdChanged := false
			syntax.Walk(stmt.Cmd, func(n syntax.Node) bool {
				if st, ok := n.(*syntax.Stmt); ok {
					nested := f
					nested.unknown = true
					nested.dirs = nil
					after := run(st, nested)
					cwdChanged = cwdChanged || after.cwdChanged
					return false
				}
				return true
			})
			if cwdChanged {
				f.unknown = true
				f.cwdChanged = true
			}
		}
		if stmt.Background {
			return before
		}
		return f
	}
	list(file.Stmts, initial)
	return out
}

func setLaunchDir(f shellFlow, dir string) shellFlow {
	resolved := resolveLaunchDirs(f.dirs, dir)
	absolute := len(resolveLaunchDirs(nil, dir)) > 0
	unknown := len(resolved) == 0 || f.unknown && !absolute
	f.dirs, f.unknown = resolved, unknown
	return f
}

func resolveLaunchDirs(bases []string, dir string) []string {
	if strings.HasPrefix(dir, "~") {
		return nil
	}
	if path.IsAbs(strings.ReplaceAll(dir, `\`, "/")) || len(dir) >= 3 && dir[1] == ':' && (dir[2] == '/' || dir[2] == '\\') {
		return []string{ShellWorkdir("", dir)}
	}
	var out []string
	for _, base := range bases {
		d := ShellWorkdir(base, dir)
		if !slices.Contains(out, d) {
			out = append(out, d)
		}
	}
	return out
}

func guardedLaunchDir(stmt *syntax.Stmt, f shellFlow) (shellFlow, bool) {
	if binary, ok := stmt.Cmd.(*syntax.BinaryCmd); ok && binary.Op == syntax.AndStmt {
		if prior, ok := guardedLaunchDir(binary.X, f); ok {
			f = prior
		}
		return guardedLaunchDir(binary.Y, f)
	}
	if !isLiteralCD(stmt) {
		return f, false
	}
	call := stmt.Cmd.(*syntax.CallExpr)
	target, _ := launchWord(call.Args[1].Parts)
	return setLaunchDir(f, target), true
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
