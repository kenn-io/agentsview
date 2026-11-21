package parser

import (
	"context"
	"fmt"
	"io"
	"net/url"
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
	prPathRE   = regexp.MustCompile(`^/([A-Za-z0-9_.-]+(?:/[A-Za-z0-9_.-]+)+)/pull/([0-9]+)/?$`)
)

func parsePRURL(raw string) (PRLink, bool) {
	u, err := url.Parse(raw)
	if err != nil {
		return PRLink{}, false
	}
	match := prPathRE.FindStringSubmatch(u.Path)
	if match == nil {
		return PRLink{}, false
	}
	number, err := strconv.Atoi(match[2])
	if err != nil {
		return PRLink{}, false
	}
	return NewPRLink(raw, match[1], number)
}

// shellCall is one shell command, with expansions marked as opaque.
type shellCall struct {
	Words      []string
	Literal    []bool
	Repo       string
	Host       string
	RepoOpaque bool
	HostOpaque bool
	ChangedDir bool
}

type shellValue struct {
	value    string
	known    bool
	exported bool
}

type shellState struct {
	repo, host shellValue
	changedDir bool
}

// shellCommands walks shell statements without evaluating expansions or uncertain branches.
func shellCommands(command string) []shellCall {
	file, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil {
		return nil
	}
	var calls []shellCall
	var walkStmts func([]*syntax.Stmt, *shellState) int
	var walkStmt func(*syntax.Stmt, *shellState) int
	var walkIf func(*syntax.IfClause, *shellState) int
	walkIf = func(node *syntax.IfClause, state *shellState) int {
		if node.Cond == nil {
			return walkStmts(node.Then, state)
		}
		status := walkStmts(node.Cond, state)
		if status == 0 {
			return walkStmts(node.Then, state)
		}
		if status == 1 {
			if node.Else != nil {
				return walkIf(node.Else, state)
			}
			return 0
		}
		state.repo = shellValue{exported: true}
		state.host = shellValue{exported: true}
		state.changedDir = true
		return -1
	}
	walkStmts = func(stmts []*syntax.Stmt, state *shellState) int {
		status := -1
		for _, stmt := range stmts {
			status = walkStmt(stmt, state)
		}
		return status
	}
	walkStmt = func(stmt *syntax.Stmt, state *shellState) int {
		if stmt == nil || stmt.Background || stmt.Coprocess {
			return -1
		}
		status := -1
		switch node := stmt.Cmd.(type) {
		case *syntax.BinaryCmd:
			if node.Op == syntax.Pipe || node.Op == syntax.PipeAll {
				left, right := *state, *state
				walkStmt(node.X, &left)
				walkStmt(node.Y, &right)
				break
			}
			status = walkStmt(node.X, state)
			switch node.Op {
			case syntax.AndStmt, syntax.OrStmt:
				if status < 0 {
					state.repo = shellValue{known: false, exported: true}
					state.host = shellValue{known: false, exported: true}
					state.changedDir = true
				} else if node.Op == syntax.AndStmt && status == 0 || node.Op == syntax.OrStmt && status == 1 {
					status = walkStmt(node.Y, state)
				}
			default:
				state.repo = shellValue{known: false, exported: true}
				state.host = shellValue{known: false, exported: true}
				state.changedDir = true
				status = -1
			}
		case *syntax.IfClause:
			status = walkIf(node, state)
		case *syntax.Subshell:
			child := *state
			status = walkStmts(node.Stmts, &child)
		case *syntax.Block:
			status = walkStmts(node.Stmts, state)
		case *syntax.DeclClause:
			if node.Variant.Value != "export" {
				return -1
			}
			for _, assignment := range node.Args {
				if assignment.Value != nil {
					syntax.Walk(assignment.Value, func(n syntax.Node) bool {
						if sub, ok := n.(*syntax.CmdSubst); ok {
							child := *state
							walkStmts(sub.Stmts, &child)
							return false
						}
						return true
					})
				}
				if assignment.Name == nil {
					continue
				}
				var target *shellValue
				switch assignment.Name.Value {
				case "GH_REPO":
					target = &state.repo
				case "GH_HOST":
					target = &state.host
				}
				if target == nil {
					continue
				}
				target.exported = true
				if assignment.Value != nil {
					target.value, target.known = shellLiteral(assignment.Value.Parts)
				}
			}
			status = 0
		case *syntax.CallExpr:
			visitWord := func(word *syntax.Word) {
				if word == nil {
					return
				}
				syntax.Walk(word, func(n syntax.Node) bool {
					if sub, ok := n.(*syntax.CmdSubst); ok {
						child := *state
						walkStmts(sub.Stmts, &child)
						return false
					}
					return true
				})
			}
			for _, assignment := range node.Assigns {
				visitWord(assignment.Value)
			}
			for _, word := range node.Args {
				visitWord(word)
			}
			local := *state
			for _, assignment := range node.Assigns {
				if assignment.Name == nil {
					continue
				}
				value, known := "", true
				if assignment.Value != nil {
					value, known = shellLiteral(assignment.Value.Parts)
				}
				var target *shellValue
				switch assignment.Name.Value {
				case "GH_REPO":
					target = &local.repo
				case "GH_HOST":
					target = &local.host
				}
				if target != nil {
					target.value, target.known = value, known
					if len(node.Args) > 0 {
						target.exported = true
					}
				}
			}
			if len(node.Args) == 0 {
				*state = local
				return 0
			}
			var args []string
			var literals []bool
			for _, word := range node.Args {
				value, known := shellLiteral(word.Parts)
				args = append(args, value)
				literals = append(literals, known)
			}
			if !literals[0] {
				return -1
			}
			switch args[0] {
			case "true", ":":
				status = 0
			case "false":
				status = 1
			case "cd", "pushd", "popd":
				state.changedDir = true
			case "export", "unset":
				for i := 1; i < len(args); i++ {
					if !literals[i] {
						state.repo = shellValue{exported: true}
						state.host = shellValue{exported: true}
						continue
					}
					name, value, assigned := strings.Cut(args[i], "=")
					var target *shellValue
					switch name {
					case "GH_REPO":
						target = &state.repo
					case "GH_HOST":
						target = &state.host
					}
					if target == nil {
						continue
					}
					if args[0] == "unset" {
						*target = shellValue{}
					} else {
						target.exported = true
						if assigned {
							target.value = value
							target.known = true
						}
					}
				}
				status = 0
			default:
				call := shellCall{Words: args, Literal: literals, ChangedDir: local.changedDir}
				if local.repo.exported {
					call.Repo = local.repo.value
					call.RepoOpaque = !local.repo.known
				}
				if local.host.exported {
					call.Host = local.host.value
					call.HostOpaque = !local.host.known
				}
				calls = append(calls, call)
			}
		default:
			state.repo = shellValue{exported: true}
			state.host = shellValue{exported: true}
			state.changedDir = true
		}
		if stmt.Negated && status >= 0 {
			status = 1 - status
		}
		return status
	}
	walkStmts(file.Stmts, &shellState{})
	return calls
}

