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

var repoPathRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

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
	bare := len(segments) == 1 && !strings.ContainsAny(command, "$`(") && !strings.Contains(command, "GH_REPO") && !strings.Contains(command, "GH_HOST")
	var links [][]PRLink
	for _, stmt := range segments {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		call, ok := stmt.Cmd.(*syntax.CallExpr)
		if !ok || len(call.Assigns) != 0 || len(call.Args) < 4 {
			continue
		}
		words := make([]string, len(call.Args))
		literal := make([]bool, len(call.Args))
		for i, word := range call.Args {
			words[i], literal[i] = shellLiteral(word.Parts)
		}
		if !literal[0] || !literal[1] || !literal[2] || !literal[3] || words[0] != "gh" || words[1] != "pr" {
			continue
		}
		switch words[2] {
		case "view", "checkout", "edit", "diff", "checks", "comment", "review", "ready", "merge", "close", "reopen":
		default:
			continue
		}
		selector := words[3]
		if strings.ContainsAny(selector, "$`") {
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
		number, err := strconv.Atoi(selector)
		if err != nil || number <= 0 {
			continue
		}
		repo := ""
		specified := false
		for i := 4; i < len(words); i++ {
			if !literal[i] {
				continue
			}
			switch {
			case words[i] == "-R" || words[i] == "--repo":
				specified = true
				if i+1 < len(words) && literal[i+1] {
					i++
					repo = words[i]
				}
			case strings.HasPrefix(words[i], "--repo="):
				specified = true
				repo = strings.TrimPrefix(words[i], "--repo=")
			case strings.HasPrefix(words[i], "-R") && len(words[i]) > 2:
				specified = true
				repo = strings.TrimPrefix(words[i][2:], "=")
			}
		}
		host := "github.com"
		if !specified && bare {
			if normalized, ok := export.NormalizeGitRemote(remote); ok {
				host, repo, _ = strings.Cut(normalized, "/")
			}
		}
		if !repoPathRE.MatchString(repo) {
			continue
		}
		if link, ok := NewPRLink(fmt.Sprintf("https://%s/%s/pull/%d", host, repo, number), repo, number); ok {
			links = append(links, []PRLink{link})
		}
	}
	return CombinePRLinks(links...), nil
}
