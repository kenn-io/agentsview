package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestResolveRepoDotSuffix(t *testing.T) {
	for _, tc := range []struct {
		name, prefix, cwd, project string
	}{
		{"branch", "/code", "/code/sample-app.abc1", "sample_app"},
		{"environment", "/code", "/code/sample-app.envrc", "sample_app"},
		{"nested", "/code", "/code/sample-app.abc1/packages/foo", "sample_app"},
		{"multi-dot", "/code", "/code/sample-app.v1.2", "sample_app"},
		{"plain", "/code", "/code/sample-app", ""},
		{"plain with dotted child", "/code", "/code/sample-app/feature.branch", ""},
		{"hidden", "/code", "/code/.hidden/feature.branch", ""},
		{"empty suffix", "/code", "/code/sample-app.", ""},
		{"prefix itself", "/code", "/code", ""},
		{"prefix boundary", "/code", "/code-other/foo.bar", ""},
		{"clean path", "/code/./", "/code/other/../sample-app.dev/", "sample_app"},
		{"windows", `C:\code\`, `C:\code\sample-app.dev\packages`, "sample_app"},
		{"drive root", `C:\`, "C:/sample-app.dev", "sample_app"},
		{"drive cwd stays on volume", "C:/code", "C:/../code/sample-app.dev", "sample_app"},
		{"drive prefix stays on volume", "C:/../code", "C:/code/sample-app.dev", "sample_app"},
		{"UNC share root", `\\host\share`, `\\host\share\sample-app.dev`, "sample_app"},
		{"UNC cwd stays on share", "//host/share/other.dev", "//host/share/../other.dev/sample-app.dev", "sample_app"},
		{"UNC prefix stays on share", "//host/share/../other.dev", "//host/share/other.dev/sample-app.dev", "sample_app"},
		{"UNC cwd cannot match sibling share", "//host/other.dev", "//host/share/../other.dev/sample-app.dev", ""},
		{"POSIX root", "/", "/sample-app.dev", "sample_app"},
		{"POSIX colon directory", "/code", "/:/../code/sample-app.dev", "sample_app"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			project, ok := ResolveWorktreeProjectFromMappings([]WorktreeProjectMapping{
				{PathPrefix: tc.prefix, Layout: "repo_dot_suffix", Enabled: true},
			}, tc.cwd, "original")
			if tc.project == "" {
				assert.False(t, ok)
				assert.Equal(t, "original", project)
			} else {
				assert.True(t, ok)
				assert.Equal(t, tc.project, project)
			}
		})
	}
}
