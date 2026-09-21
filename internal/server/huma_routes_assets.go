package server

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/danielgtaylor/huma/v2"
)

func (s *Server) registerAssetRoutes() {
	group := huma.NewGroup(s.api, "/api/v1")
	configureRouteGroup(group, "Assets")

	s.raw(group, http.MethodGet, "/assets/{filename}", "Get imported asset", "application/octet-stream", s.humaGetAsset)
}

type assetInput struct {
	Filename string `path:"filename" required:"true" doc:"Asset filename"`
}

func (s *Server) humaGetAsset(
	_ context.Context,
	in *assetInput,
) (*bytesOutput, error) {
	filename := in.Filename
	if filename == "" {
		return nil, apiError(http.StatusBadRequest, "missing filename")
	}
	if strings.Contains(filename, "..") ||
		strings.Contains(filename, "/") ||
		strings.Contains(filename, "\\") {
		return nil, apiError(http.StatusBadRequest, "invalid filename")
	}
	ext := strings.ToLower(filepath.Ext(filename))
	contentType, ok := safeImageTypes[ext]
	if !ok {
		return nil, apiError(http.StatusForbidden, "unsupported asset type")
	}
	filePath := filepath.Join(s.cfg.DataDir, "assets", filename)
	var data []byte
	var err error
	if s.assetCache == nil {
		data, err = os.ReadFile(filePath)
	} else {
		data, err = s.assetCache.read(filename, filePath, contentType)
	}
	if err != nil {
		return nil, apiError(http.StatusNotFound, "asset not found")
	}
	return &bytesOutput{
		ContentType:  contentType,
		NoSniff:      "nosniff",
		CacheControl: "public, max-age=31536000, immutable",
		Body:         data,
	}, nil
}
