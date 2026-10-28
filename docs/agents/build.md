# Build and Dependency Rules

Read this file before changing build commands, toolchain setup, CI build tags,
or frontend dependencies. Use the `Makefile` as the command reference.

## CI runner policy

All CI jobs use standard GitHub-hosted runners, including fork pull requests,
scheduled checks, and desktop builds. Select labels from
[GitHub's supported runners](https://docs.github.com/en/actions/reference/runners/github-hosted-runners).
Keep the explicit architecture labels for Linux ARM64 and macOS desktop builds.

Use the local `go-cache-restore` and `go-cache-save` actions for Go caches and
disable the built-in cache in `actions/setup-go`. Use npm caching in
`actions/setup-node`. Set `cache-dependency-path` to the lockfiles for the
packages the job installs. Desktop jobs cache Rust dependencies through
`Swatinem/rust-cache` with the GitHub cache backend. Pull-request workflow
callers set `cache-mode: read` so jobs can restore caches without saving copies.
Playwright jobs run browsers in Microsoft's public MCR image through a remote
server.

`desktop-artifacts.yml` builds the frontend once on Linux and packages those
assets in its Windows, Linux, and Linux ARM64 desktop builds.

Go builds and tests use `-trimpath` everywhere: the `Makefile` exports it in
`GOFLAGS`, the Go workflows set `GOFLAGS: -trimpath`, and desktop sidecar builds
pass it unless `AGENTSVIEW_SIDECAR_TRIMPATH=false`. Matching flags let desktop
builds reuse compile results from test jobs and let local worktrees share one
build cache. See the [README](../../README.md#shared-go-build-cache) for opting
out and debugging.

Windows Go test and lint jobs in `ci.yml` put caches and temp files under
`RUNNER_TEMP`. Capture tests use a private system-drive temporary directory so
ancestor permissions satisfy their checks.

## Go and SQLite

- Build with `CGO_ENABLED=1`; the SQLite driver requires CGO.
- Use the `fts5` build tag for full-text search.
- The Makefile's `LDFLAGS` set the PostHog project key with
  `-X go.kenn.io/kit/telemetry/posthog.projectKey`. Builds without it send no
  telemetry, so keep it out of test and e2e builds.
- `DISTRIBUTION` sets `main.distribution` and
  `go.kenn.io/kit/telemetry/posthog.distribution` to who ships the binary.
  Official direct downloads use `kenn` and keep self-updating, like builds
  with no value. Any other value names a package manager: those builds refuse
  `agentsview update` installs and point users at the package manager.
  `make homebrew` is the release build with `DISTRIBUTION=homebrew`, written
  to `$(BINDIR)/agentsview`.

## Frontend

- The embedded Svelte frontend requires Node.js and the frontend toolchain. Read
  `frontend/AGENTS.md` before working in that directory.
- `@kenn-io/kit-ui` is a public git dependency pinned to a commit in
  `frontend/package.json`.
- The lockfile records the GitHub dependency as an SSH URL because npm uses that
  canonical form. npm still fetches it anonymously over HTTPS. Do not rewrite
  the lockfile URL.
- To update kit-ui, change the commit hash in `frontend/package.json` and run
  `npm install`.
