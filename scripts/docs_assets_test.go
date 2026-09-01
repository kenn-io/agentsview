package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHydrateAssetsForceFetchesRemoteAssetBranches(t *testing.T) {
	tempDir := t.TempDir()
	remoteRepo := filepath.Join(tempDir, "remote")
	localRepo := filepath.Join(tempDir, "local")
	require.NoError(t, os.MkdirAll(localRepo, 0o755))

	git(t, tempDir, "init", "--bare", remoteRepo)
	oldStaticDir := filepath.Join(tempDir, "old-static")
	writeStaticAssets(t, oldStaticDir, "old static")
	oldStaticCommit := commitBareAssetTree(
		t, remoteRepo, oldStaticDir, "old static assets",
	)
	updateBareBranch(t, remoteRepo, "docs-assets", oldStaticCommit)

	git(t, localRepo, "init")
	git(t, localRepo, "remote", "add", "origin", remoteRepo)
	git(t, localRepo, "fetch", "origin", "docs-assets:refs/remotes/origin/docs-assets")

	newStaticDir := filepath.Join(tempDir, "new-static")
	writeStaticAssets(t, newStaticDir, "new static")
	newStaticCommit := commitBareAssetTree(
		t, remoteRepo, newStaticDir, "new static assets",
	)
	updateBareBranch(t, remoteRepo, "docs-assets", newStaticCommit)

	generatedDir := filepath.Join(tempDir, "generated")
	writeGeneratedAssets(t, generatedDir, "generated")
	generatedCommit := commitBareAssetTree(
		t, remoteRepo, generatedDir, "generated assets",
	)
	updateBareBranch(t, remoteRepo, "docs-generated-assets", generatedCommit)

	docsAssetsDir := filepath.Join(localRepo, "docs", "assets")
	require.NoError(t, os.MkdirAll(docsAssetsDir, 0o755))
	writeStaticAssets(t, filepath.Join(docsAssetsDir, "static"), "stale local static")
	writeAssetFiles(
		t, filepath.Join(docsAssetsDir, "generated"),
		[]string{"screenshots/dashboard.png"}, "stale local generated",
	)

	script, err := os.ReadFile(filepath.Join("..", "docs", "assets", "hydrate-assets.sh"))
	require.NoError(t, err)
	scriptPath := filepath.Join(docsAssetsDir, "hydrate-assets.sh")
	require.NoError(t, os.WriteFile(scriptPath, script, 0o755))

	cmd := exec.Command("bash", scriptPath)
	cmd.Dir = localRepo
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))

	logo, err := os.ReadFile(filepath.Join(localRepo, "docs", "assets", "static", "og-image.png"))
	require.NoError(t, err)
	assert.Equal(t, "new static", strings.TrimRight(string(logo), "\r\n"))

	screenshot, err := os.ReadFile(filepath.Join(localRepo, "docs", "assets", "generated", "screenshots", "dashboard.png"))
	require.NoError(t, err)
	assert.Equal(t, "generated", strings.TrimRight(string(screenshot), "\r\n"))

	semanticSetup, err := os.ReadFile(filepath.Join(
		localRepo, "docs", "assets", "generated", "screenshots",
		"semantic-search-setup.png",
	))
	require.NoError(t, err)
	assert.Equal(t, "generated", strings.TrimRight(
		string(semanticSetup), "\r\n",
	))
}

func TestAssetPublishersRejectUnexpectedFiles(t *testing.T) {
	cases := []struct {
		name      string
		scriptRel string
		write     func(*testing.T, string, string)
	}{
		{
			name:      "static",
			scriptRel: filepath.Join("docs", "assets", "update-static-assets-branch.sh"),
			write:     writeStaticAssets,
		},
		{
			name:      "generated",
			scriptRel: filepath.Join("docs", "screenshots", "update-generated-assets-branch.sh"),
			write:     writeGeneratedAssets,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {

			tempDir := t.TempDir()
			repo := filepath.Join(tempDir, "repo")
			sourceDir := filepath.Join(tempDir, "source")
			require.NoError(t, os.MkdirAll(repo, 0o755))
			git(t, repo, "init")
			tc.write(t, sourceDir, "asset")
			require.NoError(t, os.WriteFile(filepath.Join(sourceDir, ".env.local"), []byte("TOKEN=secret\n"), 0o600))

			scriptPath := installScript(t, repo, tc.scriptRel)
			cmd := exec.Command("bash", scriptPath, "--source", sourceDir)
			cmd.Dir = repo
			output, err := cmd.CombinedOutput()

			require.Error(t, err, string(output))
			assert.Contains(t, string(output), "unexpected")
			assert.Contains(t, string(output), ".env.local")
		})
	}
}

