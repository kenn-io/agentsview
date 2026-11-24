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
	"lock":     "--reason -r",
	"revert":   "--body -b --body-file -F --title -t",
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

// AnalyzePRCommand extracts links and shared shell context from one parse.
func AnalyzePRCommand(ctx context.Context, command, cwd, remote string, allowBare, hostUnknown bool) ([]PRLink, bool, bool, bool, []PRLink, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, false, false, nil, err
	}
	file, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil {
		return nil, false, false, false, nil, ctx.Err()
	}
	directoryChanged, repositoryChanged, persistent, firstRepo, firstHost, calls := bashChangesRepositoryContext(file, cwd)
	remoteHost, remoteRepo := "", ""
	if normalized, ok := export.NormalizeGitRemote(remote); ok {
		remoteHost, remoteRepo, _ = strings.Cut(normalized, "/")
	}
	var links [][]PRLink
	var dependsOnRemote []PRLink
	for _, entry := range calls {
		call := entry.call
		if err := ctx.Err(); err != nil {
			return nil, false, false, false, nil, err
		}
		bareSafe := allowBare && (firstRepo < 0 || entry.end < firstRepo) && !hasCommandSubstitution(call)
		host, hostSet, hostValid := "", false, true
		envRepo, envRepoSet, envRepoValid := "", false, false
		for _, a := range append(append([]*syntax.Assign(nil), entry.assigns...), call.Assigns...) {
			if a.Name == nil {
				continue
			}
			if a.Name.Value == "GH_REPO" {
				bareSafe = false
				envRepoSet = true
				if a.Value != nil {
					envRepo, envRepoValid = shellLiteral(a.Value.Parts)
				} else {
					envRepo, envRepoValid = "", false
				}
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
		callHostUnknown := hostUnknown || firstHost >= 0 && entry.end >= firstHost
		if words[2] == "co" {
			words[2] = "checkout"
		}
		switch words[2] {
		case "view", "checkout", "edit", "diff", "checks", "comment", "review", "ready", "merge", "close", "reopen", "update-branch", "lock", "unlock", "revert":
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
		number, err := strconv.Atoi(strings.TrimPrefix(selector, "#"))
		if err != nil || number <= 0 || invalid {
			continue
		}
		if !specified {
			if envRepoSet {
				if !envRepoValid {
					continue
				}
				repo, specified = envRepo, true
			} else if !bareSafe {
				continue
			} else {
				repo = remoteRepo
			}
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

// SamePRWorkingDirectory compares recorded checkout paths across platforms.
func SamePRWorkingDirectory(cwd, dir string) bool {
	return cwd != "" && dir != "" && path.Clean(strings.ReplaceAll(cwd, `\`, "/")) == path.Clean(strings.ReplaceAll(dir, `\`, "/"))
}

type prShellCall struct {
	call    *syntax.CallExpr
	end     int
	assigns []*syntax.Assign
}

func bashChangesRepositoryContext(file *syntax.File, cwd string) (bool, bool, bool, int, int, []prShellCall) {
	type scope struct {
		local   bool
		call    int
		assigns []*syntax.Assign
	}
	type event struct {
		node      syntax.Node
		scope     scope
		uncertain bool
	}
	var events []event
	var stack []scope
	functions := make(map[string]*syntax.Stmt)
	active := make(map[string]bool)
	expansions := 0
	// Bound the combined traversal and parsing work from expanded shell bodies.
	remaining := 100000
	var calls []prShellCall
	var walk func(syntax.Node, bool)
	walk = func(root syntax.Node, expanded bool) {
		syntax.Walk(root, func(node syntax.Node) bool {
			if node == nil {
				if call := stack[len(stack)-1].call; call >= 0 {
					calls[call].end = len(events) - 1
				}
				stack = stack[:len(stack)-1]
				return true
			}
			if expanded {
				cost := 1 + int(node.End().Offset()-node.Pos().Offset())
				if cost > remaining {
					remaining = 0
					return false
				}
				remaining -= cost
			}
			var current scope
			if len(stack) > 0 {
				current = stack[len(stack)-1]
			}
			current.call = -1
			switch n := node.(type) {
			case *syntax.FuncDecl:
				functions[n.Name.Value] = n.Body
				return false
			case *syntax.Subshell, *syntax.CmdSubst, *syntax.ProcSubst:
				current.local = true
			case *syntax.CallExpr:
				uncertain := false
				if len(n.Args) >= 3 {
					name, nameOK := shellLiteral(n.Args[0].Parts)
					option, optionOK := shellLiteral(n.Args[1].Parts)
					script, scriptOK := shellLiteral(n.Args[2].Parts)
					if nameOK && optionOK && scriptOK && (path.Base(name) == "bash" || path.Base(name) == "sh") && (option == "-c" || option == "-lc") {
						if expansions >= 4096 || len(script) > remaining {
							uncertain = true
						} else if nested, err := syntax.NewParser().Parse(strings.NewReader(script), ""); err == nil {
							remaining -= len(script)
							child := scope{local: true, call: -1, assigns: append(append([]*syntax.Assign(nil), current.assigns...), n.Assigns...)}
							stack = append(stack, child)
							expansions++
							walk(nested, true)
							stack = stack[:len(stack)-1]
							uncertain = remaining == 0
						}
					}
				}
				if len(n.Args) > 0 {
					name, literal := shellLiteral(n.Args[0].Parts)
					if body := functions[name]; literal && body != nil && !active[name] {
						if expansions >= 4096 || remaining == 0 {
							uncertain = true
						} else {
							child := scope{local: current.local, call: -1, assigns: append(append([]*syntax.Assign(nil), current.assigns...), n.Assigns...)}
							stack = append(stack, child)
							active[name] = true
							expansions++
							walk(body, true)
							delete(active, name)
							stack = stack[:len(stack)-1]
							uncertain = uncertain || remaining == 0
						}
					}
				}
				current.call = len(calls)
				calls = append(calls, prShellCall{n, len(events), current.assigns})
				events = append(events, event{node, current, uncertain})
			case *syntax.DeclClause:
				events = append(events, event{node, current, false})
			}
			stack = append(stack, current)
			return true
		})
	}
	walk(file, false)
	firstRepo, firstHost := -1, -1
	var directory, repository, persistent bool
	record := func(dst *int, index int) {
		if *dst < 0 || index < *dst {
			*dst = index
		}
	}
	for index, item := range events {
		if item.uncertain {
			record(&firstRepo, index)
			record(&firstHost, index)
			repository, persistent = true, true
		}
		switch n := item.node.(type) {
		case *syntax.DeclClause:
			for _, a := range n.Args {
				if a.Name == nil {
					continue
				}
				if a.Name.Value == "GH_REPO" {
					record(&firstRepo, index)
				}
				if a.Name.Value == "GH_HOST" {
					record(&firstHost, index)
				}
				if !item.scope.local && (a.Name.Value == "GH_REPO" || a.Name.Value == "GH_HOST") {
					repository = true
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
				if a.Name.Value == "GH_REPO" && len(words) == 0 {
					record(&firstRepo, index)
				}
				if a.Name.Value == "GH_HOST" {
					literalPrefix := false
					if name == "gh" && len(words) > 1 && words[1] == "pr" && a.Value != nil {
						_, literalPrefix = shellLiteral(a.Value.Parts)
					}
					if !literalPrefix {
						record(&firstHost, index)
					}
				}
				if !item.scope.local && len(words) == 0 && (a.Name.Value == "GH_REPO" || a.Name.Value == "GH_HOST") {
					repository = true
				}
			}
			dir := ""
			if name == "cd" && len(n.Args) == 2 {
				dir = strings.ReplaceAll(words[1], `\`, "/")
				if dir != "" && !path.IsAbs(dir) && (len(dir) <= 1 || dir[1] != ':') {
					dir = path.Join(strings.ReplaceAll(cwd, `\`, "/"), dir)
				}
			}
			if (name == "cd" || name == "pushd" || name == "popd") && !SamePRWorkingDirectory(cwd, dir) {
				record(&firstRepo, index)
				if !item.scope.local {
					directory = true
				}
			}
			if name == "eval" || name == "source" || name == "." || functions[name] != nil {
				record(&firstRepo, index)
				record(&firstHost, index)
			}
			if repositoryMutation(words) {
				record(&firstRepo, index)
			}
			if name == "unset" {
				for _, word := range words[1:] {
					if word == "GH_HOST" {
						record(&firstHost, index)
					}
				}
			}
			if (name == "git" || name == "gh") && repositoryMutation(words) {
				persistent = true
			}
			if !item.scope.local && (repositoryMutation(words) || functions[name] != nil || name == "eval" || name == "source" || name == ".") {
				repository = true
				persistent = persistent || functions[name] != nil
			}
		}
	}
	return directory, repository || persistent, persistent, firstRepo, firstHost, calls
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
		write := false
		for _, word := range words[2:] {
			if word == "--get" || word == "--get-all" || word == "--list" || word == "-l" {
				return false
			}
			if word == "--add" || word == "--replace-all" || word == "--unset" || word == "--unset-all" || word == "--rename-section" || word == "--remove-section" || word == "set" || word == "unset" {
				write = true
			}
		}
		for i, word := range words[2:] {
			key := strings.ToLower(word)
			if strings.HasPrefix(key, "remote.") || strings.HasPrefix(key, "branch.") || strings.HasPrefix(key, "url.") {
				return write || i+3 < len(words)
			}
		}
		return false
	case "remote":
		return len(words) > 2 && (words[2] == "add" || words[2] == "remove" || words[2] == "rename" || words[2] == "set-url")
	}
	return false
}
