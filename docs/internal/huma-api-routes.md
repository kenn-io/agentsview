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
