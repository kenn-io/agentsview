package server

import (
	"context"

	"go.kenn.io/agentsview/internal/update"
)

// UpdateCheckFunc is the signature for functions that check for
// available updates. The default is update.CheckForUpdate.
type UpdateCheckFunc func(
	ctx context.Context,
	currentVersion string,
	forceCheck bool,
	cacheDir string,
) (*update.UpdateInfo, error)

type updateCheckResponse struct {
	UpdateAvailable bool   `json:"update_available"`
	CurrentVersion  string `json:"current_version"`
	LatestVersion   string `json:"latest_version,omitempty"`
	IsDevBuild      bool   `json:"is_dev_build,omitempty"`
}
