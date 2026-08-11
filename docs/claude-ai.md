# Claude.ai sync

Claude.ai imports use an isolated authenticated desktop WKWebView. Session cookies remain in that browser profile and are never sent to the AgentsView daemon, cache, database, or scheduler configuration.

Automatic sync is optional. When enabled, the desktop application runs a native scheduler at the selected interval **only while AgentsView is running**. It does not wake a quit application or provide a headless/background service. Scheduled imports use the same browser summary → local marker plan → browser detail → local ingest protocol as manual imports.

The scheduler stores its enabled flag, interval, and credential-free last-run timestamps, counts, and error in `claude-sync-schedule.json` under the application data directory. The Claude cache records durable import progress and summary markers, so a later scan safely skips unchanged conversation details. **Repair import** explicitly refetches every conversation and clears only the matching Claude.ai permanent-deletion markers, allowing a user to restore an intentionally deleted Claude.ai archive without affecting other providers.
