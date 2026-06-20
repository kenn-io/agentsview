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
