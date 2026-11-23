package parser

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"go.kenn.io/agentsview/internal/export"
	"mvdan.cc/sh/v3/syntax"
)

var (
	repoPathRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	prHostRE   = regexp.MustCompile(`^[A-Za-z0-9.-]+(?::[0-9]+)?$`)
)

var ghPRValueFlags = map[string]string{
	"view":     "--json --jq -q --template -t",
	"diff":     "--color --exclude -e",
	"checks":   "--interval -i --jq -q --json --template -t",
	"merge":    "--author-email -A --body -b --body-file -F --match-head-commit --subject -t",
	"edit":     "--add-assignee --add-label --add-project --add-reviewer --attach --base -B --body -b --body-file -F --milestone -m --remove-assignee --remove-label --remove-project --remove-reviewer --title -t",
	"comment":  "--attach --body -b --body-file -F",
	"review":   "--body -b --body-file -F",
	"close":    "--comment -c",
	"reopen":   "--comment -c",
	"checkout": "--branch -b --worktree",
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

// CommandPRLinks finds PRs named by direct gh commands in a shell input.
func CommandPRLinks(ctx context.Context, command, remote string, allowBare bool) ([]PRLink, error) {
	links, _, _, _, _, err := AnalyzePRCommand(ctx, command, remote, allowBare, false)
	return links, err
}

// AnalyzePRCommand extracts links and shared shell context from one parse.
func AnalyzePRCommand(ctx context.Context, command, remote string, allowBare, hostUnknown bool) ([]PRLink, bool, bool, bool, []PRLink, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, false, false, nil, err
	}
	file, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil {
		return nil, false, false, false, nil, ctx.Err()
	}
	directoryChanged, repositoryChanged, persistent, firstRepo, firstHost := bashChangesRepositoryContext(file)
	var calls []*syntax.CallExpr
	syntax.Walk(file, func(node syntax.Node) bool {
		if call, ok := node.(*syntax.CallExpr); ok {
			calls = append(calls, call)
		}
		return true
	})
	remoteHost, remoteRepo := "", ""
	if normalized, ok := export.NormalizeGitRemote(remote); ok {
		remoteHost, remoteRepo, _ = strings.Cut(normalized, "/")
	}
	var links [][]PRLink
	var dependsOnRemote []PRLink
	for _, call := range calls {
		if err := ctx.Err(); err != nil {
			return nil, false, false, false, nil, err
		}
		bareSafe := allowBare && (!firstRepo.IsValid() || call.End().Offset() < firstRepo.Offset()) && !hasCommandSubstitution(call)
		host, hostSet, hostValid := "", false, true
		for _, a := range call.Assigns {
			if a.Name == nil {
				continue
			}
			if a.Name.Value == "GH_REPO" {
				bareSafe = false
			}
			if a.Name.Value == "GH_HOST" {
				hostSet = true
				if a.Value == nil {
					hostValid = false
				} else {
					host, hostValid = shellLiteral(a.Value.Parts)
					hostValid = hostValid && host != ""
				}
			}
		}
		words := make([]string, len(call.Args))
		literal := make([]bool, len(call.Args))
		for i, word := range call.Args {
			words[i], literal[i] = shellLiteral(word.Parts)
		}
		if len(words) < 4 || !literal[0] || !literal[1] || !literal[2] || words[0] != "gh" || words[1] != "pr" {
			continue
		}
		callHostUnknown := hostUnknown || firstHost.IsValid() && call.End().Offset() >= firstHost.Offset()
		if words[2] == "co" {
			words[2] = "checkout"
		}
		switch words[2] {
		case "view", "checkout", "edit", "diff", "checks", "comment", "review", "ready", "merge", "close", "reopen":
		default:
			continue
		}
		selector, repo := "", ""
		specified, invalid := false, false
		for i := 3; i < len(words); i++ {
			if !literal[i] {
				if selector == "" {
					invalid = true
				}
				if len(call.Args[i].Parts) > 0 {
					if prefix, ok := call.Args[i].Parts[0].(*syntax.Lit); ok && (strings.HasPrefix(prefix.Value, "-R") || strings.HasPrefix(prefix.Value, "--repo")) {
						invalid = true
					}
				}
				continue
			}
			word := words[i]
			switch {
			case word == "-R" || word == "--repo":
				specified = true
				if i+1 >= len(words) || !literal[i+1] {
					invalid = true
					continue
				}
				i++
				repo = words[i]
			case strings.HasPrefix(word, "--repo="):
				specified = true
				repo = strings.TrimPrefix(word, "--repo=")
			case strings.HasPrefix(word, "-R") && len(word) > 2:
				specified = true
				repo = strings.TrimPrefix(word[2:], "=")
			case strings.HasPrefix(word, "-"):
				if !strings.Contains(word, "=") && strings.Contains(" "+ghPRValueFlags[words[2]]+" ", " "+word+" ") {
					i++
				}
			case selector == "":
				selector = word
			}
		}
		if strings.HasPrefix(selector, "https://") || strings.HasPrefix(selector, "http://") {
			found, err := PromptPRLinksContext(ctx, selector)
			if err != nil {
				return nil, false, false, false, nil, err
			}
			links = append(links, found)
			continue
		}
		number, err := strconv.Atoi(selector)
		if err != nil || number <= 0 || invalid {
			continue
		}
		if !specified {
			if !bareSafe {
				continue
			}
			repo = remoteRepo
		}
		explicitHost := ""
		if specified && strings.Count(repo, "/") == 2 {
			explicitHost, repo, _ = strings.Cut(repo, "/")
		}
		if explicitHost == "" && !hostValid {
			continue
		}
		if explicitHost != "" && !prHostRE.MatchString(explicitHost) {
			continue
		}
		if !repoPathRE.MatchString(repo) {
			continue
		}
		selectedHost := ""
		if explicitHost != "" {
			selectedHost = explicitHost
		} else if hostSet {
			selectedHost = host
			if !specified && remoteHost != "" && !strings.EqualFold(host, remoteHost) {
				continue
			}
		} else if remoteHost != "" && !callHostUnknown {
			selectedHost = remoteHost
		}
		if selectedHost == "" && specified && !callHostUnknown {
			selectedHost = "github.com"
		}
		if selectedHost == "" {
			continue
		}
		if link, ok := NewPRLink(fmt.Sprintf("https://%s/%s/pull/%d", selectedHost, repo, number), repo, number); ok {
			links = append(links, []PRLink{link})
			if !specified || (explicitHost == "" && !hostSet) {
				dependsOnRemote = append(dependsOnRemote, link)
			}
		}
	}
	return CombinePRLinks(links...), directoryChanged, repositoryChanged, persistent, dependsOnRemote, nil
}

