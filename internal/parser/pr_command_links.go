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

func shellLiteralPrefix(parts []syntax.WordPart) (string, bool) {
	var prefix strings.Builder
	for _, part := range parts {
		if quoted, ok := part.(*syntax.DblQuoted); ok && !quoted.Dollar {
			value, complete := shellLiteralPrefix(quoted.Parts)
			prefix.WriteString(value)
			if !complete {
				return prefix.String(), false
			}
			continue
		}
		value, complete := shellLiteral([]syntax.WordPart{part})
		prefix.WriteString(value)
		if !complete {
			return prefix.String(), false
		}
	}
	return prefix.String(), true
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
	directoryChanged, repositoryChanged, calls := bashChangesRepositoryContext(file)
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
		bareSafe := allowBare && !directoryChanged && !repositoryChanged
		words := make([]string, len(call.Args))
		literal := make([]bool, len(call.Args))
		for i, word := range call.Args {
			words[i], literal[i] = shellLiteral(word.Parts)
		}
		if len(words) < 4 || !literal[0] || !literal[1] || !literal[2] || words[0] != "gh" || words[1] != "pr" {
			continue
		}
		callHostUnknown := hostUnknown || repositoryChanged
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
				prefix, _ := shellLiteralPrefix(call.Args[i].Parts)
				flag, _, attached := strings.Cut(prefix, "=")
				if (attached || len(flag) == 2 && strings.HasPrefix(flag, "-")) && strings.Contains(" "+ghPRValueFlags[words[2]]+" ", " "+flag+" ") {
					continue
				}
				if selector == "" {
					invalid = true
				}
				if strings.HasPrefix(prefix, "-R") || strings.HasPrefix(prefix, "--repo") {
					invalid = true
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
			if !bareSafe {
				continue
			}
			repo = remoteRepo
		}
		explicitHost := ""
		if specified && strings.Count(repo, "/") == 2 {
			explicitHost, repo, _ = strings.Cut(repo, "/")
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
			if !specified || explicitHost == "" {
				dependsOnRemote = append(dependsOnRemote, link)
			}
		}
	}
	return CombinePRLinks(links...), directoryChanged, repositoryChanged, repositoryChanged, dependsOnRemote, nil
}

// SamePRWorkingDirectory compares recorded checkout paths across platforms.
func SamePRWorkingDirectory(cwd, dir string) bool {
	return cwd != "" && dir != "" && path.Clean(strings.ReplaceAll(cwd, `\`, "/")) == path.Clean(strings.ReplaceAll(dir, `\`, "/"))
}

func bashChangesRepositoryContext(file *syntax.File) (bool, bool, []*syntax.CallExpr) {
	functions := make(map[string]bool)
	syntax.Walk(file, func(node syntax.Node) bool {
		if fn, ok := node.(*syntax.FuncDecl); ok {
			functions[fn.Name.Value] = true
			return false
		}
		return true
	})
	var directory, repository bool
	var calls []*syntax.CallExpr
	syntax.Walk(file, func(node syntax.Node) bool {
		switch n := node.(type) {
		case *syntax.FuncDecl:
			return false
		case *syntax.Assign:
			if n.Name != nil && (n.Name.Value == "GH_REPO" || n.Name.Value == "GH_HOST") {
				repository = true
			}
		case *syntax.CallExpr:
			calls = append(calls, n)
			words := make([]string, len(n.Args))
			for i, arg := range n.Args {
				words[i], _ = shellLiteral(arg.Parts)
			}
			for len(words) > 1 && (words[0] == "command" || words[0] == "builtin") {
				words = words[1:]
			}
			if len(words) > 0 {
				name := words[0]
				directory = directory || name == "cd" || name == "pushd" || name == "popd"
				repository = repository || name == "eval" || name == "source" || name == "." || functions[name] || repositoryMutation(words)
			}
		}
		return true
	})
	return directory, repository, calls
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
