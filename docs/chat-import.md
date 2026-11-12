---
title: Chat Import
description: Import Claude.ai, ChatGPT, Gemini Apps, and Grok web conversations into AgentsView
---

You can bring your Claude.ai, ChatGPT, Gemini Apps, and Grok web conversations
into the same searchable archive as your coding sessions. You have two ways to
bring them in: sync Claude.ai, ChatGPT, Gemini Apps, and Grok web chats through
Chrome to fetch new and changed chats, or import export files from Claude.ai,
ChatGPT, and Gemini Apps. Grok web chats can only be imported through
[Sync](#sync-grok-web-chats). ChatGPT dot, Codex web, and Codex cloud task
chats use the separate opt-in
[Codex Cloud source](/docs/configuration/#codex-cloud).

## Export your data

### Claude.ai

1. Open [claude.ai/settings](https://claude.ai/settings).
2. Scroll to **Export Data** and click **Export**.
3. Download the `.zip` file containing `conversations.json` from the link
   Claude emails you.

### ChatGPT

1. Open [chatgpt.com/settings](https://chatgpt.com/settings).
2. Under **Data controls**, click **Export data**.
3. Download the `.zip` file from the link ChatGPT emails you. It contains your
   conversation data along with any images you uploaded or generated with
   DALL-E.

### Gemini Apps

1. Open [Google Takeout](https://takeout.google.com/).
2. Under **My Activity**, select **Gemini Apps**.
3. Create an export and download the `.zip` file.

Import the `.zip`, the extracted folder, or
`Takeout/My Activity/Gemini Apps/MyActivity.html` itself. Each `Prompted`
activity record becomes a session with your prompt and Gemini's response.
Other activity, such as feedback, is reported as skipped. Mixed Takeout
archives may contain other products' activity; AgentsView ignores it.

The parser reads the English rendering of the export. Takeout labels each
timestamp with a time zone abbreviation, such as `PST` or `IST`. AgentsView
uses your computer's time zone to resolve an abbreviation it shares, and also
knows the US zones, `UTC`, `GMT`, and `GMT+5:30`-style offsets. If the import
reports an unsupported zone, rerun it with `TZ` set to your Google account's
time zone, for example `TZ=Asia/Kolkata agentsview source import ...`.
Non-English exports are reported as unsupported before anything is imported.

## Import from the web UI

The web import dialog currently supports Claude.ai and ChatGPT. To import
Gemini Apps exports, use the [CLI](#importing-via-the-cli).

To open the import dialog, click **More actions**, the `…` button in the
header, and select **Import conversations**.

![Import button in header](/docs/assets/generated/screenshots/import-button.png)

1. Select a provider: **Claude.ai** or **ChatGPT**.
2. Upload your export file:
   - **Claude.ai.** Upload `conversations.json` or the `.zip` from your data
     export.
   - **ChatGPT.** Upload the `.zip` from your data export.
3. Click **Import**.

![Import modal — Claude.ai](/docs/assets/generated/screenshots/import-modal-claude.png)

![Import modal — ChatGPT](/docs/assets/generated/screenshots/import-modal-chatgpt.png)

When the import finishes, the dialog shows a summary such as "5 conversations
processed", with separate counts for new and updated conversations below it,
plus unchanged and error counts when they are nonzero. The session list
refreshes automatically.

## Sync Claude.ai chats

Chrome Sync registers Google Chrome only; Chromium, Edge, and Brave are not
supported. Set up Chrome once:

1. Run `agentsview source chrome setup`.
2. If AgentsView is already running, restart it. AgentsView opens its Chrome
   connection only after setup, so a server started before setup cannot reach
   Chrome.
3. Open `chrome://extensions` in Chrome and turn on **Developer mode**.
4. Click **Load unpacked** and select the folder printed by the setup command.

Keep Chrome open while Sync runs. If your executable path or data directory
changes, rerun setup and reload the extension at `chrome://extensions`. After
an upgrade mismatch, restart AgentsView if you upgraded it, rerun setup, reload
the extension, and click **Sync** again. The extension reconnects automatically
when AgentsView restarts. Registration is per OS user, and the last setup wins.
AgentsView uses the first Chrome profile that connects until it disconnects.

Most installs need no extra configuration. Chrome reaches AgentsView through a
private socket at `<dataDir>/chrome/host.sock`, and setup writes that path into
the launcher. If your data directory path is too long for a socket, AgentsView
places the socket in `av-chrome-<hash>/` inside `$XDG_RUNTIME_DIR`, or inside
your temp directory when `$XDG_RUNTIME_DIR` is not set. On macOS and Linux,
that directory must belong to you with mode 0700, so a shared `/tmp` does not
qualify. If no directory qualifies, setup fails and the server logs why; use a
shorter data directory or set `$XDG_RUNTIME_DIR`. If setup and the server see
different values of `$XDG_RUNTIME_DIR` or `$TMPDIR`, Chrome cannot connect, and
you need to rerun setup after changing either. The server removes the socket
and its directory when it stops.

Earlier development builds placed that socket in `~/av-chrome-<hash>/`. If you
ran setup with one of those builds, rerun `agentsview source chrome setup` and
reload the extension. Until you do, the server logs a warning at startup and
Chrome stays disconnected. You can delete any old `~/av-chrome-<hash>/`
directories.

There is no removal command. To uninstall Chrome Sync, remove the extension at
`chrome://extensions`, then delete `<dataDir>/chrome/extension/` and the
launcher, `<dataDir>/chrome/host.cmd` on Windows or `<dataDir>/chrome/host` on
macOS and Linux. Also delete the native host registration for your platform:

- **Windows.** Delete `<dataDir>/chrome/io.kenn.agentsview.json` and the
  registry key
  `HKEY_CURRENT_USER\Software\Google\Chrome\NativeMessagingHosts\io.kenn.agentsview`.
- **macOS.** Delete
  `~/Library/Application Support/Google/Chrome/NativeMessagingHosts/io.kenn.agentsview.json`.
- **Linux.** Delete
  `<configRoot>/NativeMessagingHosts/io.kenn.agentsview.json`. Chrome checks
  `$CHROME_CONFIG_HOME/google-chrome` first, then
  `$XDG_CONFIG_HOME/google-chrome`, then `~/.config/google-chrome`.

If Sync reports a disconnected host after setup, restart AgentsView if it was
running during setup, then open Chrome and enable the extension at
`chrome://extensions`. If it still fails, check the server startup error log
for a Chrome host endpoint bind failure.

To start a sync from the UI, open **Import conversations** and select
**Claude.ai**. Local web UIs connected to a writable archive show a Chrome
card; other connections show file upload alone. The card checks the connection
automatically, and **Sync** stays disabled until Chrome connects. **Not
connected** asks you to open Chrome and shows the setup command and extension
loading steps. Once the card shows **Connected**, **Sync** is enabled and uses
your existing Claude.ai session, including Google sign-in.

![Claude.ai Sync with Chrome connected](/docs/assets/generated/screenshots/import-claude-sync.png)

If Sync needs authentication, the card shows **Signed out**. Click **Sign in**,
complete sign-in in the Chrome profile that has the AgentsView extension, then
return and click **Sync**. Enable the extension in only one Chrome profile.
Sync uses the first profile that connects. **Update needed** keeps **Sync**
enabled and shows the restart, setup, and reload steps described above. While
Sync runs, progress replaces the file upload area and **Stop** cancels the run.
Finished runs show the new, updated, unchanged, and error counts. Other
failures show **Sync failed** with a **Retry** button.

Closing the dialog does not stop Sync. While it runs, the **Import** button in
the header shows a spinner, and you can reopen the dialog to check progress,
stop the run, or read a result or failure that arrived while it was closed.
Reloading or closing the AgentsView page ends Sync.

You can also run `agentsview source import --type claude-ai --sync` while the
server and Chrome are running, with no AgentsView tab open.

Sync reuses an open Claude.ai tab. If none is open, it opens a background
Claude.ai tab and leaves it open.

In the desktop app connected to its local archive, click **Sign in**, sign in
with an email code, close the sign-in window, and then click **Sync**. The
desktop sign-in window supports email codes only.

Sync checks every chat, including archived chats. It fetches new chats and
chats whose `updated_at`, visible leaf, or stored transcript changed. A zip
re-import that changes text, message count, or title triggers another fetch.
Title-only changes count as updates and refresh open session lists. Resync
clears freshness, so the next Sync fetches each chat once. Search stays
available while Sync runs.

### Choose which organizations Sync imports

A Claude.ai login can belong to more than one organization, such as a personal
plan and a Team or Enterprise workspace. By default, Sync imports every
organization that has chats. To limit which organizations Sync imports, list
their UUIDs in `config.toml`:

```toml
[agents.claude-ai]
organizations = ["11111111-1111-4111-8111-111111111111"]
```

- Sync skips every organization whose UUID is not listed. Case and surrounding
  spaces do not matter.
- An empty list, or omitting the `organizations` key, imports every
  organization.
- A value that is not a list of non-empty strings, or any other key in
  `[agents.claude-ai]`, stops Claude.ai Sync with an error naming
  `config.toml`. A misspelled key never widens the import.
- The server reads this setting when it starts. Restart AgentsView after
  editing it.
- The list only limits what Sync fetches going forward. Chats imported earlier
  stay in the archive.

Changed chats follow Claude.ai's visible branch, even when it has fewer turns.
Usage-only archives also update their message and user-turn counts to match
that branch, and the first Sync after upgrading refetches cached usage-only
chat details once to repair their counts. If a replacement loses a pin or note,
Sync keeps the previous version in Trash with its pins and notes. Replacements
that preserve every pin and note make no Trash copy. Switching back on
Claude.ai restores those turns, while dropped pins and notes remain in the
Trash copy. Trashed and permanently deleted chats stay deleted.

Each chat has a 32 MiB response limit. Larger chats count as failed while Sync
continues. An expired sign-in or two chat failures in a row ends Sync.
Completed chats stay imported and refresh in the session list, and the next
Sync fetches unfinished chats. After you click **Stop**, the server may briefly
report that Chrome Sync is already running; wait a moment, then click
**Retry**.

Sign-in persists in the browser that holds it. To sign out of Chrome, open
Claude.ai in Chrome and use its log-out menu. In the desktop app, open
**Sign in** and use Claude.ai's log-out menu there. Credentials stay in Chrome
or the desktop sign-in window. Sync requires a local connection and either the
Chrome native host or the desktop app; file imports remain available in the web
UI.

Claude.ai's private endpoints can change without notice. If Sync reports that
your sign-in expired, sign in again.

## Sync ChatGPT chats

You can sync ChatGPT from the **Import conversations** dialog or from the
command line.

1. Set up the Chrome extension by following
   [Sync Claude.ai chats](#sync-claudeai-chats). If you set it up before this
   release, rerun `agentsview source chrome setup`, reload the extension at
   `chrome://extensions`, and approve its new access to `chatgpt.com`.
2. Sign in to ChatGPT in the Chrome profile that has the extension enabled.
3. Open **Import conversations**, select **ChatGPT**, and click **Sync** on the
   Chrome card above the ZIP upload. Alternatively, with AgentsView and Chrome
   running, run:

   ```bash
   agentsview source import --type chatgpt --sync
   ```

The ChatGPT card behaves like the Claude.ai card: it shows the Chrome
connection status, shows **Sign in** with **Signed out** to open `chatgpt.com`
in a new tab, and keeps Sync running if you close the dialog. The desktop app
also syncs ChatGPT through Chrome; its own sign-in window serves Claude.ai
only.

Sync reuses an open ChatGPT tab. If none is open, it opens one in the
background and leaves it open.

Inside the ChatGPT page, the extension reads a short-lived access token and
uses it only for the allowed conversation-list and conversation requests. That
token never reaches AgentsView or native messaging; only the conversation
responses do. Sync requires a local writable archive and a connected Chrome
extension.

What Sync fetches:

- Both active and archived chat lists. Coverage of Project chats and Business
  workspaces is unverified.
- Only chats whose list `update_time` differs from the last Sync, or whose
  stored copy changed since then. A chat whose list and conversation timestamps
  differ by more than 5 seconds is fetched again on the next Sync. The first
  Sync fetches every listed chat, including chats already stored from a ZIP
  import.
- One chat about every 3 seconds. A 2,000-chat first Sync takes about 100
  minutes.
- Any chat with nothing to import yet is skipped until its `update_time`
  changes.

How Sync updates stored chats:

- New turns, completed tool results, and missing models are added in place.
  Message IDs and pins stay intact, and no Trash copy is made.
- An edited, regenerated, or shorter chat replaces the stored copy with
  ChatGPT's visible branch. Under every archive policy, Sync moves the previous
  version to Trash with its pins, notes, and archived images. Syncing the same
  change again creates no additional copy.
- Your messages now store their ChatGPT message and parent IDs, while ChatGPT's
  replies do not. Sync fills these IDs and missing models into chats imported
  earlier; ZIP re-imports do not.
- ChatGPT does not signal when a reply is still being written. A chat synced
  mid-reply keeps that partial reply until its `update_time` changes. Sync
  accepts a difference of up to 5 seconds between the list and conversation
  timestamps, which differ slightly for the same chat.

Images appear as `[image unavailable]`. Sync keeps an uploaded image from a ZIP
import when the user turn's position, role, timestamp, and surrounding text
match, and when its saved message and parent IDs match if the archive has them.
Images generated by ChatGPT also become `[image unavailable]`, even when
nothing changed. The first Sync of a chat with a ZIP-imported generated image
therefore replaces the chat, and the Trash copy keeps the image.

When Sync stops:

- Each chat has a 32 MiB response limit. Larger chats count as failed while
  Sync continues.
- Two failed chats in a row end Sync. Oversized chats do not count toward this
  limit.
- A rate limit that outlasts the retries, or a requested wait longer than 60
  seconds, ends Sync early. Run Sync again later to continue.
- A chat in a format this version cannot read counts as a failed chat, so two
  in a row end Sync. Either way, Sync reports that AgentsView needs an update.
- An expired sign-in ends Sync. Sign in to ChatGPT in Chrome and run Sync
  again.

Chats imported before Sync stopped stay imported. ChatGPT's private API can
change without notice.

## Sync Gemini Apps chats

Gemini Sync runs from the command line. The **Import conversations** dialog
does not offer Gemini Sync or Gemini Takeout imports; to import Takeout files,
use the [CLI](#importing-via-the-cli).

1. Set up the Chrome extension by following
   [Sync Claude.ai chats](#sync-claudeai-chats). If you set it up before this
   release, rerun `agentsview source chrome setup`, reload the extension at
   `chrome://extensions`, and approve its new access to `gemini.google.com`.
2. Sign in to Gemini in the Chrome profile that has the extension enabled.
3. With AgentsView and Chrome running, run:

   ```bash
   agentsview source import --type gemini-apps --sync
   ```

Sync reuses an open Gemini tab. If none is open, it opens one in the background
and leaves it open. If the open tab belongs to a second Google account
(`/u/1/`), Sync reads that account.

Gemini has no public API. The extension makes the same two read requests as the
Gemini web app: the chat list and one chat's turns. It reads the page's request
token inside the Gemini tab and never sends it to AgentsView; only the
responses reach the local server.

What Sync imports:

- Each chat becomes one `gemini-apps:<chat id>` session with every visible
  turn. When Gemini keeps several drafts of a reply, Sync stores the draft
  Gemini shows.
- Prompt and reply text only. Attachments, images, and Canvas content are not
  imported.
- Takeout imports create separate one-prompt sessions, so a chat imported both
  ways appears twice.
- Pinned chats were not available to check. If a pinned chat is missing after
  Sync, unpin it and run Sync again.

What Sync fetches:

- Only chats whose list update time differs from the last Sync, or whose stored
  copy changed since then. The first Sync fetches every listed chat.
- One turn page about every 3 seconds. Each page holds up to 20 turns, so a
  long chat takes several requests. Chat list pages are not paced; Sync
  requests the next one as soon as it finishes the previous one.
- If a chat's newest reply has no text yet, such as while Gemini is still
  writing it, Sync stores the chat as it is and fetches it again on the next
  Sync. This stops once the reply has text, or once the chat's list update time
  is more than 24 hours old, so a reply that never finishes is not fetched
  forever.
- A chat whose list update time cannot be read is still fetched. If its newest
  reply has no text, Sync fetches it again on every Sync until the reply has
  text. Otherwise it is fetched once and then treated as unchanged.

How Sync updates stored chats:

- New turns and replies that were cut off are completed in place. Message IDs
  and pins stay intact, and no Trash copy is made.
- A regenerated reply, another selected draft, or a shorter chat replaces the
  stored copy with what Gemini shows now. Sync moves the previous version to
  Trash with its pins and notes.

When Sync stops:

- Each chat has a 32 MiB limit across all its turn pages. Larger chats count as
  failed while Sync continues.
- Two failed chats in a row end Sync. Oversized chats and chats Gemini refuses
  to return count as failures, but do not count toward this limit.
- A rate limit that outlasts the retries, or a requested wait longer than 60
  seconds, ends Sync early. Run Sync again later to continue.
- A chat in a format this version cannot read counts as a failed chat, so two
  in a row end Sync. Either way, Sync reports that AgentsView needs an update.
  A chat list in an unreadable format ends Sync with the same message.
- An expired sign-in ends Sync. Sign in to Gemini in Chrome and run Sync again.

Chats imported before Sync stopped stay imported. Gemini's request names and
response layout can change with any web app release.

## Sync Grok web chats

Grok web Sync imports your chats from grok.com from the command line; the
**Import conversations** dialog does not offer it. These sessions use the
`grok-web` type, separate from local Grok Build sessions (`grok`). AgentsView
imports Grok web chats only through Sync and does not read the data download
that Grok offers in its settings.

1. Set up the Chrome extension by following
   [Sync Claude.ai chats](#sync-claudeai-chats). If you set it up before this
   release, rerun `agentsview source chrome setup`, reload the extension at
   `chrome://extensions`, and approve its new access to `grok.com`.
2. Sign in to Grok in the Chrome profile that has the extension enabled.
3. With AgentsView and Chrome running, run:

   ```bash
   agentsview source import --type grok-web --sync
   ```

Sync reuses an open Grok tab. If none is open, it opens one in the background
and leaves it open.

The extension makes the same two read requests as the Grok web app: the chat
list and one chat's responses. It sends them with the tab's sign-in cookies,
which stay in Chrome; only the responses reach the local server.

What Sync imports:

- Each chat becomes one `grok-web:<chat id>` session in the **grok.com**
  project.
- Grok keeps every regenerated reply and edited prompt as a branch. Sync stores
  the branch Grok shows, which ends at the newest response with text, and
  records each reply's model.
- Prompt and reply text only. Older branches, reasoning steps, attachments,
  images, search results, and workspaces are not imported.

What Sync fetches:

- Only chats whose list update time differs from the last Sync, or whose stored
  copy changed since then. The first Sync fetches every listed chat.
- One chat about every 3 seconds; its responses arrive in one request. Chat
  list pages are not paced.
- If Grok is still writing a reply, Sync stores the chat as it is and fetches
  it again on the next Sync. This stops once the reply is complete, or once the
  chat's list update time is more than 24 hours old, so a reply that never
  finishes is not fetched forever.
- A chat whose list update time is missing or cannot be read is still fetched.
  If Grok is still writing its reply, Sync fetches it again on every Sync until
  the reply is complete. Otherwise it is fetched once and then treated as
  unchanged.

How Sync updates stored chats:

- New turns and replies that were cut off are completed in place. Message IDs
  and pins stay intact, and no Trash copy is made.
- When the shown branch changes, for example after you regenerate a reply or
  edit a prompt, Sync replaces the stored chat with what Grok shows now and
  moves the previous version to Trash with its pins and notes.

When Sync stops:

- Each chat has a 32 MiB limit. Larger chats count as failed while Sync
  continues.
- Two failed chats in a row end Sync. Oversized chats count as failures, but do
  not count toward this limit.
- A rate limit that outlasts the retries, or a requested wait longer than 60
  seconds, ends Sync early. Run Sync again later to continue.
- A chat in a format this version cannot read counts as a failed chat, so two
  in a row end Sync. Either way, Sync reports that AgentsView needs an update.
  A chat list in an unreadable format ends Sync with the same message.
- An expired sign-in (HTTP 401 or 403) ends Sync. Sign in to Grok in Chrome and
  run Sync again.

Chats imported before Sync stopped stay imported. Grok's requests and response
fields can change with any web app release.

## Importing via the CLI

To import from the command line, run `agentsview source import`:

```bash
agentsview source import --type claude-ai ~/Downloads/claude-export.zip
agentsview source import --type chatgpt ~/Downloads/chatgpt-export.zip
agentsview source import --type gemini-apps ~/Downloads/takeout.zip
```

| Flag | Description |
|------|-------------|
| `--type` | `claude-ai`, `chatgpt`, or `gemini-apps`; with `--sync`, also `grok-web` (required) |
| `--sync` | Sync through Chrome instead of importing a file. Needs a running server and the Chrome extension; takes no path or `--replace`. |
| `--replace` | Session ID whose archived messages the import may replace when the normal import refuses them; repeat for more sessions (`claude-ai` and `chatgpt` only). See [Replacing archived history](#replacing-archived-history). |

The input path can be a `.zip` file, a `conversations.json` file (Claude.ai
only), a Gemini Apps `MyActivity.html` file, or a directory containing the
extracted export.

## What gets imported

### Messages

AgentsView imports conversation turns as sessions. When a provider emits
thinking or reasoning blocks or tool usage, those message types are preserved.
Each Gemini Apps `Prompted` record becomes one user message containing its
complete visible plain text. HTML presentation does not infer speaker roles,
and inline code and preformatted text remain text without generated Markdown.

### ChatGPT images

ChatGPT exports include both DALL-E generations and files you uploaded during
conversations. AgentsView extracts these images from the `.zip` file, stores
them locally in the data assets directory, and displays them inline in the
message viewer as they appeared in the original conversation.

Supported formats: PNG, JPG, JPEG, WebP, and GIF.

### Metadata

Each imported session includes:

- The conversation title as the session display name.
- Created and updated timestamps.
- Message and user message counts.
- Model information, when available.

## How imported sessions appear

Imported conversations appear in the session list alongside your locally
tracked agent sessions. They are grouped under the **claude.ai**,
**chatgpt.com**, **gemini.google.com**, or **grok.com** project, so you can
filter to them with the project filter or browse them mixed in with your other
sessions.

Imported sessions support the same features as any other session: search,
export, publish to Gist, insights, pinned messages, and analytics.

## Re-import an export file

You can safely re-import the same export file:

- **Claude.ai.** Existing sessions are updated with any new messages, and
  user-edited display names are preserved. An export with fewer messages than
  the archived session, such as an older export, is reported as an error and
  leaves the stored session unchanged unless you explicitly replace it with
  `--replace`. Equal-length or longer exports can refresh earlier messages,
  including attachment text; earlier turns do not have to match the archive.
- **ChatGPT.** Unchanged sessions are skipped. An export can add messages when
  every archived message still matches the beginning of the export, comparing
  message text, each archived tool call's name and category, and whether its
  result is empty. If an archived message's text is the start of the export's
  text for the same turn, such as a copy cut short by an earlier conversion,
  AgentsView updates it with the export's full text in place and keeps its
  position and any pin. Empty archived text never counts as a cut copy, and
  extra tool calls in the export are ignored. If a tool result was empty when
  archived and is filled in the export, AgentsView writes it into the archived
  message while keeping its position and any pin. For example, if you export
  during a code run, keep chatting, and export again, the re-import adds the
  run's output and the new turns. Other result differences keep the archived
  result. Shorter exports and other changes are reported as errors and leave
  the archive unchanged unless you explicitly replace the session with
  `--replace`. Trashed conversations are skipped, and user display names are
  preserved.
- **Gemini Apps.** Existing sessions are matched by the canonical UTC timestamp
  and its zero-based occurrence among records sharing that timestamp. Inserting
  or reordering records with other timestamps does not change existing IDs.
  Content changes update the same one-message session, and unchanged records
  are skipped.

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

Streamed progress events carry only the counts, while the CLI summary shows the
same reasons next to its error count. Gemini Apps records that the parser
rejects before they have a session ID are counted in `errors` without an entry.

## Replacing archived history

Use replace mode when a session's archived copy is wrong and every re-import
refuses it, for example because messages were cut short or the export has a
different set of messages than the archive. List each session to replace by its
ID, `chatgpt:<conversation id>` or `claude-ai:<conversation uuid>`:

```bash
agentsview source import --type chatgpt --replace 'chatgpt:<conversation-id>' ~/Downloads/chatgpt-export.zip
```

Repeat `--replace` for each session you want to replace. The import API accepts
the same list as a repeatable query parameter:

```text
POST /api/v1/import/chatgpt?replace=<session-id>&replace=<session-id>
POST /api/v1/import/claude-ai?replace=<session-id>
```

What happens to a listed session:

- AgentsView replaces it only when the normal import would refuse it. An export
  that adds messages or fills empty tool results still updates it in place.
- Its messages become the export's exact messages. The session keeps its name,
  and pins stay on messages that still match.
- The previous version moves to Trash as `<session-id>:replaced:<time>` with
  its messages, name, and pins. Restore it from Trash if you need it back.
- Importing the same export again changes nothing and adds no second copy.
- A trashed session stays trashed and is not replaced. A session belonging to a
  different agent is refused as before.

Sessions you do not list are imported as usual. Gemini Apps imports do not
support `--replace`.