func shellLiteral(parts []syntax.WordPart) (string, bool) {
	var b strings.Builder
	for _, part := range parts {
		switch v := part.(type) {
		case *syntax.Lit:
			b.WriteString(v.Value)
		case *syntax.SglQuoted:
			if v.Dollar {
				return "", false
			}
			b.WriteString(v.Value)
		case *syntax.DblQuoted:
			if v.Dollar {
				return "", false
			}
			value, ok := shellLiteral(v.Parts)
			if !ok {
				return "", false
			}
			b.WriteString(value)
		default:
			return "", false
		}
	}
	return b.String(), true
}

// CommandPRLinks derives links from one shell command and child-agent prompts.
func CommandPRLinks(command, remote string) []PRLink {
	var links []PRLink
	for _, call := range shellCommands(command) {
		call = unwrapShellCall(call)
		links = CombinePRLinks(links, ghPRCallLinks(call, remote))
		prompt := launcherPrompt(call.Words, call.Literal)
		found, _ := PromptPRLinksContext(context.Background(), prompt)
		links = CombinePRLinks(links, found)
	}
	return links
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
				switch name {
				case "GH_REPO":
					call.Repo, call.RepoOpaque = value, false
				case "GH_HOST":
					call.Host, call.HostOpaque = value, false
				}
				call.Words, call.Literal = call.Words[1:], call.Literal[1:]
			}
		default:
			return call
		}
	}
	return call
}

func ghPRCallLinks(call shellCall, remote string) []PRLink {
	args := call.Words
	if len(args) < 3 || !call.Literal[0] || !strings.EqualFold(strings.TrimSuffix(path.Base(args[0]), ".exe"), "gh") {
		return nil
	}
	repo, invalidRepo, i := call.Repo, call.RepoOpaque, 1
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
	switch action {
	case "view", "diff", "checks", "merge", "edit", "comment", "review", "ready", "close", "reopen", "checkout", "update-branch", "lock", "unlock", "revert":
	default:
		return nil
	}
	flags := pflag.NewFlagSet("gh pr "+action, pflag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.SetInterspersed(true)
	repoFlag := flags.StringP("repo", "R", repo, "")
	for _, spec := range strings.Fields(ghPRFlags[action]) {
		valueFlag := strings.HasSuffix(spec, "=")
		spec = strings.TrimSuffix(spec, "=")
		name, short, _ := strings.Cut(spec, ":")
		if valueFlag {
			flags.StringP(name, short, "", "")
		} else {
			flags.BoolP(name, short, false, "")
		}
	}
	const opaque = "\x00opaque-shell-word"
	var arguments []string
	for j := i + 2; j < len(args); j++ {
		if call.Literal[j] {
			arguments = append(arguments, args[j])
		} else {
			arguments = append(arguments, opaque)
		}
	}
	if flags.Parse(arguments) != nil {
		return nil
	}
	if flags.Changed("repo") {
		repo = *repoFlag
		invalidRepo = repo == "" || repo == opaque
	}
	selectors := flags.Args()
	if len(selectors) != 1 || selectors[0] == opaque {
		return nil
	}
	selector := selectors[0]
	if link, ok := parsePRURL(selector); ok {
		return []PRLink{link}
	}
	if invalidRepo {
		return nil
	}
	number, err := strconv.Atoi(selector)
	if err != nil || number <= 0 {
		return nil
	}
	host := "github.com"
	if repo == "" && !call.ChangedDir {
		if normalized, ok := export.NormalizeGitRemote(remote); ok {
			host, repo, _ = strings.Cut(normalized, "/")
		}
	} else if parts := strings.Split(repo, "/"); len(parts) == 3 {
		host, repo = parts[0], strings.Join(parts[1:], "/")
	} else if repo != "" {
		if call.HostOpaque {
			return nil
		}
		if call.Host != "" {
			host = call.Host
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
