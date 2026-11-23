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
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil {
		return nil, nil
	}
	type segment struct {
		stmt   *syntax.Stmt
		nested bool
	}
	var segments []segment
	var collect func(*syntax.Stmt, bool)
	var collectIf func(*syntax.IfClause)
	collectIf = func(clause *syntax.IfClause) {
		for _, stmt := range clause.Cond {
			collect(stmt, true)
		}
		result, known := literalCondition(clause.Cond)
		if known && result {
			for _, stmt := range clause.Then {
				collect(stmt, true)
			}
		} else if known && clause.Else != nil {
			collectIf(clause.Else)
		}
	}
	collect = func(stmt *syntax.Stmt, nested bool) {
		switch cmd := stmt.Cmd.(type) {
		case *syntax.BinaryCmd:
			collect(cmd.X, nested)
			collect(cmd.Y, nested)
		case *syntax.IfClause:
			collectIf(cmd)
		case *syntax.WhileClause:
			for _, child := range cmd.Cond {
				collect(child, true)
			}
		case *syntax.ForClause, *syntax.CaseClause:
		case *syntax.Block:
			for _, child := range cmd.Stmts {
				collect(child, true)
			}
		case *syntax.Subshell:
			for _, child := range cmd.Stmts {
				collect(child, true)
			}
		default:
			segments = append(segments, segment{stmt, nested})
		}
	}
	for _, stmt := range file.Stmts {
		collect(stmt, false)
	}
	remoteHost, remoteRepo := "", ""
	if normalized, ok := export.NormalizeGitRemote(remote); ok {
		remoteHost, remoteRepo, _ = strings.Cut(normalized, "/")
	}
	shellHost, shellHostSet, shellHostValid := "", false, true
	var links [][]PRLink
	bareSafe := allowBare && !strings.Contains(command, "GH_REPO")
	for _, segment := range segments {
		stmt := segment.stmt
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
					if len(stmt.Redirs) > 0 && a.Name != nil && a.Name.Value == "GH_HOST" {
						hostValid = false
					}
				}
				if segment.nested && hostSet {
					bareSafe = false
				}
				if !segment.nested {
					shellHost, shellHostSet, shellHostValid = host, hostSet, hostValid
				}
			}
			continue
		}
		call, ok := stmt.Cmd.(*syntax.CallExpr)
		if !ok {
			continue
		}
		if hasCommandSubstitution(call) {
			bareSafe = false
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
			if literal[0] && words[0] == "cd" {
				remoteHost, remoteRepo = "", ""
			}
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
		selectedHost := ""
		if explicitHost != "" {
			selectedHost = explicitHost
		} else if hostSet {
			selectedHost = host
			if remoteHost != "" && !strings.EqualFold(host, remoteHost) {
				continue
			}
		} else if remoteHost != "" {
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

func literalCondition(stmts []*syntax.Stmt) (bool, bool) {
	if len(stmts) == 0 {
		return true, true // else
	}
	if len(stmts) != 1 || stmts[0].Negated || len(stmts[0].Redirs) != 0 {
		return false, false
	}
	call, ok := stmts[0].Cmd.(*syntax.CallExpr)
	if !ok || len(call.Assigns) != 0 || len(call.Args) != 1 {
		return false, false
	}
	word, literal := shellLiteral(call.Args[0].Parts)
	return word == "true", literal && (word == "true" || word == "false")
}

// BashChangesWorkingDirectory reports literal cd commands in a shared Bash call.
func BashChangesWorkingDirectory(command string) bool {
	file, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil {
		return false
	}
	changed := false
	syntax.Walk(file, func(node syntax.Node) bool {
		if changed {
			return false
		}
		switch call := node.(type) {
		case *syntax.Subshell, *syntax.FuncDecl, *syntax.CmdSubst, *syntax.ProcSubst:
			return false
		case *syntax.CallExpr:
			if len(call.Args) == 0 || len(call.Args) > 2 {
				return false
			}
			name, literal := shellLiteral(call.Args[0].Parts)
			if !literal || name != "cd" {
				return false
			}
			if len(call.Args) == 1 {
				changed = true
				return false
			}
			_, changed = shellLiteral(call.Args[1].Parts)
			return false
		}
		return !changed
	})
	return changed
}
