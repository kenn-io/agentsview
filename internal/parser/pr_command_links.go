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
func CommandPRLinks(ctx context.Context, command, remote string) ([]PRLink, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil {
		return nil, nil
	}
	var segments []*syntax.Stmt
	var collect func(*syntax.Stmt)
	collect = func(stmt *syntax.Stmt) {
		if binary, ok := stmt.Cmd.(*syntax.BinaryCmd); ok {
			collect(binary.X)
			collect(binary.Y)
			return
		}
		segments = append(segments, stmt)
	}
	for _, stmt := range file.Stmts {
		collect(stmt)
	}
	remoteHost, remoteRepo, _ := strings.Cut(remote, "/")
	if normalized, ok := export.NormalizeGitRemote(remote); ok {
		remoteHost, remoteRepo, _ = strings.Cut(normalized, "/")
	} else {
		remoteHost, remoteRepo = "", ""
	}
	shellHost, shellHostSet, shellHostValid := "", false, true
	var links [][]PRLink
	bareSafe := !strings.ContainsAny(command, "$`(") && !strings.Contains(command, "GH_REPO")
	for _, stmt := range segments {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		host, hostSet, hostValid := shellHost, shellHostSet, shellHostValid
		recordHost := func(a *syntax.Assign) {
			if a.Name == nil || a.Name.Value != "GH_HOST" {
				return
			}
			hostSet = true
			if a.Value == nil {
				hostValid = false
				return
			}
			value, ok := shellLiteral(a.Value.Parts)
			if !ok || value == "" || (host != "" && !strings.EqualFold(host, value)) {
				hostValid = false
				return
			}
			host = value
		}
		if decl, ok := stmt.Cmd.(*syntax.DeclClause); ok {
			if decl.Variant.Value == "export" {
				for _, a := range decl.Args {
					recordHost(a)
				}
				shellHost, shellHostSet, shellHostValid = host, hostSet, hostValid
			}
			continue
		}
		call, ok := stmt.Cmd.(*syntax.CallExpr)
		if !ok {
			continue
		}
		for _, a := range call.Assigns {
			recordHost(a)
		}
		words := make([]string, len(call.Args))
		literal := make([]bool, len(call.Args))
		for i, word := range call.Args {
			words[i], literal[i] = shellLiteral(word.Parts)
		}
		if len(words) == 0 {
			continue
		}
		if !literal[0] || words[0] != "gh" {
			if words[0] != "echo" {
				bareSafe = false
			}
			continue
		}
		if len(words) < 4 || !literal[1] || !literal[2] || words[1] != "pr" {
			bareSafe = false
			continue
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
				return nil, err
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
		matchingRemote := remoteRepo != "" && len(CombinePRLinks([]PRLink{{Host: remoteHost, Repository: remoteRepo, Number: number}}, []PRLink{{Host: remoteHost, Repository: repo, Number: number}})) == 1
		selectedHost := ""
		if explicitHost != "" {
			selectedHost = explicitHost
		} else if hostSet {
			selectedHost = host
			if matchingRemote && !strings.EqualFold(host, remoteHost) {
				continue
			}
		} else if matchingRemote {
			selectedHost = remoteHost
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