func TestGeneratedAssetPublisherAcceptsSemanticSetupScreenshot(t *testing.T) {
	tempDir := t.TempDir()
	repo := filepath.Join(tempDir, "repo")
	sourceDir := filepath.Join(tempDir, "source")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	git(t, repo, "init")
	writeGeneratedAssets(t, sourceDir, "asset")

	scriptPath := installScript(
		t, repo,
		filepath.Join("docs", "screenshots", "update-generated-assets-branch.sh"),
	)
	cmd := exec.Command("bash", scriptPath, "--source", sourceDir)
	cmd.Dir = repo
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))

	show := exec.Command(
		"git", "show",
		"docs-generated-assets:screenshots/semantic-search-setup.png",
	)
	show.Dir = repo
	published, err := show.Output()
	require.NoError(t, err)
	assert.Equal(t, "asset", strings.TrimRight(string(published), "\r\n"))
}

func TestCheckDocsRejectsCorruptedMarkdownSyntax(t *testing.T) {
	tempDir := t.TempDir()
	repo := filepath.Join(tempDir, "repo")
	require.NoError(t, os.MkdirAll(repo, 0o755))

	checkScript := installScript(t, repo, filepath.Join("scripts", "check-docs.sh"))
	installScript(t, repo, filepath.Join("docs", "scripts", "check_markdown_sources.py"))
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "docs", "assets"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(repo, "docs", "assets", "hydrate-assets.sh"),
		[]byte("#!/usr/bin/env bash\nset -euo pipefail\n"),
		0o755,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(repo, "docs", "activity.md"),
		[]byte(strings.Join([]string{
			"______________________________________________________________________",
			"",
			"## title: Activity description: Activity, concurrency, and session-time reporting in AgentsView",
			"",
			"!!! warning \"Experimental\" This warning was collapsed by a formatter.",
			"",
		}, "\n")),
		0o644,
	))

	cmd := exec.Command("bash", checkScript)
	cmd.Dir = repo
	pythonPath := requireRunnablePython3(t)
	cmd.Env = append(envWithout("PATH", "PYTHON"), "PYTHON="+pythonPath, "PATH=/usr/bin:/bin")
	output, err := cmd.CombinedOutput()

	require.Error(t, err, string(output))
	assert.Contains(t, string(output), "docs markdown")
	assert.Contains(t, string(output), "activity.md")
}

func TestCheckDocsRequiresRipgrepForMediaReferenceChecks(t *testing.T) {
	tempDir := t.TempDir()
	repo := filepath.Join(tempDir, "repo")
	require.NoError(t, os.MkdirAll(repo, 0o755))

	checkScript := installScript(t, repo, filepath.Join("scripts", "check-docs.sh"))
	installScript(t, repo, filepath.Join("docs", "scripts", "check_markdown_sources.py"))
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "docs", "assets"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(repo, "docs", "assets", "hydrate-assets.sh"),
		[]byte("#!/usr/bin/env bash\nset -euo pipefail\n"),
		0o755,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(repo, "docs", "activity.md"),
		[]byte(strings.Join([]string{
			"---",
			"title: Activity",
			"description: Activity docs",
			"---",
			"",
			"Valid docs page.",
			"",
		}, "\n")),
		0o644,
	))

	bashPath, err := exec.LookPath("bash")
	require.NoError(t, err)
	cmd := exec.Command(bashPath, checkScript)
	cmd.Dir = repo
	pythonPath := requireRunnablePython3(t)
	emptyBin := filepath.Join(tempDir, "empty-bin")
	require.NoError(t, os.MkdirAll(emptyBin, 0o755))
	cmd.Env = append(envWithout("PATH", "PYTHON"), "PYTHON="+pythonPath, "PATH="+emptyBin)
	output, err := cmd.CombinedOutput()

	require.Error(t, err, string(output))
	assert.Contains(t, string(output), "rg not found")
}

