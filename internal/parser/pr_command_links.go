package parser

import (
	"context"
	"fmt"
	"io"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/spf13/pflag"
	"go.kenn.io/agentsview/internal/export"
	"mvdan.cc/sh/v3/syntax"
)

var (
	repoPartRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	repoPathRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
)

// shellCall is one shell command, with expansions marked as opaque.
type shellCall struct {
	Words      []string
	Literal    []bool
	Unknown    bool
	ChangedDir bool
}

// shellCommands collects calls without assuming which branches executed.
func shellCommands(command string) ([]shellCall, bool) {
	var calls []shellCall
	contextUnknown, changedDir := false, false
	var walk func(string)
	walk = func(text string) {
		file, err := syntax.NewParser().Parse(strings.NewReader(text), "")
		if err != nil {
			contextUnknown, changedDir = true, true
			return
		}
		var visit func(syntax.Node) bool
		visit = func(node syntax.Node) bool {
			switch n := node.(type) {
			case *syntax.Subshell, *syntax.CmdSubst:
				parentUnknown, parentDir := contextUnknown, changedDir
				var stmts []*syntax.Stmt
				switch child := n.(type) {
				case *syntax.Subshell:
					stmts = child.Stmts
				case *syntax.CmdSubst:
					stmts = child.Stmts
				}
				for _, stmt := range stmts {
					syntax.Walk(stmt, visit)
				}
				contextUnknown, changedDir = parentUnknown, parentDir
				return false
			case *syntax.DeclClause:
				contextUnknown = contextUnknown || declUnknown(n)
			case *syntax.CallExpr:
				callUnknown := contextUnknown
				for _, assignment := range n.Assigns {
					if assignment.Name != nil && (assignment.Name.Value == "GH_REPO" || assignment.Name.Value == "GH_HOST") {
						if assignment.Name.Value == "GH_HOST" && len(n.Args) > 0 && assignment.Value != nil {
							value, literal := shellLiteral(assignment.Value.Parts)
							if literal && value == "github.com" {
								continue
							}
						}
						callUnknown = true
					}
				}
				if len(n.Args) == 0 {
					contextUnknown = callUnknown
					return true
				}
				var call shellCall
				for _, word := range n.Args {
					value, literal := shellLiteral(word.Parts)
					call.Words = append(call.Words, value)
					call.Literal = append(call.Literal, literal)
				}
				if !call.Literal[0] {
					contextUnknown = true
					return true
				}
				builtin := unwrapBuiltin(call)
				switch path.Base(builtin.Words[0]) {
				case "eval", "source", ".":
					contextUnknown, changedDir = true, true
				case "cd", "pushd", "popd":
					changedDir = true
				case "export", "unset":
					for i, name := range builtin.Words[1:] {
						name, _, _ = strings.Cut(name, "=")
						if !builtin.Literal[i+1] || name == "GH_REPO" || name == "GH_HOST" {
							contextUnknown = true
						}
					}
				}
				unwrapped := unwrapShellCall(call)
				callUnknown = callUnknown || unwrapped.Unknown || contextUnknown
				if len(unwrapped.Words) >= 3 && unwrapped.Literal[0] && unwrapped.Literal[1] && unwrapped.Literal[2] &&
					(path.Base(unwrapped.Words[0]) == "sh" || path.Base(unwrapped.Words[0]) == "bash" || path.Base(unwrapped.Words[0]) == "zsh") &&
					(unwrapped.Words[1] == "-c" || unwrapped.Words[1] == "-lc") {
					parentUnknown, parentDir := contextUnknown, changedDir
					contextUnknown = callUnknown
					walk(unwrapped.Words[2])
					contextUnknown, changedDir = parentUnknown, parentDir
				} else {
					call.ChangedDir = changedDir
					call.Unknown = callUnknown
					calls = append(calls, call)
				}
			}
			return true
		}
		syntax.Walk(file, visit)
	}
	walk(command)
	return calls, changedDir
}

// declUnknown reports whether a declaration may change GH_REPO or GH_HOST.
func declUnknown(n *syntax.DeclClause) bool {
	switch n.Variant.Value {
	case "export", "declare", "typeset", "readonly", "local":
	default:
		return false
	}
	for _, arg := range n.Args {
		if arg.Name == nil && arg.Value != nil {
			if value, literal := shellLiteral(arg.Value.Parts); literal && strings.HasPrefix(value, "-") {
				continue
			}
		}
		if arg.Name == nil || arg.Name.Value == "GH_REPO" || arg.Name.Value == "GH_HOST" {
			return true
		}
	}
	return false
}

func unwrapBuiltin(call shellCall) shellCall {
	for len(call.Words) > 1 && call.Literal[0] && call.Literal[1] && !strings.HasPrefix(call.Words[1], "-") && (path.Base(call.Words[0]) == "builtin" || path.Base(call.Words[0]) == "command") {
		call.Words, call.Literal = call.Words[1:], call.Literal[1:]
	}
	return call
}

