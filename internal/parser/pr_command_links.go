package parser

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"sort"
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
	directoryChanged, repositoryChanged, persistent, repoEnv, hostEnv, calls := bashChangesRepositoryContext(file, cwd)
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
		bareSafe := allowBare && (repoEnv.first < 0 || entry.end < repoEnv.first) && !hasCommandSubstitution(call)
		env := callEnvAssigns(entry, repoEnv, hostEnv)
		words := make([]string, len(call.Args))
		literal := make([]bool, len(call.Args))
		for i, word := range call.Args {
			words[i], literal[i] = shellLiteral(word.Parts)
		}
		if len(words) < 4 || !literal[0] || !literal[1] || !literal[2] || words[0] != "gh" || words[1] != "pr" {
			continue
		}
		callHostUnknown := hostUnknown || hostEnv.first >= 0 && entry.end >= hostEnv.first
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
			if env.repoSet {
				if !env.repoValid {
					continue
				}
				repo, specified = env.repo, true
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
		if explicitHost == "" && !env.hostValid {
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
		} else if env.hostSet {
			selectedHost = env.host
			if !specified && remoteHost != "" && !strings.EqualFold(env.host, remoteHost) {
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
			if !specified || (explicitHost == "" && !env.hostSet) {
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
	call                 *syntax.CallExpr
	end                  int
	assigns              []*syntax.Assign
	repoSince, hostSince int
}

// prEnvTrack records the first event that may alter a variable's meaning and
// every event that changes it.
type prEnvTrack struct {
	first   int
	changes []int
}

func (t *prEnvTrack) record(index int, change bool) {
	if t.first < 0 || index < t.first {
		t.first = index
	}
	if change {
		t.changes = append(t.changes, index)
	}
}

func (t prEnvTrack) changedBetween(since, through int) bool {
	i := sort.SearchInts(t.changes, since)
	return i < len(t.changes) && t.changes[i] <= through
}

type prShellScope struct {
	local                bool
	call                 int
	assigns              []*syntax.Assign
	repoSince, hostSince int
}

func childPRScope(parent prShellScope, assigns []*syntax.Assign, local bool, at int) prShellScope {
	child := prShellScope{local: local, call: -1, assigns: append(append([]*syntax.Assign(nil), parent.assigns...), assigns...), repoSince: parent.repoSince, hostSince: parent.hostSince}
	for _, a := range assigns {
		if a.Name != nil && a.Name.Value == "GH_REPO" {
			child.repoSince = at
		}
		if a.Name != nil && a.Name.Value == "GH_HOST" {
			child.hostSince = at
		}
	}
	return child
}

type prEnvAssigns struct {
	repo, host                             string
	repoSet, repoValid, hostSet, hostValid bool
}

// callEnvAssigns resolves the GH_REPO and GH_HOST a call sees, dropping an
// inherited value that an expanded shell changed before the call.
func callEnvAssigns(entry prShellCall, repoEnv, hostEnv prEnvTrack) prEnvAssigns {
	env := prEnvAssigns{hostValid: true}
	for group, assigns := range [][]*syntax.Assign{entry.assigns, entry.call.Assigns} {
		if group == 1 {
			env.repoValid = env.repoValid && !(env.repoSet && repoEnv.changedBetween(entry.repoSince, entry.end))
			env.hostValid = env.hostValid && !(env.hostSet && hostEnv.changedBetween(entry.hostSince, entry.end))
		}
		for _, a := range assigns {
			if a.Name == nil {
				continue
			}
			if a.Name.Value == "GH_REPO" {
				env.repoSet = true
				env.repo, env.repoValid = "", false
				if a.Value != nil {
					env.repo, env.repoValid = shellLiteral(a.Value.Parts)
				}
			}
			if a.Name.Value == "GH_HOST" {
				env.hostSet = true
				env.host, env.hostValid = "", false
				if a.Value != nil {
					env.host, env.hostValid = shellLiteral(a.Value.Parts)
					env.hostValid = env.hostValid && env.host != ""
				}
			}
		}
	}
	return env
}

func bashChangesRepositoryContext(file *syntax.File, cwd string) (bool, bool, bool, prEnvTrack, prEnvTrack, []prShellCall) {
	type event struct {
		node      syntax.Node
		scope     prShellScope
		uncertain bool
	}
	var events []event
	var stack []prShellScope
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
			var current prShellScope
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
							child := childPRScope(current, n.Assigns, true, len(events))
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
							child := childPRScope(current, n.Assigns, current.local, len(events))
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
				calls = append(calls, prShellCall{n, len(events), current.assigns, current.repoSince, current.hostSince})
				events = append(events, event{node, current, uncertain})
			case *syntax.DeclClause:
				events = append(events, event{node, current, false})
			}
			stack = append(stack, current)
			return true
		})
	}
	walk(file, false)
	repoEnv, hostEnv := prEnvTrack{first: -1}, prEnvTrack{first: -1}
	var directory, repository, persistent bool
	for index, item := range events {
		if item.uncertain {
			repoEnv.record(index, true)
			hostEnv.record(index, true)
			repository, persistent = true, true
		}
		switch n := item.node.(type) {
		case *syntax.DeclClause:
			for _, a := range n.Args {
				if a.Name == nil {
					continue
				}
				if a.Name.Value == "GH_REPO" {
					repoEnv.record(index, true)
				}
				if a.Name.Value == "GH_HOST" {
					hostEnv.record(index, true)
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
					repoEnv.record(index, true)
				}
				if a.Name.Value == "GH_HOST" {
					literalPrefix := false
					if name == "gh" && len(words) > 1 && words[1] == "pr" && a.Value != nil {
						_, literalPrefix = shellLiteral(a.Value.Parts)
					}
					if !literalPrefix {
						hostEnv.record(index, len(words) == 0)
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
				repoEnv.record(index, false)
				if !item.scope.local {
					directory = true
				}
			}
			if name == "eval" || name == "source" || name == "." || functions[name] != nil {
				repoEnv.record(index, true)
				hostEnv.record(index, true)
			}
			if repositoryMutation(words) {
				repoEnv.record(index, false)
			}
			if name == "unset" {
				for _, word := range words[1:] {
					if word == "GH_REPO" {
						repoEnv.record(index, true)
					}
					if word == "GH_HOST" {
						hostEnv.record(index, true)
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
	return directory, repository || persistent, persistent, repoEnv, hostEnv, calls
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
	return words[0] == "git" && gitRepositoryMutation(words[1:])
}

// gitRepositoryMutation reports whether git's subcommand, after its global
// options, can change a repository's remotes or branches.
func gitRepositoryMutation(words []string) bool {
	i := 0
	for i < len(words) && strings.HasPrefix(words[i], "-") {
		option := words[i]
		i++
		switch option {
		case "-C", "-c", "--git-dir", "--work-tree", "--namespace", "--config-env", "--super-prefix":
			if i >= len(words) {
				return true
			}
			i++
		}
	}
	if i >= len(words) {
		return false
	}
	words = words[i:]
	switch words[0] {
	case "config":
		return gitConfigMutation(words[1:])
	case "remote":
		return len(words) > 1 && (words[1] == "add" || words[1] == "remove" || words[1] == "rename" || words[1] == "set-url")
	}
	return false
}

func gitConfigMutation(args []string) bool {
	write := false
	for _, word := range args {
		if word == "--get" || word == "--get-all" || word == "--list" || word == "-l" {
			return false
		}
		if word == "--add" || word == "--replace-all" || word == "--unset" || word == "--unset-all" || word == "--rename-section" || word == "--remove-section" || word == "set" || word == "unset" {
			write = true
		}
	}
	for i, word := range args {
		key := strings.ToLower(word)
		if strings.HasPrefix(key, "remote.") || strings.HasPrefix(key, "branch.") || strings.HasPrefix(key, "url.") {
			return write || i+1 < len(args)
		}
	}
	return false
}
