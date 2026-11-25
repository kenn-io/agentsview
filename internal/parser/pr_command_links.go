package parser

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

var (
	repoPathRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	prHostRE   = regexp.MustCompile(`^[A-Za-z0-9.-]+(?::[0-9]+)?$`)
)

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

func shellWords(args []*syntax.Word) ([]string, []bool) {
	words := make([]string, len(args))
	literal := make([]bool, len(args))
	for i, word := range args {
		words[i], literal[i] = shellLiteral(word.Parts)
	}
	return words, literal
}

// quotedValue stays one argument when it contains a simple shell variable.
func quotedValue(word *syntax.Word) bool {
	for _, part := range word.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			if strings.ContainsAny(p.Value, "*?[~") {
				return false
			}
		case *syntax.SglQuoted:
		case *syntax.DblQuoted:
			if p.Dollar {
				return false
			}
			for _, inner := range p.Parts {
				switch v := inner.(type) {
				case *syntax.Lit:
				case *syntax.ParamExp:
					if v.Param == nil || v.Param.Value == "@" || v.Param.Value == "*" || v.Index != nil || v.Exp != nil || v.Repl != nil || v.Slice != nil || v.NestedParam != nil {
						return false
					}
				default:
					return false
				}
			}
		default:
			return false
		}
	}
	return true
}

// gh flags differ by subcommand, including short flags that take a value in
// one command and are boolean in another. Unknown flags are ambiguous.
func prCommandFlag(command, flag string) (takesValue, ok bool) {
	flags := map[string][2]string{
		"view":          {"-q --jq --json -t --template", "-c --comments -w --web"},
		"checkout":      {"-b --branch --worktree", "--detach -f --force --recurse-submodules"},
		"edit":          {"--add-assignee --add-label --add-project --add-reviewer --attach -B --base -b --body -F --body-file -m --milestone --remove-assignee --remove-label --remove-project --remove-reviewer -t --title", "--remove-milestone"},
		"diff":          {"--color -e --exclude", "--allow-escape-sequences --name-only --patch -w --web"},
		"checks":        {"-i --interval -q --jq --json -t --template", "--fail-fast --required --watch -w --web"},
		"comment":       {"--attach -b --body -F --body-file", "--create-if-none --delete-last --edit-last -e --editor -w --web --yes"},
		"review":        {"-b --body -F --body-file", "-a --approve -c --comment -r --request-changes"},
		"ready":         {"", "--undo"},
		"merge":         {"-A --author-email -b --body -F --body-file --match-head-commit -t --subject", "--admin --auto -d --delete-branch --disable-auto -m --merge -r --rebase -s --squash"},
		"close":         {"-c --comment", "-d --delete-branch"},
		"reopen":        {"-c --comment", ""},
		"update-branch": {"", "--rebase"},
		"lock":          {"-r --reason", ""},
		"unlock":        {"", ""},
		"revert":        {"-b --body -F --body-file -t --title", "-d --draft"},
	}
	for kind, list := range flags[command] {
		for name := range strings.FieldsSeq(list) {
			if flag == name {
				return kind == 0, true
			}
		}
	}
	return false, false
}

// unwrapPRCommand removes execution wrappers whose arguments have fixed roles.
func unwrapPRCommand(args []*syntax.Word) []*syntax.Word {
	for len(args) > 0 {
		words, literal := shellWords(args)
		if !literal[0] {
			return nil
		}
		switch words[0] {
		case "command", "env":
			args = args[wrappedCommandStart(words, literal):]
		case "timeout":
			i := 1
			for i < len(words) && literal[i] && strings.HasPrefix(words[i], "-") {
				flag := words[i]
				i++
				switch flag {
				case "--":
					goto duration
				case "-k", "--kill-after", "-s", "--signal":
					i++
				case "--foreground", "--preserve-status", "-v", "--verbose":
				default:
					if !strings.HasPrefix(flag, "--kill-after=") && !strings.HasPrefix(flag, "--signal=") {
						return nil
					}
				}
			}
		duration:
			if i >= len(words) || !literal[i] || words[i] == "" {
				return nil
			}
			args = args[i+1:]
		default:
			return args
		}
	}
	return nil
}

func wrappedCommandStart(words []string, literal []bool) int {
	i := 1
	for i < len(words) && literal[i] && (words[0] == "command" && words[i] == "-p" || words[0] == "env" && strings.Contains(words[i], "=")) {
		i++
	}
	if i < len(words) && literal[i] && words[i] == "--" {
		i++
	}
	return i
}

// ghPRArgs moves inherited repository flags after the PR action.
func ghPRArgs(args []*syntax.Word) []*syntax.Word {
	words, literal := shellWords(args)
	if len(words) == 0 || !literal[0] || words[0] != "gh" {
		return nil
	}
	i := 1
	for i < len(words) && literal[i] && strings.HasPrefix(words[i], "-") {
		switch {
		case words[i] == "-R" || words[i] == "--repo":
			i += 2
		case strings.HasPrefix(words[i], "-R") && len(words[i]) > 2, strings.HasPrefix(words[i], "--repo="):
			i++
		default:
			return nil
		}
	}
	if i+1 >= len(args) || !literal[i] || !literal[i+1] || words[i] != "pr" {
		return nil
	}
	result := append([]*syntax.Word{args[0], args[i], args[i+1]}, args[1:i]...)
	return append(result, args[i+2:]...)
}

