# Build and Dependency Rules

Read this file before changing build commands, toolchain setup, CI build tags,
or frontend dependencies. Use the `Makefile` as the command reference.

## Namespace runner policy

The repository's Namespace runner profiles use **Restricted** Access Level, as
confirmed by the maintainer. This setting is managed in the Namespace profile
editor under **Advanced Settings**, outside the workflow YAML. Namespace
documents Restricted mode for untrusted third-party code: it disables the runner
workload's access to Namespace APIs. See
[Namespace access levels](https://namespace.so/docs/solutions/github-actions/runner-controls/access-levels).

Running fork pull requests on these profiles is intentional. Do not infer
unrestricted infrastructure or credential access from a Namespace runner label
or GitHub's self-hosted classification alone. A security finding must identify
the resource an untrusted job can reach, the boundary it crosses, and evidence
that the configured restrictions allow that access. If runtime enforcement
cannot be checked, state that limitation instead of claiming an exposure or
guaranteeing isolation.

Restricted API access and cache persistence are separate controls. Evaluate
cache findings against the actual profile and cache-volume settings, including
which branches may persist updates. See
[Namespace cache protection](https://namespace.so/docs/solutions/github-actions/caching#protect-caches-from-updates).
Do not treat Restricted mode as proof of cache protection. Revisit this policy
when the runner configuration changes.

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
