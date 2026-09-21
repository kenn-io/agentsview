package server_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/server"
	"go.kenn.io/agentsview/internal/update"
)

func stubChecker(
	info *update.UpdateInfo, err error,
) server.UpdateCheckFunc {
	return func(context.Context, string, bool, string) (*update.UpdateInfo, error) {
		return info, err
	}
}

func TestCheckUpdateUpToDate(t *testing.T) {
	t.Parallel()

	te := setupWithServerOpts(t, []server.Option{
		server.WithVersion(server.VersionInfo{
			Version:   "v1.0.0",
			Commit:    "abc123",
			BuildDate: "2026-01-01",
		}),
		server.WithUpdateChecker(stubChecker(nil, nil)),
	})

	w := te.get(t, "/api/v1/update/check")
	assertStatus(t, w, 200)

	resp := decode[updateCheckResp](t, w)
	assert.Equal(t, "v1.0.0", resp.CurrentVersion)
	assert.False(t, resp.UpdateAvailable, "expected update_available=false when up to date")
}

func TestCheckUpdateAvailable(t *testing.T) {
	t.Parallel()

	te := setupWithServerOpts(t, []server.Option{
		server.WithVersion(server.VersionInfo{
			Version:   "v0.9.0",
			Commit:    "abc123",
			BuildDate: "2026-01-01",
		}),
		server.WithUpdateChecker(stubChecker(
			&update.UpdateInfo{
				CurrentVersion: "v0.9.0",
				LatestVersion:  "v1.0.0",
			},
			nil,
		)),
	})

	w := te.get(t, "/api/v1/update/check")
	assertStatus(t, w, 200)

	resp := decode[updateCheckResp](t, w)
	assert.True(t, resp.UpdateAvailable)
	assert.Equal(t, "v1.0.0", resp.LatestVersion)
	assert.Equal(t, "v0.9.0", resp.CurrentVersion)
}

func TestCheckUpdateDevBuild(t *testing.T) {
	t.Parallel()

	te := setupWithServerOpts(t, []server.Option{
		server.WithVersion(server.VersionInfo{
			Version:   "dev",
			Commit:    "unknown",
			BuildDate: "",
		}),
		server.WithUpdateChecker(stubChecker(
			&update.UpdateInfo{
				CurrentVersion: "dev",
				LatestVersion:  "v1.0.0",
				IsDevBuild:     true,
			},
			nil,
		)),
	})

	w := te.get(t, "/api/v1/update/check")
	assertStatus(t, w, 200)

	resp := decode[updateCheckResp](t, w)
	assert.False(t, resp.UpdateAvailable, "expected update_available=false for dev build")
	assert.True(t, resp.IsDevBuild)
	assert.Equal(t, "dev", resp.CurrentVersion)
}

func TestCheckUpdateError(t *testing.T) {
	t.Parallel()

	te := setupWithServerOpts(t, []server.Option{
		server.WithVersion(server.VersionInfo{
			Version:   "v1.0.0",
			Commit:    "abc123",
			BuildDate: "2026-01-01",
		}),
		server.WithUpdateChecker(stubChecker(
			nil, errors.New("network error"),
		)),
	})

	w := te.get(t, "/api/v1/update/check")
	assertStatus(t, w, 200)

	resp := decode[updateCheckResp](t, w)
	assert.Equal(t, "v1.0.0", resp.CurrentVersion)
	assert.False(t, resp.UpdateAvailable, "expected update_available=false on error")
}

func TestCheckUpdateDisabled(t *testing.T) {
	t.Parallel()

	te := setupWithServerOpts(t, []server.Option{
		server.WithVersion(server.VersionInfo{
			Version:   "v1.0.0",
			Commit:    "abc123",
			BuildDate: "2026-01-01",
		}),
		server.WithUpdateChecker(stubChecker(
			&update.UpdateInfo{
				CurrentVersion: "v1.0.0",
				LatestVersion:  "v2.0.0",
			},
			nil,
		)),
	}, func(c *config.Config) {
		c.DisableUpdateCheck = true
	})

	w := te.get(t, "/api/v1/update/check")
	assertStatus(t, w, 200)

	resp := decode[updateCheckResp](t, w)
	assert.False(t, resp.UpdateAvailable, "expected update_available=false when disabled")
	assert.Equal(t, "v1.0.0", resp.CurrentVersion)
}

type updateCheckResp struct {
	UpdateAvailable bool   `json:"update_available"`
	CurrentVersion  string `json:"current_version"`
	LatestVersion   string `json:"latest_version"`
	IsDevBuild      bool   `json:"is_dev_build"`
}
