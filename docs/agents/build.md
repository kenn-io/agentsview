# Build and Dependency Rules

Read this file before changing build commands, toolchain setup, CI build tags,
or frontend dependencies. Use the `Makefile` as the command reference.

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
