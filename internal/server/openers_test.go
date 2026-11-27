package server

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLaunchOpenerSurvivesRequestCancellation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test fixture uses a POSIX executable script")
	}
	dir := t.TempDir()
	release := filepath.Join(dir, "release")
	completed := filepath.Join(dir, "completed")
	bin := filepath.Join(dir, "editor")
	require.NoError(t, exec.CommandContext(t.Context(), "mkfifo", release).Run())
	require.NoError(t, os.WriteFile(bin, []byte(`#!/bin/sh
IFS= read -r _ < "$1/release"
printf completed > "$1/completed"
`), 0o755))
	releaseFile, err := os.OpenFile(release, os.O_RDWR, 0o600)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = releaseFile.WriteString("\n")
		_ = releaseFile.Close()
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	require.NoError(t, launchOpener(ctx, Opener{Kind: "editor", Bin: bin}, dir))
	cancel()
	_, err = releaseFile.WriteString("release\n")
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		_, err := os.Stat(completed)
		return err == nil
	}, 5*time.Second, 5*time.Millisecond)
}

func TestLaunchOpenerRejectsCancelledRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	assert.ErrorIs(t, launchOpener(ctx, Opener{Kind: "editor", Bin: "unused"}, t.TempDir()), context.Canceled)
}

func TestFindOpenersCodeBuddyIDE(t *testing.T) {
	cn := Opener{ID: "codebuddy-ide", Name: "CodeBuddy CN", Kind: "action", Bin: "/opt/bin/buddycn"}
	insiders := Opener{
		ID: "codebuddy-ide-insiders", Name: "CodeBuddy CN Insiders", Kind: "action",
		Bin: "/opt/bin/buddycn-insiders",
	}
	onPath := map[string]string{
		"buddycn":          "/opt/bin/buddycn",
		"buddycn-insiders": "/opt/bin/buddycn-insiders",
	}
	for _, tc := range []struct {
		name        string
		goos        string
		path        map[string]string
		apps        []string
		handlers    []string
		want        []Opener
		wantQueried []string
	}{
		{
			name:        "linux without scheme handlers",
			goos:        "linux",
			path:        onPath,
			wantQueried: []string{"codebuddycn", "codebuddycn-insiders"},
		},
		{
			name:        "linux with one scheme handler",
			goos:        "linux",
			path:        onPath,
			handlers:    []string{"codebuddycn"},
			want:        []Opener{cn},
			wantQueried: []string{"codebuddycn", "codebuddycn-insiders"},
		},
		{
			name:        "linux with both scheme handlers",
			goos:        "linux",
			path:        onPath,
			handlers:    []string{"codebuddycn", "codebuddycn-insiders"},
			want:        []Opener{cn, insiders},
			wantQueried: []string{"codebuddycn", "codebuddycn-insiders"},
		},
		{
			name:     "linux handler without launcher",
			goos:     "linux",
			handlers: []string{"codebuddycn", "codebuddycn-insiders"},
		},
		{
			name: "windows launchers",
			goos: "windows",
			path: onPath,
			want: []Opener{cn, insiders},
		},
		{
			name: "darwin app bundle or launcher",
			goos: "darwin",
			path: onPath,
			apps: []string{"/Applications/CodeBuddy CN.app"},
			want: []Opener{
				{ID: "codebuddy-ide", Name: "CodeBuddy CN", Kind: "action", Bin: "/Applications/CodeBuddy CN.app"},
				insiders,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var queried []string
			got := findOpeners(t.Context(), tc.goos, openerProbe{
				lookPath: func(file string) (string, error) {
					if path, ok := tc.path[file]; ok {
						return path, nil
					}
					return "", exec.ErrNotFound
				},
				stat: func(name string) (os.FileInfo, error) {
					if slices.Contains(tc.apps, name) {
						return nil, nil
					}
					return nil, os.ErrNotExist
				},
				schemeHandler: func(_ context.Context, scheme string) bool {
					queried = append(queried, scheme)
					return slices.Contains(tc.handlers, scheme)
				},
			})
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.wantQueried, queried)
		})
	}
}

func TestXDGSchemeHandlerRegistered(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test fixture uses a POSIX executable script")
	}
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "xdg-mime"), []byte(`#!/bin/sh
case "$*" in
"query default x-scheme-handler/codebuddycn") echo codebuddycn-url-handler.desktop ;;
"query default x-scheme-handler/codebuddycn-insiders") ;;
*) echo unexpected.desktop; exit 2 ;;
esac
`), 0o755))
	t.Setenv("PATH", dir)

	assert.True(t, xdgSchemeHandlerRegistered(t.Context(), "codebuddycn"))
	assert.False(t, xdgSchemeHandlerRegistered(t.Context(), "codebuddycn-insiders"))
	assert.False(t, xdgSchemeHandlerRegistered(t.Context(), "other"))

	t.Setenv("PATH", t.TempDir())
	assert.False(t, xdgSchemeHandlerRegistered(t.Context(), "codebuddycn"))
}
