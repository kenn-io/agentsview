package web

import (
	"io/fs"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAssetsIncludesIndex(t *testing.T) {
	assets, err := Assets()
	require.NoError(t, err)

	_, err = fs.ReadFile(assets, "index.html")
	require.NoError(t, err)
}
