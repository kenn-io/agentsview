package config

import (
	"fmt"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
	"os"
	"path/filepath"
	"testing"
)

func TestRemoteConfigPrecedence(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dir)
	hegel.Test(t, func(ht *hegel.T) {
		env := hegel.Draw(ht, hegel.Booleans())
		urlEnv := hegel.Draw(ht, hegel.Booleans())
		kind := hegel.Draw(ht, hegel.Integers(0, 5))
		absolute := kind == 1 || kind == 4 || kind == 5
		name := fmt.Sprintf("token-%d", hegel.Draw(ht, hegel.Integers(0, 100000)))
		path := name
		switch kind {
		case 1:
			path = filepath.Join(dir, name)
		case 2:
			path = filepath.Join("nested", name)
		case 3:
			path = filepath.Join("..", name)
		case 4:
			path = "~/" + name
		case 5:
			path = ""
		}
		require.NoError(ht, os.WriteFile(filepath.Join(dir, "config.toml"), []byte(fmt.Sprintf("[remote]\nurl='https://file.example'\ntoken_file='%s'\nspoke_host='workstation'\n", filepath.ToSlash(path))), 0600))
		value := ""
		if env {
			value = path
		}
		t.Setenv("AGENTSVIEW_REMOTE_TOKEN_FILE", value)
		t.Setenv("AGENTSVIEW_REMOTE_URL", "")
		if urlEnv {
			t.Setenv("AGENTSVIEW_REMOTE_URL", "https://env.example")
		}
		remote, err := LoadRemote()
		require.NoError(ht, err)
		wantURL, base := "https://file.example", dir
		if urlEnv {
			wantURL = "https://env.example"
		}
		if env {
			base, err = os.Getwd()
			require.NoError(ht, err)
		}
		want := path
		if kind == 4 {
			home, e := os.UserHomeDir()
			require.NoError(ht, e)
			want = filepath.Join(home, name)
		}
		if !absolute {
			want = filepath.Join(base, path)
		}
		require.Equal(ht, wantURL, remote.URL)
		require.Equal(ht, want, remote.TokenFile)
		require.Equal(ht, "workstation", remote.SpokeHost)
	}, hegel.WithTestCases(1000))
}
