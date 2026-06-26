package parser

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestResolveFilePathFromJSON(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"file_path key", `{"file_path":"/a/b.go"}`, "/a/b.go"},
		{"path key", `{"path":"src/x.go"}`, "src/x.go"},
		{"filePath key", `{"filePath":"app.ts"}`, "app.ts"},
		{"file key", `{"file":"new.go"}`, "new.go"},
		{"precedence file_path wins", `{"path":"p","file_path":"fp"}`, "fp"},
		{"precedence path beats filePath", `{"path":"b","filePath":"c"}`, "b"},
		{"precedence filePath beats file", `{"filePath":"c","file":"d"}`, "c"},
		{"invalid json returns empty", "this is a raw diff, not json", ""},
		{"valid json no path key", `{"command":"ls"}`, ""},
		{"empty string", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ResolveFilePathFromJSON(tt.input))
		})
	}
}