func shellLiteral(parts []syntax.WordPart) (string, bool) {
	var b strings.Builder
	for _, part := range parts {
		switch v := part.(type) {
		case *syntax.Lit:
			b.WriteString(v.Value)
		case *syntax.SglQuoted:
			if v.Dollar {
				return b.String(), false
			}
			b.WriteString(v.Value)
		case *syntax.DblQuoted:
			if v.Dollar {
				return b.String(), false
			}
			value, ok := shellLiteral(v.Parts)
			if !ok {
				b.WriteString(value)
				return b.String(), false
			}
			b.WriteString(value)
		default:
			return b.String(), false
		}
	}
	return b.String(), true
}

// CommandPRLinks derives links and reports whether later calls may use a different checkout.
func CommandPRLinks(ctx context.Context, command, remote string) ([]PRLink, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if len(command) > 256*1024 {
		return nil, true, nil
	}
	var lists [][]PRLink
	calls, changed := shellCommands(command)
	for _, call := range calls {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		call = unwrapShellCall(call)
		lists = append(lists, ghPRCallLinks(ctx, call, remote))
		prompt := launcherPrompt(call.Words, call.Literal)
		found, err := PromptPRLinksContext(ctx, prompt)
		if err != nil {
			return nil, false, err
		}
		lists = append(lists, found)
	}
	return CombinePRLinks(lists...), changed, ctx.Err()
}

func unwrapShellCall(call shellCall) shellCall {
	for len(call.Words) > 0 && call.Literal[0] {
		switch path.Base(call.Words[0]) {
		case "timeout":
			if len(call.Words) < 3 || !call.Literal[1] || strings.HasPrefix(call.Words[1], "-") {
				return call
			}
			call.Words, call.Literal = call.Words[2:], call.Literal[2:]
		case "nohup":
			call.Words, call.Literal = call.Words[1:], call.Literal[1:]
		case "env":
			call.Words, call.Literal = call.Words[1:], call.Literal[1:]
			for len(call.Words) > 0 && call.Literal[0] {
				name, value, ok := strings.Cut(call.Words[0], "=")
				if !ok {
					break
				}
				if name == "GH_REPO" || name == "GH_HOST" && value != "github.com" {
					call.Unknown = true
				}
				call.Words, call.Literal = call.Words[1:], call.Literal[1:]
			}
		default:
			return call
		}
	}
	return call
}

