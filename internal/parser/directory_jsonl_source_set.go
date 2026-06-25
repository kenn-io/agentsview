package parser

import (
	"path/filepath"
	"strings"
)

// DirectoryJSONLSourceSet constrains JSONL sources to the common
// <root>/<project>/<session>.<ext> shape while keeping JSONLSourceSet's source
// methods available through embedding.
type DirectoryJSONLSourceSet struct {
	JSONLSourceSet
}

// newDirectoryJSONLSourceSet returns a JSONL source helper for providers whose
// transcripts live one project directory below each configured root. The
// returned helper is always recursive enough to classify watched project files,
// but it rejects root-level and deeper nested files through IncludePath.
func newDirectoryJSONLSourceSet(
	provider AgentType,
	roots []string,
	opts ...jsonlOption,
) DirectoryJSONLSourceSet {
	var options JSONLSourceSetOptions
	for _, opt := range opts {
		opt(&options)
	}
	userIncludePath := options.IncludePath
	options.Recursive = true
	options.IncludePath = func(root, path string) bool {
		if !isDirectoryJSONLPath(root, path) {
			return false
		}
		return userIncludePath == nil || userIncludePath(root, path)
	}
	if options.ProjectHint == nil {
		options.ProjectHint = func(root, path string) string {
			return directoryJSONLProjectFromPath(path)
		}
	}
	return DirectoryJSONLSourceSet{
		JSONLSourceSet: jsonlSourceSetFromOptions(provider, roots, options),
	}
}

func isDirectoryJSONLPath(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	return len(parts) == 2 &&
		parts[0] != "" && parts[0] != "." && parts[0] != ".." &&
		parts[1] != "" && parts[1] != "." && parts[1] != ".."
}

func directoryJSONLProjectFromPath(path string) string {
	return filepath.Base(filepath.Dir(path))
}
