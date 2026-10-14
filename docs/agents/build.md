# Build and Dependency Rules

Read this file before changing build commands, toolchain setup, CI build tags,
or frontend dependencies. Use the `Makefile` as the command reference.

## CI runner policy

All CI jobs use standard GitHub-hosted runners, including fork pull requests,
scheduled checks, and desktop builds. Select labels from
[GitHub's supported runners](https://docs.github.com/en/actions/reference/runners/github-hosted-runners).
Keep the explicit architecture labels for Linux ARM64 and macOS desktop builds.

Use the built-in Go cache in `actions/setup-go` and npm caching in
`actions/setup-node`. Set `cache-dependency-path` to the lockfiles for the
packages the job installs. Desktop jobs cache Rust dependencies through
`Swatinem/rust-cache` with the GitHub cache backend. Playwright jobs install
browsers and system dependencies on each run.

## Go and SQLite

- Build with `CGO_ENABLED=1`; the SQLite driver requires CGO.
- Use the `fts5` build tag for full-text search.
- Do not add the `kit_posthog_disabled` tag to `go test`. The telemetry reporter
  disables itself under `testing.Testing()`. E2E binaries run as real
  processes, so their build keeps the tag.

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
