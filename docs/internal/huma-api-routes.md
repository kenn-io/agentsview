# Huma API Routes

The server API is registered with Huma route groups. Keep each route group
self-contained so OpenAPI ownership stays close to runtime behavior.

## File Ownership

- Put route registration, endpoint-specific input types, response wrapper types,
  enums, and handlers in the route group file that owns the path.
- Keep `internal/server/huma_routes.go` limited to shared Huma plumbing: API
  configuration, route registration helpers, common path/query inputs, error
  conversion, schema naming, SSE/write helpers, and middleware.
- Move a helper into shared plumbing only when at least two route groups use it
  and it has no domain-specific policy.
- Do not add new typed handlers to a catch-all API file. Add a new group file
  when a new API area does not fit an existing group.

## Compatibility Guardrails

When changing route registration or generated client contracts:

- Preserve existing paths, methods, status codes, response events, and content
  types unless the change is intentional and covered by tests.
- Add or update parity tests for JSON bodies, raw downloads, multipart imports,
  SSE terminal events, and error responses touched by the change.
- Run `npm run generate:api` from `frontend/` and verify generated output only
  changes when the OpenAPI contract intentionally changed.
- Keep generated frontend code under `frontend/src/lib/api/generated/`; it is
  marked as generated in `.gitattributes` and should not be hand-edited.

## Generated contract and clients

Run `npm run generate:api` in `frontend/` to regenerate the committed
[`openapi.yaml`](../../openapi.yaml), the Orval TypeScript client, and the
DoorDash Go client in `internal/apiclient`. The Go client currently covers the
session-watch stream. `npm run check:api` checks all three outputs for drift.
The standalone `agentsview openapi --yaml` command prints the same schema
without opening an archive or starting a server.

Prek and CI run the shared `huma-check` linter pinned to the merge of kit PR
#84. The `client` rule is temporarily disabled under kata task 58fz because that
version mistakes the GitHub gist and user endpoints for local route-group paths.
Remove the exclusion after the upstream matcher is fixed. A full audit can be
run by omitting `-disable client` from the hook command.

## Collection nullability

AgentsView uses `encoding/json/v2`, which encodes a nil Go slice as an empty
JSON array. Server initialization sets `huma.DefaultArrayNullable` to `false`
before Huma builds the schema, so ordinary slice fields have the same
non-nullable array contract in OpenAPI 3.1 and on the wire.

- Use an ordinary slice for an array that may be empty but never null.
- Use an explicit nullable representation and `nullable:"true"` only when `null`
  is part of the wire contract.
- Use generated response types in stores and components. Do not cast a generated
  response to a handwritten wire type; fix the schema or add a typed adapter
  instead.