func shellCommand(args []*syntax.Word) (string, bool) {
	words, literal := shellWords(args)
	if len(words) < 3 || !literal[0] {
		return "", false
	}
	switch path.Base(words[0]) {
	case "sh", "bash", "zsh":
	default:
		return "", false
	}
	for i := 1; i < len(words); i++ {
		if !literal[i] || !strings.HasPrefix(words[i], "-") || strings.HasPrefix(words[i], "--") {
			return "", false
		}
		if strings.Contains(words[i][1:], "n") {
			return "", false
		}
		if strings.Contains(words[i][1:], "c") {
			if i+1 < len(words) && literal[i+1] {
				return words[i+1], true
			}
			return "", false
		}
	}
	return "", false
}

// AnalyzePRCommand finds PR links in a single shell invocation.
func AnalyzePRCommand(ctx context.Context, command string) ([]PRLink, error) {
	return analyzePRCommand(ctx, command, strings.Contains(command, "GH_HOST"), 0)
}

func analyzePRCommand(ctx context.Context, command string, hostUnknown bool, depth int) ([]PRLink, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil {
		return nil, ctx.Err()
	}
	var calls []*syntax.CallExpr
	syntax.Walk(file, func(node syntax.Node) bool {
		if _, ok := node.(*syntax.FuncDecl); ok {
			return false
		}
		if call, ok := node.(*syntax.CallExpr); ok {
			calls = append(calls, call)
		}
		return true
	})
	var links [][]PRLink
	for _, call := range calls {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		args := unwrapPRCommand(call.Args)
		if inner, ok := shellCommand(args); ok && depth < 8 {
			found, err := analyzePRCommand(ctx, inner, hostUnknown || strings.Contains(inner, "GH_HOST"), depth+1)
			if err != nil {
				return nil, err
			}
			links = append(links, found)
			continue
		}
		args = ghPRArgs(args)
		if args == nil {
			continue
		}
		words, literal := shellWords(args)
		if len(words) < 4 || !literal[0] || !literal[1] || !literal[2] || words[0] != "gh" || words[1] != "pr" {
			continue
		}
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
				// An attached, quoted value cannot add shell arguments.
				if len(args[i].Parts) > 1 && quotedValue(args[i]) {
					if prefix, ok := args[i].Parts[0].(*syntax.Lit); ok {
						flag, _, attached := strings.Cut(prefix.Value, "=")
						takesValue, known := prCommandFlag(words[2], flag)
						if attached && known && takesValue {
							continue
						}
					}
				}
				invalid = true
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
				flag, _, attached := strings.Cut(word, "=")
				if !strings.HasPrefix(flag, "--") && len(flag) > 2 {
					flag, attached = flag[:2], true
				}
				takesValue, ok := prCommandFlag(words[2], flag)
				if !ok && selector != "" {
					// An unknown option may consume a later repository flag.
					for j := i + 1; j < len(words); j++ {
						if !literal[j] || words[j] == "--repo" || strings.HasPrefix(words[j], "--repo=") || strings.HasPrefix(words[j], "-R") {
							invalid = true
						}
					}
					i = len(words)
				} else if !ok || (attached && !takesValue) {
					invalid = true
				} else if takesValue && !attached {
					if i+1 >= len(words) || (!literal[i+1] && !quotedValue(args[i+1])) {
						invalid = true
					} else {
						i++
					}
				}
			case selector == "":
				selector = word
			}
		}
		if invalid {
			continue
		}
		if strings.HasPrefix(selector, "https://") || strings.HasPrefix(selector, "http://") {
			found, err := PromptPRLinksContext(ctx, selector)
			if err != nil {
				return nil, err
			}
			links = append(links, found)
			continue
		}
		number, err := strconv.Atoi(strings.TrimPrefix(selector, "#"))
		if err != nil || number <= 0 {
			continue
		}
		if !specified {
			continue
		}
		explicitHost, repo, valid := splitPRRepository(repo)
		if !valid {
			continue
		}
		selectedHost := explicitHost
		if selectedHost == "" && !hostUnknown {
			selectedHost = "github.com"
		}
		if selectedHost == "" {
			continue
		}
		if link, ok := NewPRLink(fmt.Sprintf("https://%s/%s/pull/%d", selectedHost, repo, number), repo, number); ok {
			links = append(links, []PRLink{link})
		}
	}
	return CombinePRLinks(links...), nil
}

// splitPRRepository separates an explicit host from a -R repository and
// reports whether both parts are well formed.
func splitPRRepository(repo string) (host, name string, valid bool) {
	if strings.Count(repo, "/") == 2 {
		host, repo, _ = strings.Cut(repo, "/")
	}
	return host, repo, (host == "" || prHostRE.MatchString(host)) && repoPathRE.MatchString(repo)
}
