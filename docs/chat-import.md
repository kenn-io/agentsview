---
title: Chat Import
description: Import Claude.ai and ChatGPT conversations into AgentsView
---

AgentsView can import your conversation history from Claude.ai
and ChatGPT. Both services let you export your data as a zip
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

## Importing via the UI

Click the **Import conversations** button in the header
(the upload icon in the top-right area) to open the import
dialog.

![Import button in header](/assets/generated/screenshots/import-button.png)

1. **Select a provider** — choose Claude.ai or ChatGPT
2. **Upload your file**
   - Claude.ai: accepts `conversations.json` or the `.zip`
     from your data export
   - ChatGPT: accepts the `.zip` from your data export
3. Click **Import**

![Import modal — Claude.ai](/assets/generated/screenshots/import-modal-claude.png)

![Import modal — ChatGPT](/assets/generated/screenshots/import-modal-chatgpt.png)

The dialog shows a summary when finished — for example,
"5 conversations processed (4 new, 1 updated)". The session
list refreshes automatically.

## Importing via the CLI

Use `agentsview import` to import from the command line:

```bash
agentsview import --type claude-ai ~/Downloads/claude-export.zip
agentsview import --type chatgpt ~/Downloads/chatgpt-export.zip
```

| Flag | Description |
|------|-------------|
| `--type` | `claude-ai` or `chatgpt` (required) |

The path can be a `.zip` file, a `conversations.json` file
(Claude.ai only), or a directory containing the extracted
export.

## What Gets Imported

### Messages

All conversation turns are imported as sessions: user
messages, assistant responses, thinking/reasoning blocks,
and tool usage (code interpreter, web search, DALL-E).

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
the **claude.ai** or **chatgpt.com** project, so you can
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
