---
title: Chat Import
description: Import Claude.ai, ChatGPT, and Gemini Apps conversations into AgentsView
---

AgentsView can import your conversation history from Claude.ai,
ChatGPT, and Gemini Apps. These services let you export your data as a zip
file — AgentsView reads these exports and adds the conversations
to your local database alongside your agent coding sessions.

## Exporting Your Data

### Claude.ai

1. Go to [claude.ai/settings](https://claude.ai/settings)
2. Scroll to **Export Data** and click **Export**
3. Claude emails you a download link for a `.zip` file
   containing `conversations.json`

### ChatGPT

1. Go to [chatgpt.com/settings](https://chatgpt.com/settings)
2. Under **Data controls**, click **Export data**
3. ChatGPT emails you a download link for a `.zip` file
   containing conversation data and any images you uploaded
   or generated with DALL-E

### Gemini Apps

1. Open [Google Takeout](https://takeout.google.com/)
2. Select **Gemini Apps** under **My Activity**
3. Create an export and download the resulting `.zip` file

AgentsView imports `Prompted` activity records. Canvas, feedback, and unknown
activity kinds are reported as skipped. Each prompt becomes a one-turn session;
the importer resolves the timestamp's explicit exported zone instead of using
the host timezone. Mixed Takeout archives may contain other product activity;
those explicitly identified cells are ignored. The current parser supports the
observed English rendering for Gemini Apps cells, including the named zones it
currently recognizes and complete `GMT±H`, `GMT±HH`, `GMT±H:MM`, and
`GMT±HH:MM` zones; omitted minutes mean zero. Declared non-English or otherwise
unsupported localized Gemini candidates and malformed zone tokens are reported
as unsupported before any sessions are emitted.

## Importing via the UI

The web import dialog currently supports Claude.ai and ChatGPT. Import Gemini
Apps exports with the [CLI](#importing-via-the-cli).

Click the **Import conversations** button in the header
(the upload icon in the top-right area) to open the import
dialog.

![Import button in header](/docs/assets/generated/screenshots/import-button.png)

1. **Select a provider** — choose Claude.ai or ChatGPT
2. **Upload your file**
   - Claude.ai: accepts `conversations.json` or the `.zip`
     from your data export
   - ChatGPT: accepts the `.zip` from your data export
3. Click **Import**

![Import modal — Claude.ai](/docs/assets/generated/screenshots/import-modal-claude.png)

![Import modal — ChatGPT](/docs/assets/generated/screenshots/import-modal-chatgpt.png)

The dialog shows a summary when finished — for example,
"5 conversations processed (4 new, 1 updated)". The session
list refreshes automatically.

## Sync Claude.ai chats

Setup registers Google Chrome only. Chromium, Edge, and Brave aren't supported.
Set up Chrome once:

1. Run `agentsview chrome setup`.
2. Open `chrome://extensions` and turn on **Developer mode**.
3. Click **Load unpacked** and select the folder printed by the command.

Keep Chrome open during Sync. Re-run setup and reload the extension at `chrome://extensions` when the executable path or data directory changes, or when Sync asks for it after an upgrade. If the extension is newer than the server, upgrade AgentsView. Then click **Sync** again.
The extension reconnects when AgentsView restarts. Only one Chrome profile connects at a time. The first profile to connect keeps the connection; other profiles wait until it disconnects.

There's no removal command. Remove the extension at `chrome://extensions`, then
delete `<dataDir>/chrome/extension/` and the launcher, `<dataDir>/chrome/host.cmd`
on Windows or `<dataDir>/chrome/host` on macOS and Linux. Also delete the native
host registration for your platform:

- Windows: `<dataDir>/chrome/io.kenn.agentsview.json` and the registry key
  `HKEY_CURRENT_USER\Software\Google\Chrome\NativeMessagingHosts\io.kenn.agentsview`.
- macOS: `~/Library/Application Support/Google/Chrome/NativeMessagingHosts/io.kenn.agentsview.json`.
- Linux: `<configRoot>/NativeMessagingHosts/io.kenn.agentsview.json`. Chrome uses `CHROME_CONFIG_HOME` first, then `$XDG_CONFIG_HOME/google-chrome`, then `~/.config/google-chrome`.

If Sync reports a disconnected host after setup, open Chrome and enable the extension at `chrome://extensions`. If it still fails, check the server startup error log for a Chrome host endpoint bind failure.

**Sync** appears in every local web UI connected to a writable archive. A disconnected Chrome host is reported when you click **Sync**. Open **Import conversations**, select **Claude.ai**, and click **Sync**. Chrome uses your existing Claude.ai session, including Google sign-in. If you need to sign in, click **Sign in**, finish in the new tab, then return to AgentsView and click **Sync**.

You can also run `agentsview import --type claude-ai --sync` while the server and Chrome are running, with no AgentsView tab open.

Sync reuses an open Claude.ai tab. When none is available, it opens a background Claude.ai tab and leaves it open.

In the desktop app connected to its local archive, click **Sign in**, use an email code, close the sign-in window, then click **Sync**. The desktop sign-in window supports email codes only.

Sync checks every chat, including archived chats. It fetches new chats and chats whose `updated_at`, visible leaf, or stored transcript changed. A zip re-import that changes text or message count triggers another fetch. Resync clears freshness, so the next Sync fetches each chat once. Search stays available during Sync.

Changed chats show Claude.ai's visible branch, even when it has fewer turns. If a replacement loses a pin or note, Sync keeps the previous version in Trash with its pins and notes. Replacements that preserve every pin and note make no copy. Switching back on Claude.ai restores those turns, but dropped pins and notes stay in the Trash copy. Trashed and permanently deleted chats stay deleted.

Each chat has a 32 MiB response limit. Larger chats count as failed while Sync continues. An expired sign-in or two chat failures in a row ends Sync. Closing the dialog cancels it. Completed chats stay imported, and the next Sync fetches unfinished chats.

Sign-in persists in the browser that holds it. To sign out, open **Sign in** and use Claude.ai's own log-out menu. Credentials stay in Chrome or the desktop sign-in window. Sync requires a local connection and either the Chrome native host or the desktop app; file imports remain available in the web UI.

Claude.ai's private endpoints can change without notice. Sign in again if Sync reports that your sign-in expired.

## Importing via the CLI

Use `agentsview import` to import from the command line:

```bash
agentsview import --type claude-ai ~/Downloads/claude-export.zip
agentsview import --type chatgpt ~/Downloads/chatgpt-export.zip
agentsview import --type gemini-apps ~/Downloads/takeout.zip
```

| Flag | Description |
|------|-------------|
| `--type` | `claude-ai`, `chatgpt`, or `gemini-apps` (required) |
| `--replace` | Session ID whose archived messages the import may replace when the normal import refuses them; repeat for more sessions (`claude-ai` and `chatgpt` only). See [Replacing archived history](#replacing-archived-history). |

The path can be a `.zip` file, a `conversations.json` file
(Claude.ai only), a Gemini Apps `MyActivity.html` file, or a
directory containing the extracted export.

## What Gets Imported

### Messages

Conversation turns are imported as sessions. Providers that emit
thinking/reasoning blocks or tool usage preserve those message types;
each Gemini Apps `Prompted` record becomes one user message containing its
complete visible plain text. HTML presentation does not infer speaker roles;
inline code and preformatted text remain text without generated Markdown.

### Images (ChatGPT)

ChatGPT exports include images — both DALL-E generations
and files you uploaded during conversations. AgentsView
extracts these from the zip and stores them locally in the
data assets directory. Images appear inline in the message
viewer, just as they did in the original conversation.

Supported formats: PNG, JPG, JPEG, WebP, GIF.

### Metadata

Each imported session includes:

- Conversation title as the session display name
- Created and updated timestamps
- Message and user message counts
- Model information (when available)

## How Imported Sessions Appear

Imported conversations appear in the session list alongside
your locally-tracked agent sessions. They are grouped under
the **claude.ai**, **chatgpt.com**, or **gemini.google.com** project, so you can
filter to them using the project filter or browse them
mixed in with your other sessions.

Imported sessions support the same features as any other
session: search, export, publish to Gist, insights, pinned
messages, and analytics.

## Re-importing

You can safely re-import the same export file:

- **Claude.ai** — existing sessions are updated with any
  new messages. User-edited display names are preserved. An export
  with fewer messages than the archived session (for example an older
  export) is reported as an error and leaves the stored session unchanged
  unless you explicitly replace it with `--replace`.
  Equal-length or longer exports can refresh earlier messages, including
  attachment text; earlier turns do not have to match the archive.
- **ChatGPT** — unchanged sessions are skipped. An export may add messages when
  every archived message still matches the beginning of the export. The match
  compares message text and each archived tool call's name and category, and
  checks whether its result is empty. An archived message whose text is the
  start of the export's text for the same turn, such as a copy cut short by an
  earlier conversion, takes the export's full text in place; it keeps its place
  and any pin. Empty archived text never counts as a cut copy. Extra tool calls
  in the export are ignored. A tool result that was empty when archived and is
  filled in the export is written into the archived message, which keeps its
  place and any pin. For example, you export during a code run, keep chatting,
  and export again: the re-import adds the run's output and the new turns. Other
  result differences keep the archived result. Shorter exports and other changes
  are reported as errors and leave the archive unchanged unless you explicitly
  replace the session with `--replace`. Trashed conversations are skipped. User
  display names are preserved.
- **Gemini Apps** — existing sessions are matched by the canonical UTC
  timestamp and its zero-based occurrence among records sharing that
  timestamp. Inserting or reordering records with other timestamps doesn't
  change existing IDs. Content changes update the same one-message session;
  unchanged records are skipped.

Claude.ai and ChatGPT import results list each conversation that could not be
imported under `refusals`, with its AgentsView session ID
(`chatgpt:<conversation id>` or `claude-ai:<conversation uuid>`, the export ID
with the provider prefix) and a reason:

- `diverged`: the export changes archived messages.
- `shorter_export`: the export has fewer messages than the archive.
- `trashed`: a Claude.ai or Gemini Apps session is in the trash. Restore it
  first. A trashed ChatGPT conversation stays a skip.
- `transient`: anything else. Importing again may work.

For `diverged` or `shorter_export`, you can explicitly
[replace the archived history](#replacing-archived-history). Otherwise, these
refusals repeat on every import of the same export. A `trashed` refusal repeats
until you restore the session.

Streamed progress events carry only the counts. The CLI summary shows the same
reasons next to its error count. Gemini Apps records the parser rejects before
they have a session ID are counted in `errors` without an entry.

## Replacing archived history

Use replace mode when a session's archived copy is wrong and every re-import
refuses it, for example because messages were cut short or the export has a
different set of messages than the archive. List each session to replace by
its ID, `chatgpt:<conversation id>` or `claude-ai:<conversation uuid>`:

```bash
agentsview import --type chatgpt --replace 'chatgpt:<conversation-id>' ~/Downloads/chatgpt-export.zip
```

Repeat `--replace` for each session. The import API takes the same list as a
repeatable query parameter:

```text
POST /api/v1/import/chatgpt?replace=<session-id>&replace=<session-id>
POST /api/v1/import/claude-ai?replace=<session-id>
```

What happens to a listed session:

- It's replaced only when the normal import would refuse it. An export that
  adds messages or fills empty tool results still updates it in place.
- Its messages become exactly the export's messages. The session keeps its
  name, and pins stay on messages that still match.
- The previous version moves to the trash as `<session-id>:replaced:<time>`
  with its messages, name, and pins. Restore it from Trash if you need it back.
- Importing the same export again changes nothing and adds no second copy.
- A trashed session stays trashed and isn't replaced. A session that belongs
  to a different agent is refused as before.

Sessions you don't list are imported as usual. Gemini Apps imports don't
support `--replace`.
