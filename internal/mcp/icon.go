package mcp

import (
	_ "embed"
	"encoding/base64"
)

// serverIconPNG is copied unchanged from desktop/src-tauri/icons/128x128@2x.png.
//
//go:embed assets/icon.png
var serverIconPNG []byte

// A data URI works for stdio and HTTP without a deployment-specific public URL.
var serverIconSource = "data:image/png;base64," + base64.StdEncoding.EncodeToString(serverIconPNG)