func TestBuiltSiteCheckRequiresMarkdownCompanions(t *testing.T) {
	tempDir := t.TempDir()
	repo := filepath.Join(tempDir, "repo")
	require.NoError(t, os.MkdirAll(repo, 0o755))

	checkScript := installScript(t, repo, filepath.Join("docs", "scripts", "check_built_site.py"))
	writeMinimalBuiltDocsSite(t, filepath.Join(repo, "docs", "site"))

	pythonPath := requireRunnablePython3(t)
	cmd := exec.Command(pythonPath, checkScript)
	cmd.Dir = filepath.Join(repo, "docs")
	output, err := cmd.CombinedOutput()

	require.Error(t, err, string(output))
	assert.Contains(t, string(output), "missing route markdown /")
}

func TestBuiltSiteCheckRejectsSvgUsePlainHref(t *testing.T) {
	tests := []struct {
		name     string
		useTag   string
		wantPass bool
	}{
		{
			name:     "plain href breaks instant navigation",
			useTag:   `<svg viewBox="0 0 24 24"><use href="#icon"/></svg>`,
			wantPass: false,
		},
		{
			name:     "xlink href is allowed",
			useTag:   `<svg viewBox="0 0 24 24"><use xlink:href="#icon"/></svg>`,
			wantPass: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tempDir := t.TempDir()
			repo := filepath.Join(tempDir, "repo")
			require.NoError(t, os.MkdirAll(repo, 0o755))

			checkScript := installScript(t, repo, filepath.Join("docs", "scripts", "check_built_site.py"))
			siteDir := filepath.Join(repo, "docs", "site")
			writeMinimalBuiltDocsSite(t, siteDir)
			writeBuiltSiteMarkdownCompanions(t, siteDir)

			indexPath := filepath.Join(siteDir, "index.html")
			page, err := os.ReadFile(indexPath)
			require.NoError(t, err)
			patched := strings.Replace(string(page), "</body>", tt.useTag+"</body>", 1)
			require.NoError(t, os.WriteFile(indexPath, []byte(patched), 0o644))

			pythonPath := requireRunnablePython3(t)
			cmd := exec.Command(pythonPath, checkScript)
			cmd.Dir = filepath.Join(repo, "docs")
			output, err := cmd.CombinedOutput()

			if tt.wantPass {
				require.NoError(t, err, string(output))
				assert.Contains(t, string(output), "built site checks passed")
			} else {
				require.Error(t, err, string(output))
				assert.Contains(t, string(output), "Use xlink:href instead")
			}
		})
	}
}