func ghPRCallLinks(ctx context.Context, call shellCall, remote string) []PRLink {
	args := call.Words
	if len(args) < 3 || !call.Literal[0] || !strings.EqualFold(strings.TrimSuffix(path.Base(args[0]), ".exe"), "gh") {
		return nil
	}
	repo, invalidRepo, i := "", call.Unknown, 1
	if i+1 < len(args) && call.Literal[i] && (args[i] == "-R" || args[i] == "--repo") {
		repo = args[i+1]
		invalidRepo = !call.Literal[i+1] || repo == ""
		i += 2
	} else if call.Literal[i] && strings.HasPrefix(args[i], "--repo=") {
		repo = strings.TrimPrefix(args[i], "--repo=")
		invalidRepo = repo == ""
		i++
	} else if call.Literal[i] && strings.HasPrefix(args[i], "-R") && len(args[i]) > 2 {
		repo = strings.TrimPrefix(args[i][2:], "=")
		invalidRepo = repo == ""
		i++
	}
	if i+2 >= len(args) || !call.Literal[i] || !call.Literal[i+1] || args[i] != "pr" {
		return nil
	}
	action := args[i+1]
	if action == "co" {
		action = "checkout"
	}
	switch action {
	case "view", "diff", "checks", "merge", "edit", "comment", "review", "ready", "close", "reopen", "checkout", "update-branch", "lock", "unlock", "revert":
	default:
		return nil
	}
	flags := pflag.NewFlagSet("gh pr "+action, pflag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.SetInterspersed(true)
	repoFlag := flags.StringP("repo", "R", repo, "")
	for spec := range strings.FieldsSeq(ghPRFlags[action]) {
		valueFlag := strings.HasSuffix(spec, "=")
		spec = strings.TrimSuffix(spec, "=")
		name, short, _ := strings.Cut(spec, ":")
		if valueFlag {
			flags.StringP(name, short, "", "")
		} else {
			flags.BoolP(name, short, false, "")
		}
	}
	arguments := shellFlagArguments(call, i+2)
	if flags.Parse(arguments) != nil {
		return nil
	}
	if flags.Changed("repo") {
		repo = *repoFlag
		invalidRepo = repo == "" || strings.Contains(repo, opaque)
	}
	selectors := flags.Args()
	if len(selectors) != 1 || selectors[0] == opaque {
		return nil
	}
	selector := selectors[0]
	if links, _ := PromptPRLinksContext(ctx, selector); len(links) == 1 {
		return links
	}
	if invalidRepo {
		return nil
	}
	number, err := strconv.Atoi(selector)
	if err != nil || number <= 0 {
		return nil
	}
	host := "github.com"
	if repo == "" && !call.ChangedDir && !call.Unknown {
		if normalized, ok := export.NormalizeGitRemote(remote); ok {
			host, repo, _ = strings.Cut(normalized, "/")
		}
	} else if parts := strings.Split(repo, "/"); len(parts) == 3 {
		host, repo = parts[0], strings.Join(parts[1:], "/")
	} else if repo != "" {
		if call.Unknown {
			return nil
		}
	}
	if !repoPartRE.MatchString(host) || !repoPathRE.MatchString(repo) {
		return nil
	}
	if link, ok := NewPRLink(fmt.Sprintf("https://%s/%s/pull/%d", host, repo, number), repo, number); ok {
		return []PRLink{link}
	}
	return nil
}

const opaque = "\x00opaque-shell-word"

func shellFlagArguments(call shellCall, start int) []string {
	var arguments []string
	for i := start; i < len(call.Words); i++ {
		arg := call.Words[i]
		if call.Literal[i] {
			arguments = append(arguments, arg)
		} else if strings.HasPrefix(arg, "-") && (strings.Contains(arg, "=") || !strings.HasPrefix(arg, "--") && len(arg) >= 2) {
			arguments = append(arguments, arg+opaque)
		} else {
			arguments = append(arguments, opaque)
		}
	}
	return arguments
}

// Options follow the GitHub CLI's arity and short names for each subcommand.
var ghPRFlags = map[string]string{
	"view":          "comments:c jq:q= json= template:t= web:w",
	"diff":          "allow-escape-sequences color= exclude:e= name-only patch web:w",
	"checks":        "fail-fast interval:i= jq:q= json= required template:t= watch web:w",
	"merge":         "admin author-email:A= auto body:b= body-file:F= delete-branch:d disable-auto match-head-commit= merge:m rebase:r squash:s subject:t=",
	"edit":          "add-assignee= add-label= add-project= add-reviewer= attach= base:B= body:b= body-file:F= milestone:m= remove-assignee= remove-label= remove-milestone remove-project= remove-reviewer= title:t=",
	"comment":       "attach= body:b= body-file:F= create-if-none delete-last edit-last editor:e web:w yes",
	"review":        "approve:a body:b= body-file:F= comment:c request-changes:r",
	"ready":         "undo",
	"close":         "comment:c= delete-branch:d",
	"reopen":        "comment:c=",
	"checkout":      "branch:b= detach force:f recurse-submodules worktree=",
	"update-branch": "rebase",
	"lock":          "reason:r=",
	"unlock":        "",
	"revert":        "body:b= body-file:F= draft:d title:t=",
}

func launcherPrompt(words []string, literal []bool) string {
	if len(words) == 0 || !literal[0] {
		return ""
	}
	program := path.Base(words[0])
	if program != "claude" && program != "codex" {
		return ""
	}
	printMode, execMode := false, false
	var prompt string
	endOptions := false
	for i := 1; i < len(words); i++ {
		if !literal[i] {
			return ""
		}
		arg := words[i]
		if !endOptions && arg == "--" {
			endOptions = true
			continue
		}
		if endOptions {
			if prompt == "" {
				prompt = arg
			}
			continue
		}
		if arg == "--help" || arg == "-h" || arg == "--version" || arg == "-V" || program == "claude" && arg == "-v" {
			return ""
		}
		if program == "claude" && (arg == "-p" || arg == "--print") {
			printMode = true
			continue
		}
		if program == "codex" && !execMode && (arg == "exec" || arg == "e") {
			execMode = true
			continue
		}
		if strings.HasPrefix(arg, "-") {
			if !strings.Contains(arg, "=") && launcherValueFlag(program, arg) && i+1 < len(words) {
				i++
			}
			continue
		}
		if program == "codex" && !execMode {
			return ""
		}
		if prompt == "" {
			prompt = arg
		}
	}
	if program == "claude" && !printMode || program == "codex" && (!execMode || prompt == "resume" || prompt == "fork" || prompt == "review" || prompt == "help") {
		return ""
	}
	return prompt
}

func launcherValueFlag(program, flag string) bool {
	switch flag {
	case "-m", "--model", "-c", "--config", "--settings", "--output-format", "--permission-mode", "--resume", "-r", "--output-schema", "--output-last-message", "-o", "-C", "--cd", "--add-dir", "--system-prompt", "--append-system-prompt":
		return true
	}
	return program == "codex" && (flag == "-p" || flag == "--profile" || flag == "-s" || flag == "--sandbox")
}
