package why

import (
	"path"
	"regexp"
	"strings"
	"unicode"
)

// NamedRef is an explicitly named issue or existing repository document.
type NamedRef struct{ Key, Issue, Path string }

var issuePattern = regexp.MustCompile(`^(?:[a-z][a-z0-9_-]*#[a-z0-9]{4}|#(?:[0-9]+|[a-z0-9]{4}))$`)

// FindNamedRefs scans whole tokens so unsafe paths cannot yield a safe suffix.
// Documents are checked at most 64 times; neither issues nor bodies are fetched.
func FindNamedRefs(texts []string, exists func(string) (bool, error)) ([]NamedRef, error) {
	refs := []NamedRef{}
	seen := map[string]bool{}
	issues, paths, checks := 0, 0, 0
	for _, text := range texts {
		var tokens []string
		for _, chunk := range strings.Fields(text) {
			// Reject the entire path context before splitting Markdown wrappers.
			whole := strings.TrimLeft(chunk, "`'\"([{<*")
			if strings.Contains(whole, "://") || strings.HasPrefix(whole, "/") || strings.HasPrefix(whole, "../") || strings.Contains(whole, "/../") || strings.Contains(whole, "(/") || strings.Contains(whole, "[/") || strings.Contains(whole, ":/") || strings.ContainsRune(whole, '\\') {
				continue
			}
			delimiters := "`'\"()[]{}<>,;!*"
			if strings.Contains(chunk, "#") {
				delimiters += ":"
			}
			tokens = append(tokens, strings.FieldsFunc(chunk, func(r rune) bool { return unicode.IsSpace(r) || strings.ContainsRune(delimiters, r) })...)
		}
		for _, token := range tokens {
			token = strings.TrimRight(token, ".:")
			if issuePattern.MatchString(token) {
				key := "issue:" + token
				if issues < 5 && !seen[key] {
					refs = append(refs, NamedRef{Key: key, Issue: token})
					issues++
					seen[key] = true
				}
				continue
			}
			if paths >= 8 || checks >= 64 || !documentCandidate(token) {
				continue
			}
			key := "path:" + token
			if seen[key] {
				continue
			}
			seen[key] = true
			checks++
			yes, err := exists(token)
			if err != nil {
				return nil, err
			}
			if yes {
				refs = append(refs, NamedRef{Key: key, Path: token})
				paths++
			}
		}
	}
	return refs, nil
}
func documentCandidate(token string) bool {
	if strings.HasPrefix(token, "/") || strings.ContainsAny(token, ":\\#?\x00") {
		return false
	}
	switch path.Ext(token) {
	case ".md", ".mdx", ".txt", ".toml", ".yaml", ".yml", ".json":
	default:
		return false
	}
	parts := strings.Split(strings.TrimPrefix(token, "./"), "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}
