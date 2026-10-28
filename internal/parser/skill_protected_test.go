//go:build darwin

package parser

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestSkillInferenceSkipsProtectedPaths(t *testing.T) {
	for _, tc := range []struct {
		name  string
		dir   string
		link  bool
		allow bool
		want  string
	}{
		{name: "downloads root", dir: "Downloads", want: "Downloads"},
		{name: "downloaded skill", dir: "Downloads/skill-folder", want: "skill-folder"},
		{name: "symlink into downloads", dir: "Downloads/skill-folder", link: true, want: "linked-skill"},
		{name: "explicit opt in", dir: "Downloads/skill-folder", allow: true, want: "frontmatter-name"},
		{name: "ordinary skill", dir: "src/skill-folder", want: "frontmatter-name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			dir := filepath.Join(home, tc.dir)
			require.NoError(t, os.MkdirAll(dir, 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "SKILL.md"),
				[]byte("---\nname: frontmatter-name\n---\n"), 0o600))
			if tc.link {
				link := filepath.Join(home, "linked-skill")
				require.NoError(t, os.Symlink(dir, link))
				dir = link
			}
			SetAllowProtectedPathProbes(tc.allow)
			t.Cleanup(func() { SetAllowProtectedPathProbes(false) })
			path := filepath.Join(dir, "SKILL.md")
			_, _, _, _, _, calls, _ := ExtractTextContent(t.Context(), gjson.Parse(
				`[{"type":"tool_use","id":"call-1","name":"Read","input":{"file_path":`+strconv.Quote(path)+`}}]`,
			))
			require.Len(t, calls, 1)
			assert.Equal(t, tc.want, calls[0].SkillName)
		})
	}
}
