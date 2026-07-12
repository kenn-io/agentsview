# SPA Cache Busting Design

## Problem

Browsers can retain an old SPA entry document after the embedded frontend is
replaced. The stale document requests an obsolete fingerprinted JavaScript or
CSS asset. The server currently treats that missing asset as a client-side route
and returns `index.html` with status 200, so the browser receives HTML where it
expected JavaScript or CSS. A fresh browser profile avoids the stale entry and
hides the failure.

## Design

Keep SPA routing behavior while making cache intent and asset failures
unambiguous:

- Serve `index.html`, including base-path-injected and client-route fallback
  responses, with `Cache-Control: no-cache` so browsers revalidate it.
- Serve existing files below `/assets/` with
  `Cache-Control: public, max-age=31536000, immutable`. Vite fingerprints
  these filenames, so their contents do not change at a stable URL.
- Return status 404 for missing files below `/assets/` instead of serving the
  SPA entry document. Other missing paths remain client-side route fallbacks.

The implementation remains in the existing SPA handler. It does not change the
frontend build, generated assets, or component rendering.

## Verification

HTTP-level Go tests will use a small in-memory filesystem and assert the status,
body, content type, and cache headers visible to a browser. Tests will cover the
entry document, a fingerprinted asset, a missing asset, and the existing
client-route fallback behavior.
