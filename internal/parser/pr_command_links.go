package parser

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"

	"go.kenn.io/agentsview/internal/export"
	"mvdan.cc/sh/v3/syntax"
)

var (
	repoPartRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	repoPathRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
)

type shellCall struct {
	Words   []string
	Literal []bool
	Tainted bool
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

// CommandPRLinks derives links and reports whether the shell context changed.
func CommandPRLinks(ctx context.Context, command, remote string) ([]PRLink, bool, error) {
	links, _, changed, err := CommandPRLinkSets(ctx, command, remote)
	return links, changed, err
}

// CommandPRLinkSets also returns links that do not rely on the checkout remote.
func CommandPRLinkSets(ctx context.Context, command, remote string) ([]PRLink, []PRLink, bool, error) {
	file, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil {
		return nil, nil, true, nil
	}
	var calls []shellCall
	tainted := false
	syntax.Walk(file, func(node syntax.Node) bool {
		switch n := node.(type) {
		case *syntax.FuncDecl:
			tainted = true
			return false
		case *syntax.Assign:
			if n.Name != nil && (n.Name.Value == "GH_REPO" || n.Name.Value == "GH_HOST") {
				tainted = true
			}
		case *syntax.CallExpr:
			for _, assignment := range n.Assigns {
				if assignment.Name != nil && (assignment.Name.Value == "GH_REPO" || assignment.Name.Value == "GH_HOST") {
					tainted = true
				}
			}
			var call shellCall
			for _, word := range n.Args {
				value, literal := shellLiteral(word.Parts)
				call.Words = append(call.Words, value)
				call.Literal = append(call.Literal, literal)
			}
			if len(call.Words) > 0 && call.Literal[0] {
				switch path.Base(call.Words[0]) {
				case "cd", "pushd", "popd", "eval", "source", ".":
					tainted = true
				case "export", "unset":
					for _, arg := range call.Words[1:] {
						name, _, _ := strings.Cut(arg, "=")
						if name == "GH_REPO" || name == "GH_HOST" {
							tainted = true
						}
					}
				}
			}
			call.Tainted = tainted
			calls = append(calls, call)
		}
		return true
	})
	var lists, explicitLists [][]PRLink
	for _, call := range calls {
		if err := ctx.Err(); err != nil {
			return nil, nil, false, err
		}
		callRemote := remote
		if call.Tainted {
			callRemote = ""
		}
		lists = append(lists, ghPRCallLinks(ctx, call, callRemote))
		explicitLists = append(explicitLists, ghPRCallLinks(ctx, call, ""))
		links, err := PromptPRLinksContext(ctx, launcherPrompt(call.Words, call.Literal))
		if err != nil {
			return nil, nil, false, err
		}
		lists = append(lists, links)
		explicitLists = append(explicitLists, links)
	}
	return CombinePRLinks(lists...), CombinePRLinks(explicitLists...), tainted, ctx.Err()
}

func ghPRCallLinks(ctx context.Context, call shellCall, remote string) []PRLink {
	args := call.Words
	if len(args) < 4 || !call.Literal[0] || path.Base(args[0]) != "gh" {
		return nil
	}
	repo, selector := "", ""
	repoSpecified := false
	i := 1
	if i < len(args) && call.Literal[i] && (args[i] == "-R" || args[i] == "--repo") {
		if i+1 >= len(args) || !call.Literal[i+1] {
			return nil
		}
		repo = args[i+1]
		repoSpecified = true
		i += 2
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
	valueFlags := map[string]string{
		"view":     "--json --jq -q --template -t",
		"diff":     "--color --exclude -e",
		"checks":   "--interval -i --jq -q --json --template -t",
		"merge":    "--author-email -A --body -b --body-file -F --match-head-commit --subject -t",
		"edit":     "--add-assignee --add-label --add-project --add-reviewer --attach --base -B --body -b --body-file -F --milestone -m --remove-assignee --remove-label --remove-milestone --remove-project --remove-reviewer --title -t",
		"comment":  "--attach --body -b --body-file -F",
		"review":   "--body -b --body-file -F",
		"close":    "--comment -c",
		"reopen":   "--comment -c",
		"checkout": "--branch -b --worktree",
		"lock":     "--reason -r",
		"revert":   "--body -b --body-file -F --title -t",
	}
	takesValue := strings.Fields(valueFlags[action])
	for j := i + 2; j < len(args); j++ {
		if !call.Literal[j] {
			if selector == "" {
				return nil
			}
			continue
		}
		arg := args[j]
		if arg == "-R" || arg == "--repo" {
			if j+1 >= len(args) || !call.Literal[j+1] {
				return nil
			}
			j++
			repo = args[j]
			repoSpecified = true
			continue
		}
		if strings.HasPrefix(arg, "--repo=") {
			repo = strings.TrimPrefix(arg, "--repo=")
			repoSpecified = true
			continue
		}
		if strings.HasPrefix(arg, "-R") && len(arg) > 2 {
			repo = strings.TrimPrefix(arg[2:], "=")
			repoSpecified = true
			continue
		}
		if strings.HasPrefix(arg, "-") {
			flag, _, attached := strings.Cut(arg, "=")
			for _, valueFlag := range takesValue {
				if flag == valueFlag && !attached {
					j++
					if j >= len(args) {
						return nil
					}
					break
				}
			}
			continue
		}
		if selector == "" {
			selector = arg
		}
	}
	if links, _ := PromptPRLinksContext(ctx, selector); len(links) == 1 && (strings.HasPrefix(selector, "https://") || strings.HasPrefix(selector, "http://")) {
		return links
	}
	number, err := strconv.Atoi(selector)
	if err != nil || number <= 0 {
		return nil
	}
	host := "github.com"
	if repo == "" && repoSpecified {
		return nil
	}
	if repo == "" {
		if normalized, ok := export.NormalizeGitRemote(remote); ok {
			host, repo, _ = strings.Cut(normalized, "/")
		}
	} else if parts := strings.Split(repo, "/"); len(parts) == 3 {
		host, repo = parts[0], strings.Join(parts[1:], "/")
	}
	if !repoPartRE.MatchString(host) || !repoPathRE.MatchString(repo) {
		return nil
	}
	if link, ok := NewPRLink(fmt.Sprintf("https://%s/%s/pull/%d", host, repo, number), repo, number); ok {
		return []PRLink{link}
	}
	return nil
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
