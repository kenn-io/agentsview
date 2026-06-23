package agentsview_test

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

type githubWorkflow struct {
	Jobs map[string]githubWorkflowJob `yaml:"jobs"`
}

type githubWorkflowJob struct {
	Steps []githubWorkflowStep `yaml:"steps"`
}

type githubWorkflowStep struct {
	Name string `yaml:"name"`
	Run  string `yaml:"run"`
	Uses string `yaml:"uses"`
}

func TestWindowsDesktopUpdateTestsRetryCargoNetworkFailures(t *testing.T) {
	contents, err := os.ReadFile(".github/workflows/ci.yml")
	require.NoError(t, err)

	var workflow githubWorkflow
	require.NoError(t, yaml.Unmarshal(contents, &workflow))

	job, ok := workflow.Jobs["desktop-windows-unit"]
	require.True(t, ok, "desktop-windows-unit job must exist")

	fetchIndex, fetchStep := findWorkflowStep(t, job, "Fetch Windows desktop Rust dependencies")
	testIndex, testStep := findWorkflowStep(t, job, "Run Windows desktop update tests")
	require.Less(t, fetchIndex, testIndex, "dependencies must be fetched before cargo test")

	assert.Contains(t, fetchStep.Run, "cargo fetch --locked --manifest-path desktop/src-tauri/Cargo.toml")
	assert.Contains(t, fetchStep.Run, "$attempt")
	assert.Contains(t, fetchStep.Run, "$LASTEXITCODE")
	assert.Contains(t, fetchStep.Run, "Start-Sleep")
	assert.Contains(t, testStep.Run, "cargo test --locked --manifest-path desktop/src-tauri/Cargo.toml --lib install_downloaded_update")
}

func TestCIDocsJobRunsFullDocsCheck(t *testing.T) {
	contents, err := os.ReadFile(".github/workflows/ci.yml")
	require.NoError(t, err)

	var workflow githubWorkflow
	require.NoError(t, yaml.Unmarshal(contents, &workflow))

	job, ok := workflow.Jobs["docs"]
	require.True(t, ok, "docs job must exist")

	uvIndex, uvStep := findWorkflowStep(t, job, "Set up uv")
	checkIndex, checkStep := findWorkflowStep(t, job, "Run docs check")
	require.Less(t, uvIndex, checkIndex, "uv must be installed before docs check")

	assert.Contains(t, uvStep.Uses, "astral-sh/setup-uv")
	assert.Equal(t, "make docs-check", checkStep.Run)
}

func TestReleaseWorkflowRestoresPricingSnapshotBeforeGoBuild(t *testing.T) {
	contents, err := os.ReadFile(".github/workflows/release.yml")
	require.NoError(t, err)

	var workflow githubWorkflow
	require.NoError(t, yaml.Unmarshal(contents, &workflow))

	for _, jobName := range []string{"build-linux", "build"} {
		job, ok := workflow.Jobs[jobName]
		require.True(t, ok, "%s job must exist", jobName)

		restoreIndex, restoreStep := findWorkflowStep(t, job, "Restore pricing snapshot")
		buildIndex, buildStep := findWorkflowStep(t, job, "Build")
		require.Less(t, restoreIndex, buildIndex,
			"%s must restore pricing snapshot before building", jobName)

		if jobName == "build-linux" {
			trustIndex, trustStep := findWorkflowStep(t, job, "Trust git checkout")
			require.Less(t, trustIndex, restoreIndex,
				"%s must trust the checkout before restoring the snapshot", jobName)
			assert.Contains(t, trustStep.Run, `safe.directory "$GITHUB_WORKSPACE"`)
			assert.Contains(t, trustStep.Run, "git status")
		}

		assertSnapshotRestoreStep(t, restoreStep)
		assert.Contains(t, buildStep.Run, "go build")
	}
}

