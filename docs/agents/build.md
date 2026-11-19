# Build and Dependency Rules

Read this file before changing build commands, toolchain setup, CI build tags,
or frontend dependencies. Use the `Makefile` as the command reference.

## CI path filters

Update the CI path filters when adding a reader of files in another lane.

## Merge queue checks

Every PR runs the full CI suite before it can join the merge queue, and every
push to `main` runs it again. A merge group therefore runs only the checks that
two PRs, each green on its own, are likely to break together:

- `lint` (golangci-lint with the Huma API contract check, and NilAway)
- `frontend` (formatting, type checks, design-system checks, unit tests)
- `Docs and scripts`
- the Linux `test` suites: Sync, Other packages (including the SQLite schema and
  data-version tests in `internal/db` and `internal/parser`), and Eval ingest
- `e2e (chromium)`

Two PRs that bump a version to the same number make the same edit, so Git merges
them without a conflict. For the SQLite data version, the merge group catches
this: `TestParsedOutputMatchesDataVersion` fails when one version has two bump
records, and two `dataVersionScopes` entries for one version do not compile.

Merge groups skip `test-extended` (Windows, macOS, the x86-64-v2 startup check,
and the simulator), the isolated Windows suites, the race detector, the
PostgreSQL, CockroachDB, and ClickHouse integration suites, WebKit e2e, Node 26
frontend tests, desktop unit tests, and the released-command check. PostgreSQL
and ClickHouse schema changes therefore rely on the integration suites that run
on every PR and on `main`. A break that only a skipped suite catches surfaces in
`main` CI after the merge.

When you add a job, decide which group it belongs to. A job skips merge groups
when its `if` includes `github.event_name != 'merge_group'`. A matrix cannot
drop an `include` entry by event, so split queue entries into their own job (as
`test` and `test-extended` share steps through a YAML anchor), or list the
matrix values in an expression (as `e2e` does). Branch protection requires
`CI gate`, not individual CI jobs, so these changes need no ruleset update.

## CI runner policy

CI jobs use standard GitHub-hosted runners, including fork pull requests,
scheduled checks, and desktop builds. Select labels from
[GitHub's supported runners](https://docs.github.com/en/actions/reference/runners/github-hosted-runners).
Keep the explicit architecture labels for Linux ARM64 and macOS desktop builds.

The exceptions are the seconds-long `CI gate`, `changes`, and
`Sibling pins on main` jobs. They run on the Namespace profile
`namespace-profile-linux-1cpu` so they do not wait in the GitHub-hosted queue.

Use the local `go-cache-restore` and `go-cache-save` actions for Go caches and
disable the built-in cache in `actions/setup-go`. Use npm caching in
`actions/setup-node`. Set `cache-dependency-path` to the lockfiles for the
packages the job installs. Desktop jobs cache Rust dependencies through
`Swatinem/rust-cache` with the GitHub cache backend. Pull-request workflow
callers set `cache-mode: read` so jobs can restore caches without saving copies.
Cache owners opt in with `owner: true` to keep build entries used during the run
and outputs referenced by fresh action records when refreshing a daily cache.
Module caches stay on their existing restore and save paths, with all modules
retained. Playwright jobs run browsers in Microsoft's public MCR image through a
remote server.

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
- Release builds must start on x86-64-v2 CPUs. CI builds the `fts5` binary and
  starts it under QEMU on a Nehalem CPU model (x86-64-v2), so a dependency
  that needs a newer CPU at startup fails CI. Check a new cgo or prebuilt
  native library against that baseline before adding it.
- Keep the Makefile's release-only link settings out of test and e2e builds.
- `DISTRIBUTION` identifies who ships the binary.
  Official direct downloads use `kenn` and keep self-updating, like builds
  with no value. Any other value names a package manager: those builds refuse
  `agentsview update` installs and point users at the package manager.
  `make homebrew` is the release build with `DISTRIBUTION=homebrew`, written
  to `$(BINDIR)/agentsview`.

## Kit checkers

`tools/go.mod` pins the kit checkers that CI and the hooks run: `kennlint` and
`huma-check`. Makefile targets run them with `go tool -modfile=tools/go.mod`, so
`tools/go.sum` verifies them.

Do not run kit tools as `go run go.kenn.io/kit/cmd/...@version`. That form also
fetches the newest kit `go.mod` to look for a deprecation notice and verifies it
against the checksum database. A kit release the checksum database has not
recorded yet then fails every such command.

`kennlint config -check` compares `.golangci.yml` with the config from the kit
version in `tools/go.mod`. That config must match the kit golangci plugin in
`.custom-gcl.yml`. Bump both together with
`go -C tools get -tool go.kenn.io/kit/cmd/kennlint@<version>` and
`go -C tools mod tidy`. Renovate skips `tools/go.mod` for this reason.

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
