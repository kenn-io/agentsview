package main

import (
	"crypto/rand"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/chromehost"
	"golang.org/x/sys/windows/registry"
)

func TestChromeRegisterHost(t *testing.T) {
	subkey := `Software\AgentsViewTest\ChromeNativeHost-` + rand.Text()
	t.Cleanup(func() { _ = registry.DeleteKey(registry.CURRENT_USER, subkey) })
	require.NoError(t, registerChromeHostKey(subkey, `C:\example\io.kenn.agentsview.json`))
	key, err := registry.OpenKey(registry.CURRENT_USER, subkey, registry.QUERY_VALUE)
	require.NoError(t, err)
	defer key.Close()
	path, _, err := key.GetStringValue("")
	require.NoError(t, err)
	registered, err := chromehost.ReadRegistrationKey(subkey)
	require.NoError(t, err)
	assert.Equal(t, path, registered)
	assert.Equal(t, `C:\example\io.kenn.agentsview.json`, path)
}