func TestCIWorkflowRestoresPricingSnapshotBeforeGoTests(t *testing.T) {
	contents, err := os.ReadFile(".github/workflows/ci.yml")
	require.NoError(t, err)

	var workflow githubWorkflow
	require.NoError(t, yaml.Unmarshal(contents, &workflow))

	cases := []struct {
		jobName   string
		buildStep string
	}{
		{"test", "Run Go tests"},
		{"coverage", "Test with coverage"},
		{"integration", "Run PostgreSQL integration tests"},
		{"e2e", "Pre-build Go binaries"},
	}

	for _, tc := range cases {
		job, ok := workflow.Jobs[tc.jobName]
		require.True(t, ok, "%s job must exist", tc.jobName)

		restoreIndex, restoreStep := findWorkflowStep(t, job, "Restore pricing snapshot")
		buildIndex, _ := findWorkflowStep(t, job, tc.buildStep)
		require.Less(t, restoreIndex, buildIndex,
			"%s must restore pricing snapshot before %s", tc.jobName, tc.buildStep)

		assertSnapshotRestoreStep(t, restoreStep)
	}
}

func TestMSYS2UpdateWorkflowRestoresPricingSnapshotBeforeGoTests(t *testing.T) {
	contents, err := os.ReadFile(".github/workflows/msys2-update-check.yml")
	require.NoError(t, err)

	var workflow githubWorkflow
	require.NoError(t, yaml.Unmarshal(contents, &workflow))

	job, ok := workflow.Jobs["windows-update-check"]
	require.True(t, ok, "windows-update-check job must exist")

	restoreIndex, restoreStep := findWorkflowStep(t, job, "Restore pricing snapshot")
	testIndex, _ := findWorkflowStep(t, job, "Run Go tests")
	require.Less(t, restoreIndex, testIndex,
		"msys2 update check must restore pricing snapshot before tests")

	assertSnapshotRestoreStep(t, restoreStep)
}

func TestDockerWorkflowRestoresPricingSnapshotBeforeImageBuild(t *testing.T) {
	contents, err := os.ReadFile(".github/workflows/docker.yml")
	require.NoError(t, err)

	var workflow githubWorkflow
	require.NoError(t, yaml.Unmarshal(contents, &workflow))

	job, ok := workflow.Jobs["build-and-push"]
	require.True(t, ok, "build-and-push job must exist")

	restoreIndex, restoreStep := findWorkflowStep(t, job, "Restore pricing snapshot")
	buildIndex, _ := findWorkflowStep(t, job, "Build and push Docker image")
	require.Less(t, restoreIndex, buildIndex,
		"docker workflow must restore pricing snapshot before image build")

	assertSnapshotRestoreStep(t, restoreStep)
}

func assertSnapshotRestoreStep(t *testing.T, step githubWorkflowStep) {
	t.Helper()

	assert.Equal(t, "go run ./internal/pricing/cmd/litellm-snapshot -restore", step.Run)
}

func TestDesktopLinuxWorkflowsRepairAppImageDirIcon(t *testing.T) {
	cases := []struct {
		name      string
		path      string
		job       string
		depsStep  string
		buildStep string
	}{
		{
			name:      "artifacts",
			path:      ".github/workflows/desktop-artifacts.yml",
			job:       "build",
			depsStep:  "Install Linux system dependencies",
			buildStep: "Build desktop bundle",
		},
		{
			name:      "release",
			path:      ".github/workflows/desktop-release.yml",
			job:       "build-linux",
			depsStep:  "Install Linux system dependencies",
			buildStep: "Build AppImage and updater bundle",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			contents, err := os.ReadFile(tc.path)
			require.NoError(t, err)

			var workflow githubWorkflow
			require.NoError(t, yaml.Unmarshal(contents, &workflow))

			job, ok := workflow.Jobs[tc.job]
			require.True(t, ok, "%s job must exist", tc.job)

			_, depsStep := findWorkflowStep(t, job, tc.depsStep)
			_, buildStep := findWorkflowStep(t, job, tc.buildStep)

			assert.Contains(t, depsStep.Run, "squashfs-tools")
			assert.Contains(t, buildStep.Run, "repair-appimage-diricon.sh")
			assert.Contains(t, buildStep.Run, "*.AppImage")
		})
	}
}

func findWorkflowStep(
	t *testing.T,
	job githubWorkflowJob,
	name string,
) (int, githubWorkflowStep) {
	t.Helper()

	for i, step := range job.Steps {
		if step.Name == name {
			return i, step
		}
	}

	require.Failf(t, "missing workflow step", "step %q was not found", name)
	return -1, githubWorkflowStep{}
}
