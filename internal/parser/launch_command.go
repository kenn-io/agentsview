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

// Reach describes whether a command in archived source can have run.
type Reach uint8

const (
	Reached Reach = iota
	Possible
	Dead
)

// LaunchCommand describes one possible agent launch.
type LaunchCommand struct {
	Agent     string
	Dirs      []string
	Unknown   bool
	Reach     Reach
	CDGuarded bool
}
type shellFlow struct {
	dirs       []string
	unknown    bool
	reach      Reach
	cdGuarded  bool
	mayStop    bool
	cwdChanged bool
}
type shellStatus uint8

const (
	statusUnknown shellStatus = iota
	statusSuccess
	statusFailure
)

func possibleReach(r Reach) Reach {
	if r == Dead {
		return Dead
	}
	return Possible
}
func mergeShellFlows(a, b shellFlow) shellFlow {
	if a.reach == Dead && b.reach == Dead {
		a.mayStop = true
		return a
	}
	if a.reach == Dead {
		b.reach = possibleReach(b.reach)
		b.mayStop = true
		return b
	}
	if b.reach == Dead {
		a.reach = possibleReach(a.reach)
		a.mayStop = true
		return a
	}
	for _, dir := range b.dirs {
		if !slices.Contains(a.dirs, dir) {
			a.dirs = append(a.dirs, dir)
		}
	}
	a.unknown = a.unknown || b.unknown
	a.mayStop = a.mayStop || b.mayStop
	a.cwdChanged = a.cwdChanged || b.cwdChanged
	if a.reach == Possible || b.reach == Possible {
		a.reach = Possible
	}
	a.cdGuarded = false
	return a
}