func TestZensicalDocsBuildExcludesScreenshotToolchain(t *testing.T) {
	tempDir := t.TempDir()
	repo := filepath.Join(tempDir, "repo")
	docsDir := filepath.Join(repo, "docs")
	require.NoError(t, os.MkdirAll(docsDir, 0o755))

	scriptPath := installScript(t, repo,
		filepath.Join("docs", "zensical-docs.sh"))
	require.NoError(t, os.WriteFile(
		filepath.Join(docsDir, "zensical.toml"),
		[]byte("[project]\ndocs_dir = \"docs\"\nsite_dir = \"site\"\n"),
		0o644,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(docsDir, "index.md"),
		[]byte("---\ntitle: Home\ndescription: Home page\n---\n"),
		0o644,
	))

	toolchainFile := filepath.Join(
		docsDir, "screenshots", "node_modules", "playwright-core",
		"trace-viewer.html",
	)
	require.NoError(t, os.MkdirAll(filepath.Dir(toolchainFile), 0o755))
	require.NoError(t, os.WriteFile(
		toolchainFile, []byte("private screenshot toolchain\n"), 0o644,
	))
	testResultFile := filepath.Join(
		docsDir, "screenshots", "test-results", "trace.zip",
	)
	require.NoError(t, os.MkdirAll(filepath.Dir(testResultFile), 0o755))
	require.NoError(t, os.WriteFile(
		testResultFile, []byte("private test trace\n"), 0o644,
	))
	publicScreenshot := filepath.Join(
		docsDir, "assets", "generated", "screenshots", "dashboard.png",
	)
	require.NoError(t, os.MkdirAll(filepath.Dir(publicScreenshot), 0o755))
	require.NoError(t, os.WriteFile(
		publicScreenshot, []byte("public screenshot\n"), 0o644,
	))
	agentGuide := filepath.Join(docsDir, "agents", "testing.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(agentGuide), 0o755))
	require.NoError(t, os.WriteFile(
		agentGuide, []byte("contributor-only instructions\n"), 0o644,
	))
	writeWebsiteTierFixture(t, docsDir)

	fakeZensical := filepath.Join(docsDir, ".venv", "bin", "zensical")
	require.NoError(t, os.MkdirAll(filepath.Dir(fakeZensical), 0o755))
	require.NoError(t, os.WriteFile(fakeZensical, []byte(`#!/usr/bin/env bash
set -euo pipefail
public_docs="$(find . -maxdepth 1 -type d -name 'zensical-public-docs.*' -print -quit)"
mkdir -p site/docs
cp -R "$public_docs"/. site/docs/
printf '<urlset></urlset>\n' > site/docs/sitemap.xml
`), 0o755))

	cmd := exec.Command("bash", scriptPath, "build")
	cmd.Dir = docsDir
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
	assert.FileExists(t, filepath.Join(docsDir, "site", "docs", "index.md"))
	assert.FileExists(t, filepath.Join(
		docsDir, "site", "docs", "assets", "generated", "screenshots",
		"dashboard.png",
	))
	assert.FileExists(t, filepath.Join(docsDir, "site", "index.html"))
	assert.FileExists(t, filepath.Join(docsDir, "site", "guide", "index.html"))
	assert.FileExists(t, filepath.Join(docsDir, "site", "llms.txt"))
	assert.FileExists(t, filepath.Join(docsDir, "site", "sitemap.xml"))
	assert.FileExists(t, filepath.Join(docsDir, "site", "docs", "sitemap.xml"))
	assert.NoDirExists(t, filepath.Join(docsDir, "site", "docs", "website"))
	assert.NoDirExists(t, filepath.Join(docsDir, "site", "docs", "agents"))
	assert.NoFileExists(t, filepath.Join(docsDir, "site", "docs", "llms.txt"))
	assert.NoFileExists(t, filepath.Join(
		docsDir, "site", "docs", "screenshots", "node_modules",
		"playwright-core", "trace-viewer.html",
	))
	assert.NoFileExists(t, filepath.Join(
		docsDir, "site", "docs", "screenshots", "test-results", "trace.zip",
	))
}

func writeWebsiteTierFixture(t *testing.T, docsDir string) {
	t.Helper()
	websiteDir := filepath.Join(docsDir, "website")
	files := map[string]string{
		"index.html":                         "<!doctype html>\n",
		"index.md":                           "# Home\n",
		filepath.Join("guide", "index.html"): "<!doctype html>\n",
		"guide.md":                           "# Guide\n",
		"404.html":                           "<!doctype html>\n",
		"favicon.svg":                        "<svg xmlns=\"http://www.w3.org/2000/svg\"/>\n",
		filepath.Join("styles", "site.css"):  "body {}\n",
		filepath.Join("scripts", "site.js"):  "// site\n",
		filepath.Join("fonts", "Inter-Regular.woff2"): "font\n",
	}
	for name, content := range files {
		path := filepath.Join(websiteDir, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	}
	require.NoError(t, os.WriteFile(
		filepath.Join(docsDir, "llms.txt"), []byte("# AgentsView\n"), 0o644,
	))
}

func installScript(t *testing.T, repo, scriptRel string) string {
	t.Helper()
	script, err := os.ReadFile(filepath.Join("..", scriptRel))
	require.NoError(t, err)
	scriptPath := filepath.Join(repo, scriptRel)
	require.NoError(t, os.MkdirAll(filepath.Dir(scriptPath), 0o755))
	require.NoError(t, os.WriteFile(scriptPath, script, 0o755))
	return scriptPath
}

func requireRunnablePython3(t *testing.T) string {
	t.Helper()
	pythonPath, err := exec.LookPath("python3")
	if err != nil {
		t.Skipf("python3 not available on PATH: %v", err)
	}
	cmd := exec.Command(pythonPath, "--version")
	if out, err := cmd.CombinedOutput(); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("python3 is not runnable: %v\n%s", err, out)
		}
		require.NoError(t, err, "python3 --version\n%s", out)
	}
	return pythonPath
}

