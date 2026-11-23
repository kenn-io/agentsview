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
	Words         []string
	Literal       []bool
	Tainted       bool
	HostUncertain bool
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

// SamePRWorkingDirectory compares paths using the recorded checkout's syntax.
func SamePRWorkingDirectory(cwd, dir string) bool {
	return cwd != "" && dir != "" && path.Clean(strings.ReplaceAll(cwd, `\`, "/")) == path.Clean(strings.ReplaceAll(dir, `\`, "/"))
}

// CommandPRLinks derives links and reports whether the shell context changed.
func CommandPRLinks(ctx context.Context, command, cwd, remote string) ([]PRLink, bool, error) {
	return commandPRLinks(ctx, command, cwd, remote, false)
}

func commandPRLinks(ctx context.Context, command, cwd, remote string, hostUncertain bool) ([]PRLink, bool, error) {
	file, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil {
		return nil, true, nil //nolint:nilerr // Malformed optional shell input leaves the shell context unknown.
	}
	var calls []shellCall
	tainted := false
	type shellScope struct {
		restore bool
		tainted bool
	}
	var scopes []shellScope
	syntax.Walk(file, func(node syntax.Node) bool {
		if node == nil {
			scope := scopes[len(scopes)-1]
			scopes = scopes[:len(scopes)-1]
			if scope.restore {
				tainted = scope.tainted
			}
			return true
		}
		if _, ok := node.(*syntax.FuncDecl); ok {
			tainted = true
			return false
		}
		scope := shellScope{tainted: tainted}
		switch node.(type) {
		case *syntax.Subshell, *syntax.CmdSubst, *syntax.ProcSubst:
			scope.restore = true
		}
		scopes = append(scopes, scope)
		switch n := node.(type) {
		case *syntax.DeclClause:
			for _, assignment := range n.Args {
				if assignment.Name != nil && (assignment.Name.Value == "GH_REPO" || assignment.Name.Value == "GH_HOST") {
					tainted = true
				}
			}
		case *syntax.CallExpr:
			call := shellCall{Tainted: tainted, HostUncertain: hostUncertain}
			for _, assignment := range n.Assigns {
				if assignment.Name != nil && (assignment.Name.Value == "GH_REPO" || assignment.Name.Value == "GH_HOST") {
					call.Tainted = true
					if assignment.Name.Value == "GH_HOST" {
						call.HostUncertain = true
					}
					if len(n.Args) == 0 {
						tainted = true
					}
				}
			}
			for _, word := range n.Args {
				value, literal := shellLiteral(word.Parts)
				call.Words = append(call.Words, value)
				call.Literal = append(call.Literal, literal)
			}
			if len(call.Words) > 0 && call.Literal[0] {
				switch path.Base(call.Words[0]) {
				case "cd":
					if len(call.Words) != 2 || !call.Literal[1] || !SamePRWorkingDirectory(cwd, call.Words[1]) {
						tainted = true
					}
				case "pushd", "popd", "eval", "source", ".":
					tainted = true
				}
			}
			call.Tainted = call.Tainted || tainted
			calls = append(calls, call)
		}
		return true
	})
	var lists [][]PRLink
	for _, call := range calls {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		callRemote := remote
		if call.Tainted {
			callRemote = ""
		}
		lists = append(lists, ghPRCallLinks(ctx, call, callRemote))
		if len(call.Words) > 0 && call.Literal[0] {
			switch path.Base(call.Words[0]) {
			case "sh", "bash", "zsh":
				for i := 1; i+1 < len(call.Words); i++ {
					if call.Literal[i] && (call.Words[i] == "-c" || call.Words[i] == "-lc") && call.Literal[i+1] {
						links, _, err := commandPRLinks(ctx, call.Words[i+1], cwd, callRemote, call.HostUncertain)
						if err != nil {
							return nil, false, err
						}
						lists = append(lists, links)
						break
					}
				}
			}
		}
		links, err := PromptPRLinksContext(ctx, launcherPrompt(call.Words, call.Literal))
		if err != nil {
			return nil, false, err
		}
		lists = append(lists, links)
	}
	return CombinePRLinks(lists...), tainted, ctx.Err()
}

// repoFlag reports whether args[i] is a -R or --repo option, its value, and
// the index of its last word, or -1 when the value is missing or not literal.
func repoFlag(call shellCall, i int) (string, int, bool) {
	arg := call.Words[i]
	switch {
	case arg == "-R" || arg == "--repo":
		if i+1 >= len(call.Words) || !call.Literal[i+1] {
			return "", -1, true
		}
		return call.Words[i+1], i + 1, true
	case strings.HasPrefix(arg, "--repo="):
		return strings.TrimPrefix(arg, "--repo="), i, true
	case strings.HasPrefix(arg, "-R") && len(arg) > 2:
		return strings.TrimPrefix(arg[2:], "="), i, true
	}
	return "", i, false
}

func ghPRCallLinks(ctx context.Context, call shellCall, remote string) []PRLink {
	args := call.Words
	if len(args) < 4 || !call.Literal[0] || path.Base(args[0]) != "gh" {
		return nil
	}
	repo, selector := "", ""
	repoSpecified := false
	i := 1
	for i < len(args) && call.Literal[i] {
		value, last, isRepo := repoFlag(call, i)
		if !isRepo {
			break
		}
		if last < 0 {
			return nil
		}
		repo, repoSpecified, i = value, true, last+1
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
			if strings.HasPrefix(args[j], "-R") || strings.HasPrefix(args[j], "--repo") {
				return nil
			}
			continue
		}
		arg := args[j]
		if value, last, isRepo := repoFlag(call, j); isRepo {
			if last < 0 {
				return nil
			}
			repo, repoSpecified, j = value, true, last
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
	if call.HostUncertain && (repo == "" || len(strings.Split(repo, "/")) != 3) {
		return nil
	}
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
	var wordsToScan []string
	for i := 1; i < len(words); i++ {
		if literal[i] {
			wordsToScan = append(wordsToScan, words[i])
		}
	}
	return strings.Join(wordsToScan, "\n")
}