func hasCommandSubstitution(node syntax.Node) bool {
	found := false
	syntax.Walk(node, func(node syntax.Node) bool {
		if _, ok := node.(*syntax.CmdSubst); ok {
			found = true
		}
		return !found
	})
	return found
}

func bashChangesRepositoryContext(file *syntax.File) (bool, bool, bool, syntax.Pos, syntax.Pos) {
	var firstRepo, firstHost syntax.Pos
	var directory, repository, persistent bool
	functions := map[string]bool{}
	syntax.Walk(file, func(node syntax.Node) bool {
		if fn, ok := node.(*syntax.FuncDecl); ok {
			functions[fn.Name.Value] = true
		}
		return true
	})
	record := func(dst *syntax.Pos, pos syntax.Pos) {
		if !dst.IsValid() || pos.Offset() < dst.Offset() {
			*dst = pos
		}
	}
	syntax.Walk(file, func(node syntax.Node) bool {
		switch n := node.(type) {
		case *syntax.FuncDecl:
			record(&firstRepo, n.Pos())
		case *syntax.DeclClause:
			for _, a := range n.Args {
				if a.Name == nil {
					continue
				}
				if a.Name.Value == "GH_REPO" {
					record(&firstRepo, a.Pos())
				}
				if a.Name.Value == "GH_HOST" {
					record(&firstHost, a.Pos())
				}
			}
		case *syntax.CallExpr:
			words := make([]string, len(n.Args))
			for i, arg := range n.Args {
				words[i], _ = shellLiteral(arg.Parts)
			}
			name := ""
			if len(words) > 0 {
				name = words[0]
			}
			for _, a := range n.Assigns {
				if a.Name == nil {
					continue
				}
				if a.Name.Value == "GH_REPO" {
					record(&firstRepo, a.Pos())
				}
				if a.Name.Value == "GH_HOST" {
					literalPrefix := false
					if name == "gh" && len(words) > 1 && words[1] == "pr" && a.Value != nil {
						_, literalPrefix = shellLiteral(a.Value.Parts)
					}
					if !literalPrefix {
						record(&firstHost, a.Pos())
					}
				}
			}
			if name == "cd" || name == "pushd" || name == "popd" {
				record(&firstRepo, n.Pos())
			}
			if name == "eval" || name == "source" || name == "." || functions[name] {
				record(&firstRepo, n.Pos())
				record(&firstHost, n.Pos())
			}
			if repositoryMutation(words) {
				record(&firstRepo, n.Pos())
			}
			if name == "unset" {
				for _, word := range words[1:] {
					if word == "GH_HOST" {
						record(&firstHost, n.Pos())
					}
				}
			}
		}
		return true
	})
	syntax.Walk(file, func(node syntax.Node) bool {
		switch n := node.(type) {
		case *syntax.FuncDecl:
			return false
		case *syntax.CallExpr:
			words := make([]string, len(n.Args))
			for i, arg := range n.Args {
				words[i], _ = shellLiteral(arg.Parts)
			}
			if len(words) > 0 && (words[0] == "git" || words[0] == "gh") && repositoryMutation(words) {
				persistent = true
			}
		}
		return true
	})
	syntax.Walk(file, func(node syntax.Node) bool {
		switch n := node.(type) {
		case *syntax.Subshell, *syntax.FuncDecl, *syntax.CmdSubst, *syntax.ProcSubst:
			return false
		case *syntax.DeclClause:
			for _, a := range n.Args {
				if a.Name != nil && (a.Name.Value == "GH_HOST" || a.Name.Value == "GH_REPO") {
					repository = true
				}
			}
		case *syntax.CallExpr:
			words := make([]string, len(n.Args))
			for i, arg := range n.Args {
				words[i], _ = shellLiteral(arg.Parts)
			}
			for _, a := range n.Assigns {
				if len(words) == 0 && a.Name != nil && (a.Name.Value == "GH_HOST" || a.Name.Value == "GH_REPO") {
					repository = true
				}
			}
			if len(words) > 0 {
				name := words[0]
				if name == "cd" || name == "pushd" || name == "popd" {
					directory = true
				}
				if repositoryMutation(words) || functions[name] || name == "eval" || name == "source" || name == "." {
					repository = true
					persistent = persistent || name == "git" || name == "gh" || functions[name]
				}
			}
		}
		return true
	})
	return directory, repository || persistent, persistent, firstRepo, firstHost
}

func repositoryMutation(words []string) bool {
	if len(words) < 2 {
		return false
	}
	if words[0] == "unset" {
		for _, word := range words[1:] {
			if word == "GH_REPO" || word == "GH_HOST" {
				return true
			}
		}
	}
	if words[0] == "gh" && len(words) >= 3 && words[1] == "repo" && words[2] == "set-default" {
		for _, word := range words[3:] {
			if word == "--view" || word == "-v" {
				return false
			}
		}
		return true
	}
	if words[0] != "git" {
		return false
	}
	switch words[1] {
	case "config":
		for _, word := range words[2:] {
			if word == "--get" || word == "--get-all" || word == "--list" || word == "-l" {
				return false
			}
		}
		return len(words) > 3
	case "remote":
		return len(words) > 2 && (words[2] == "add" || words[2] == "remove" || words[2] == "rename" || words[2] == "set-url")
	}
	return false
}