var builtDocsRoutes = func() []string {
	pages := []string{
		"quickstart",
		"changelog",
		"contributing",
		"configuration",
		"usage",
		"activity",
		"data",
		"recent-edits",
		"session-intelligence",
		"mcp",
		"token-usage",
		"one-shot-capture",
		"chat-import",
		"quality",
		"commands",
		"session-export",
		"reporting-export",
		"stats",
		"session-api",
		"semantic-search",
		"semantic-search-internals",
		"recall",
		"remote-access",
		"artifact-sync",
		"filesystem-sync",
		"pg-sync",
		"hosted-raw-sync",
		"duckdb",
	}
	routes := []string{"/", "/guide/", "/docs/"}
	for _, page := range pages {
		routes = append(routes, "/docs/"+page+"/")
	}
	return routes
}()

func routeMarkdownPath(route string) string {
	switch route {
	case "/":
		return "/index.md"
	case "/docs/":
		return "/docs/index.md"
	default:
		return "/" + strings.Trim(route, "/") + ".md"
	}
}

func writeMinimalBuiltDocsSite(t *testing.T, siteDir string) {
	t.Helper()
	for _, route := range builtDocsRoutes {
		path := filepath.Join(siteDir, strings.Trim(route, "/"), "index.html")
		if route == "/" {
			path = filepath.Join(siteDir, "index.html")
		}
		ids := []string{}
		switch route {
		case "/docs/configuration/":
			ids = append(ids, "session-discovery")
		case "/docs/token-usage/":
			ids = append(ids, "how-it-compares-to-ccusage")
		case "/docs/session-api/":
			ids = append(ids, "agentsview-session-usage")
		}
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(minimalDocsHTML(route, ids)), 0o644))
	}
	require.NoError(t, os.WriteFile(filepath.Join(siteDir, "404.html"), []byte(minimalDocsHTML("", nil)), 0o644))
	require.NoError(t, os.WriteFile(
		filepath.Join(siteDir, "sitemap.xml"),
		[]byte("<urlset>"+
			"<url><loc>https://agentsview.io/</loc></url>"+
			"<url><loc>https://agentsview.io/guide/</loc></url>"+
			"<url><loc>https://agentsview.io/docs/</loc></url>"+
			"</urlset>\n"),
		0o644,
	))
}

func writeBuiltSiteMarkdownCompanions(t *testing.T, siteDir string) {
	t.Helper()
	var llms strings.Builder
	llms.WriteString("# AgentsView\n\n")
	for _, route := range builtDocsRoutes {
		markdownPath := routeMarkdownPath(route)
		path := filepath.Join(siteDir, filepath.FromSlash(strings.TrimPrefix(markdownPath, "/")))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte("# Page\n"), 0o644))
		llms.WriteString("- [Page](https://agentsview.io" + markdownPath + "): Page\n")
	}
	require.NoError(t, os.WriteFile(filepath.Join(siteDir, "llms.txt"), []byte(llms.String()), 0o644))
}