// LaunchesFromCommand reads reach and directory evidence from shell syntax.
func LaunchesFromCommand(command, baseDir string) []LaunchCommand {
	file, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil {
		var out []LaunchCommand
		for _, agent := range []string{"codex", "claude"} {
			if regexp.MustCompile(`(?i)(^|[^a-z0-9_])` + agent + `([^a-z0-9_]|$)`).MatchString(command) {
				out = append(out, LaunchCommand{Agent: agent, Unknown: true, Reach: Possible})
			}
		}
		return out
	}
	initial := shellFlow{reach: Reached}
	if baseDir == "" {
		initial.unknown = true
	} else {
		initial.dirs = []string{baseDir}
	}
	var out []LaunchCommand
	var run func(*syntax.Stmt, shellFlow) (shellFlow, shellStatus)
	var list func([]*syntax.Stmt, shellFlow) (shellFlow, shellStatus)
	list = func(stmts []*syntax.Stmt, f shellFlow) (shellFlow, shellStatus) {
		status := statusUnknown
		for _, stmt := range stmts {
			f, status = run(stmt, f)
		}
		return f, status
	}
	emit := func(agent string, f shellFlow, uncertain bool) {
		if f.reach == Dead {
			return
		}
		r := f.reach
		if uncertain {
			r = possibleReach(r)
		}
		out = append(out, LaunchCommand{Agent: agent, Dirs: slices.Clone(f.dirs), Unknown: f.unknown, Reach: r, CDGuarded: f.cdGuarded && !uncertain})
	}
	run = func(stmt *syntax.Stmt, f shellFlow) (result shellFlow, resultStatus shellStatus) {
		if f.reach == Dead {
			return f, statusUnknown
		}
		f.dirs = slices.Clone(f.dirs)
		before := f
		before.dirs = slices.Clone(f.dirs)
		if stmt.Background {
			defer func() { result = before; resultStatus = statusUnknown }()
		}
		if stmt.Negated {
			defer func() {
				if resultStatus == statusSuccess {
					resultStatus = statusFailure
				} else if resultStatus == statusFailure {
					resultStatus = statusSuccess
				}
			}()
		}
		switch c := stmt.Cmd.(type) {
		case *syntax.BinaryCmd:
			switch c.Op {
			case syntax.AndStmt, syntax.OrStmt:
				left, status := run(c.X, f)
				rhs := left
				take := status == statusSuccess
				if c.Op == syntax.OrStmt {
					take = status == statusFailure
				}
				skip := status == statusFailure
				if c.Op == syntax.OrStmt {
					skip = status == statusSuccess
				}
				if skip {
					return left, status
				}
				if status == statusUnknown {
					rhs.reach = possibleReach(f.reach)
					rhs.cdGuarded = false
					if c.Op == syntax.AndStmt && isLiteralCD(c.X) {
						if call, ok := c.X.Cmd.(*syntax.CallExpr); ok {
							target, _ := launchWord(call.Args[1].Parts)
							rhs.dirs = nil
							rhs.unknown = before.unknown
							for _, d := range before.dirs {
								resolved := launchWorkdir(d, target)
								if resolved == "" {
									rhs.unknown = true
								} else if !slices.Contains(rhs.dirs, resolved) {
									rhs.dirs = append(rhs.dirs, resolved)
								}
							}
							if len(before.dirs) == 0 {
								resolved := launchWorkdir("", target)
								if resolved != "" {
									rhs.dirs = []string{resolved}
									rhs.unknown = false
								}
							}
							if absolute := resolveLaunchDirs(nil, target); len(absolute) > 0 {
								rhs.dirs = absolute
								rhs.unknown = false
							}
							rhs.cdGuarded = f.reach == Reached && !rhs.unknown
						}
					}
				}
				right, rightStatus := run(c.Y, rhs)
				if take {
					return right, rightStatus
				}
				merged := mergeShellFlows(left, right)
				if right.reach != Dead && !right.mayStop && left.reach == Reached {
					merged.reach = Reached
				}
				return merged, statusUnknown
			case syntax.Pipe, syntax.PipeAll:
				nested := f
				nested.reach = possibleReach(f.reach)
				nested.cdGuarded = false
				_, _ = run(c.X, nested)
				_, _ = run(c.Y, nested)
				return before, statusUnknown
			}
		case *syntax.Subshell:
			nested := f
			nested.reach = possibleReach(f.reach)
			nested.unknown = true
			nested.dirs = nil
			nested.cdGuarded = false
			_, _ = list(c.Stmts, nested)
			return f, statusUnknown
		case *syntax.IfClause:
			if len(c.Cond) == 0 {
				return list(c.Then, f)
			}
			cond, st := list(c.Cond, f)
			if st == statusSuccess {
				return list(c.Then, cond)
			}
			if st == statusFailure {
				if c.Else != nil {
					return run(&syntax.Stmt{Cmd: c.Else}, cond)
				}
				return cond, statusSuccess
			}
			branch := cond
			branch.reach = possibleReach(cond.reach)
			branch.cdGuarded = false
			yes, _ := list(c.Then, branch)
			no := cond
			if c.Else != nil {
				no, _ = run(&syntax.Stmt{Cmd: c.Else}, branch)
			}
			merged := mergeShellFlows(yes, no)
			if !yes.mayStop && !no.mayStop {
				merged.reach = cond.reach
			}
			return merged, statusUnknown
		case *syntax.CallExpr:
			args := make([]string, len(c.Args))
			dynamic := make([]bool, len(c.Args))
			for i, w := range c.Args {
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
						if sub, yes := n.(*syntax.CmdSubst); yes {
							nested := f
							nested.reach = possibleReach(f.reach)
							nested.unknown = true
							nested.dirs = nil
							nested.cdGuarded = false
							_, _ = list(sub.Stmts, nested)
							return false
						}
						return true
					})
				}
			}
			if len(args) == 0 {
				return f, statusUnknown
			}
			program := launchProgram(args[0])
			switch program {
			case "true", ":":
				return f, statusSuccess
			case "false":
				return f, statusFailure
			case "exit":
				f.mayStop = true
				f.reach = Dead
				return f, statusUnknown
			case "source", ".":
				f.mayStop = true
				f.reach = possibleReach(f.reach)
				fallthrough
			case "pushd", "popd":
				f.unknown = true
				f.cdGuarded = false
				f.cwdChanged = true
				return f, statusUnknown
			case "cd", "set-location":
				f.cwdChanged = true
				if len(args) != 2 || dynamic[1] || args[1] == "-" || strings.HasPrefix(args[1], "~") {
					f.unknown = true
					return f, statusUnknown
				}
				for _, d := range before.dirs {
					target := launchWorkdir(d, args[1])
					if target == "" {
						f.unknown = true
					} else if !slices.Contains(f.dirs, target) {
						f.dirs = append(f.dirs, target)
					}
				}
				if len(before.dirs) == 0 {
					target := launchWorkdir("", args[1])
					if target != "" {
						f.dirs = append(f.dirs, target)
					} else {
						f.unknown = true
					}
				}
				return f, statusUnknown
			}
			if (program == "bash" || program == "sh" || program == "zsh") && len(args) >= 3 && (args[1] == "-c" || args[1] == "-lc") && !dynamic[2] {
				nested, err := syntax.NewParser().Parse(strings.NewReader(args[2]), "")
				if err == nil {
					_, _ = list(nested.Stmts, f)
				} else {
					for _, agent := range []string{"codex", "claude"} {
						if strings.Contains(strings.ToLower(args[2]), agent) {
							emit(agent, f, true)
						}
					}
				}
				return f, statusUnknown
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
					for _, agent := range []string{"codex", "claude"} {
						if launchProgram(args[i]) == agent || dynamic[i] && (i == 1 || i+1 < len(args) || program == "eval" || (program == "bash" || program == "sh" || program == "zsh") && (args[i-1] == "-c" || args[i-1] == "-lc")) {
							u := f
							u.unknown = true
							u.dirs = nil
							emit(agent, u, true)
						}
					}
				}
				if program == "exec" && len(args) > 1 {
					f.mayStop = true
					f.reach = possibleReach(f.reach)
				}
				if program == "eval" {
					f.unknown = true
					f.cdGuarded = false
					f.cwdChanged = true
					f.mayStop = true
					f.reach = possibleReach(f.reach)
				}
				return f, statusUnknown
			}
			if dynamic[0] {
				if len(args) > 1 {
					u := f
					u.unknown = true
					u.dirs = nil
					emit("codex", u, true)
					emit("claude", u, true)
				}
				return f, statusUnknown
			}
			if program != "codex" && program != "claude" {
				return f, statusUnknown
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
						launch.cdGuarded = false
					} else {
						launch = setLaunchDir(launch, args[i])
					}
					continue
				}
				if program == "codex" && strings.HasPrefix(args[i], "--cd=") {
					if dynamic[i] {
						launch.dirs = nil
						launch.unknown = true
						launch.cdGuarded = false
					} else {
						launch = setLaunchDir(launch, strings.TrimPrefix(args[i], "--cd="))
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
						return f, statusUnknown
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
			return f, statusUnknown
		default:
			mayStop := false
			cwdChanged := false
			syntax.Walk(stmt.Cmd, func(n syntax.Node) bool {
				if st, ok := n.(*syntax.Stmt); ok {
					nested := f
					nested.reach = possibleReach(f.reach)
					nested.unknown = true
					nested.dirs = nil
					nested.cdGuarded = false
					after, _ := run(st, nested)
					mayStop = mayStop || after.mayStop
					cwdChanged = cwdChanged || after.cwdChanged
					return false
				}
				return true
			})
			if mayStop {
				f.mayStop = true
				f.reach = possibleReach(f.reach)
			}
			if cwdChanged {
				f.unknown = true
				f.cdGuarded = false
				f.cwdChanged = true
			}
		}
		if stmt.Background {
			return before, statusUnknown
		}
		return f, statusUnknown
	}
	_, _ = list(file.Stmts, initial)
	return out
}

func setLaunchDir(f shellFlow, dir string) shellFlow {
	resolved := resolveLaunchDirs(f.dirs, dir)
	absolute := len(resolveLaunchDirs(nil, dir)) > 0
	unknown := len(resolved) == 0 || f.unknown && !absolute
	if f.cdGuarded && (!slices.Equal(f.dirs, resolved) || unknown) {
		f.cdGuarded = false
	}
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
