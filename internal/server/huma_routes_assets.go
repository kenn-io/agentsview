package server

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

func (s *Server) registerAssetRoutes() {
	group := newRouteGroup(s.api, "/api/v1/assets", "Assets")

	s.raw(group, http.MethodGet, "/{filename}", "Get imported asset", s.humaGetAsset)
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
