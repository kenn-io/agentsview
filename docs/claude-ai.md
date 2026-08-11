# Claude.ai sync

Claude.ai imports use an isolated authenticated desktop WKWebView. Session cookies remain in that browser profile and are never sent to the AgentsView daemon, cache, database, or scheduler configuration.

The Go server owns the sync job: pagination, retries, cache markers, repair, cancellation, scheduling and SQLite import. Tauri is only an authenticated transport adapter: it receives an allow-listed typed request from the local server and returns the Claude response. This permits another desktop transport to serve the same job model later without moving archive logic out of the generic web application.

Automatic sync is optional and runs only while AgentsView is running. The Go scheduler stores its credential-free configuration under `cloud-cache/claude-ai/schedule.json`; it waits for an authenticated desktop transport rather than reading browser credentials. The cache records durable import progress and summary markers, so a later scan safely skips unchanged conversation details. **Repair import** explicitly refetches every conversation, replaces cached content even when timestamps match, and clears only matching Claude.ai permanent-deletion markers.