func minimalDocsHTML(route string, ids []string) string {
	var b strings.Builder
	b.WriteString(`<!doctype html><html><head>`)
	b.WriteString(`<meta property="og:image" content="https://agentsview.io/docs/assets/static/og-image.png">`)
	b.WriteString(`<meta property="og:image:width" content="1200">`)
	b.WriteString(`<meta property="og:image:height" content="630">`)
	b.WriteString(`<meta property="og:type" content="website">`)
	b.WriteString(`<meta property="og:site_name" content="AgentsView">`)
	b.WriteString(`<meta name="twitter:card" content="summary_large_image">`)
	b.WriteString(`<meta name="twitter:image" content="https://agentsview.io/docs/assets/static/og-image.png">`)
	if route != "" {
		b.WriteString(`<link rel="alternate" type="text/markdown" href="https://agentsview.io`)
		b.WriteString(routeMarkdownPath(route))
		b.WriteString(`">`)
	}
	b.WriteString(`</head><body>`)
	b.WriteString(`<a class="agentsview-discord-link" aria-label="Join Discord" href="https://discord.gg/fDnmxB8Wkq">Discord</a>`)
	for _, id := range ids {
		b.WriteString(`<h2 id="`)
		b.WriteString(id)
		b.WriteString(`">Heading</h2>`)
	}
	b.WriteString(`</body></html>`)
	return b.String()
}

func envWithout(names ...string) []string {
	blocked := make(map[string]struct{}, len(names))
	for _, name := range names {
		blocked[name] = struct{}{}
	}

	env := os.Environ()
	filtered := env[:0]
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		if _, ok := blocked[name]; !ok {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

func writeStaticAssets(t *testing.T, dir, content string) {
	t.Helper()
	files := []string{
		"architecture.svg",
		"og-image.png",
	}
	writeAssetFiles(t, dir, files, content)
}

func writeGeneratedAssets(t *testing.T, dir, content string) {
	t.Helper()
	files := []string{
		"screenshots/about-dialog.png",
		"screenshots/activity-breakdowns.png",
		"screenshots/activity-concurrency.png",
		"screenshots/activity-insight.png",
		"screenshots/activity-page.png",
		"screenshots/activity-sessions.png",
		"screenshots/activity-timeline.png",
		"screenshots/activity-week.png",
		"screenshots/agent-comparison.png",
		"screenshots/analytics-model-filter.png",
		"screenshots/block-filter.png",
		"screenshots/code-block-copy-btn.png",
		"screenshots/command-palette.png",
		"screenshots/data-inventory.png",
		"screenshots/data-workspace.png",
		"screenshots/dashboard.png",
		"screenshots/date-range.png",
		"screenshots/focused-transcript.png",
		"screenshots/follow-latest-toggle.png",
		"screenshots/grade-badge.png",
		"screenshots/heatmap-filtered.png",
		"screenshots/heatmap.png",
		"screenshots/hour-of-week.png",
		"screenshots/import-button.png",
		"screenshots/import-modal-chatgpt.png",
		"screenshots/import-modal-claude.png",
		"screenshots/in-session-search.png",
		"screenshots/quality.png",
		"screenshots/recall-generated-insights.png",
		"screenshots/layout-compact.png",
		"screenshots/layout-stream.png",
		"screenshots/machine-labels.png",
		"screenshots/message-copy-btn.png",
		"screenshots/message-viewer.png",
		"screenshots/project-breakdown.png",
		"screenshots/publish-modal.png",
		"screenshots/recall-corpus.png",
		"screenshots/recent-edits.png",
		"screenshots/resync-modal.png",
		"screenshots/search-grouped.png",
		"screenshots/search-results.png",
		"screenshots/semantic-search-setup.png",
		"screenshots/session-filtered.png",
		"screenshots/session-filters-active.png",
		"screenshots/session-filters.png",
		"screenshots/session-health.png",
		"screenshots/session-insight-action.png",
		"screenshots/session-list.png",
		"screenshots/session-resume-menu.png",
		"screenshots/session-shape.png",
		"screenshots/session-vital-signs.png",
		"screenshots/settings-chart-colors.png",
		"screenshots/settings-embeddings.png",
		"screenshots/settings-remote.png",
		"screenshots/settings.png",
		"screenshots/shortcuts-modal.png",
		"screenshots/signal-panel.png",
		"screenshots/skill-trends.png",
		"screenshots/starred-session.png",
		"screenshots/subagent-tree.png",
		"screenshots/summary-cards.png",
		"screenshots/theme-dark.png",
		"screenshots/theme-light.png",
		"screenshots/thinking-blocks.png",
		"screenshots/token-usage.png",
		"screenshots/tool-block-copy-btn.png",
		"screenshots/tool-blocks.png",
		"screenshots/tool-groups.png",
		"screenshots/tool-output-formatted.png",
		"screenshots/tool-usage.png",
		"screenshots/top-sessions.png",
		"screenshots/top-skills.png",
		"screenshots/trends.png",
		"screenshots/usage-attribution.png",
		"screenshots/usage-cache-efficiency.png",
		"screenshots/usage-cost-trend.png",
		"screenshots/usage-filter-dropdown.png",
		"screenshots/usage-page.png",
		"screenshots/usage-summary-cards.png",
		"screenshots/usage-toolbar.png",
		"screenshots/usage-top-sessions.png",
		"screenshots/velocity.png",
		"screenshots/vital-signs-panel.png",
		"screenshots/worktree-mappings.png",
	}
	writeAssetFiles(t, dir, files, content)
}

func writeAssetFiles(t *testing.T, dir string, files []string, content string) {
	t.Helper()
	for _, file := range files {
		path := filepath.Join(dir, file)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content+"\n"), 0o644))
	}
}

