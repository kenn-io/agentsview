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
  new messages. User-edited display names are preserved.
- **ChatGPT** — existing sessions are skipped (not
  re-imported), so your data stays unchanged.
- **Gemini Apps** — existing sessions are matched by the canonical UTC
  timestamp and its zero-based occurrence among records sharing that
  timestamp. Inserting or reordering records with other timestamps doesn't
  change existing IDs. Content changes update the same one-message session;
  unchanged records are skipped.
