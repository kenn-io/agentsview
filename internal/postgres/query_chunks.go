package postgres

import "strings"

// maxPGVars is the maximum bind variables per IN clause.
const maxPGVars = 500

// pgQueryChunked executes a callback for each chunk of IDs,
// splitting at maxPGVars to avoid excessive bind variables.
func pgQueryChunked(ids []string, fn func(chunk []string) error) error {
	for i := 0; i < len(ids); i += maxPGVars {
		end := min(i+maxPGVars, len(ids))
		if err := fn(ids[i:end]); err != nil {
			return err
		}
	}
	return nil
}

// pgInPlaceholders returns a parenthesized list of numbered parameters.
func pgInPlaceholders(ids []string, pb *paramBuilder) string {
	placeholders := make([]string, len(ids))
	for i, id := range ids {
		placeholders[i] = pb.add(id)
	}
	return "(" + strings.Join(placeholders, ",") + ")"
}