func commitBareAssetTree(
	t *testing.T, bareRepo, workTree, message string,
) string {
	t.Helper()
	indexPath := filepath.Join(t.TempDir(), "index")
	env := gitCommitEnv("GIT_INDEX_FILE=" + indexPath)
	gitBareWorkTree(t, bareRepo, workTree, env, "add", "-A", ".")
	tree := gitBareWorkTreeOutput(t, bareRepo, workTree, env, "write-tree")
	return gitBareOutput(t, bareRepo, env, "commit-tree", tree, "-m", message)
}

func updateBareBranch(t *testing.T, bareRepo, branch, commit string) {
	t.Helper()
	gitBare(t, bareRepo, nil, "update-ref", "refs/heads/"+branch, commit)
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
}

func gitBareWorkTree(
	t *testing.T, bareRepo, workTree string, env []string, args ...string,
) {
	t.Helper()
	output, err := gitBareCmd(bareRepo, workTree, env, args...).CombinedOutput()
	require.NoError(t, err, string(output))
}

func gitBareWorkTreeOutput(
	t *testing.T, bareRepo, workTree string, env []string, args ...string,
) string {
	t.Helper()
	output, err := gitBareCmd(bareRepo, workTree, env, args...).Output()
	require.NoError(t, err)
	return strings.TrimSpace(string(output))
}

func gitBare(t *testing.T, bareRepo string, env []string, args ...string) {
	t.Helper()
	output, err := gitBareCmd(bareRepo, "", env, args...).CombinedOutput()
	require.NoError(t, err, string(output))
}

func gitBareOutput(t *testing.T, bareRepo string, env []string, args ...string) string {
	t.Helper()
	output, err := gitBareCmd(bareRepo, "", env, args...).Output()
	require.NoError(t, err)
	return strings.TrimSpace(string(output))
}

func gitBareCmd(
	bareRepo, workTree string, env []string, args ...string,
) *exec.Cmd {
	fullArgs := []string{"--git-dir", bareRepo}
	if workTree != "" {
		fullArgs = append(fullArgs, "--work-tree", workTree)
	}
	fullArgs = append(fullArgs, args...)
	cmd := exec.Command("git", fullArgs...)
	if workTree != "" {
		cmd.Dir = workTree
	}
	cmd.Env = append(os.Environ(), env...)
	return cmd
}

func gitCommitEnv(extra ...string) []string {
	env := []string{
		"GIT_AUTHOR_NAME=Test User",
		"GIT_AUTHOR_EMAIL=test@example.invalid",
		"GIT_COMMITTER_NAME=Test User",
		"GIT_COMMITTER_EMAIL=test@example.invalid",
	}
	return append(env, extra...)
}
