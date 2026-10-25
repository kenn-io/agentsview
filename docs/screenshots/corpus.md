# Prepare a screenshot corpus

A corpus is a reviewed directory of native session files and a checksummed
`manifest.json`. The capture worker syncs these files with the selected
AgentsView version. Keep generations and their original intake files in private
managed storage, outside source and generated-asset branches.

## Create a generation

Run generation on the managed worker, inside the retained builder image
described in [the worker guide](README.md#prepare-the-worker-inputs). This pins
the Python interpreter and timezone database. Retain the image digest with the
recipe. Retain a frozen selection of native files and a JSON recipe. Record each
original file's SHA-256 before generation. Include the native directory
structure and companion files needed by the parsers, including subagent
transcripts and persisted tool outputs.

The recipe has this structure. Replace example checksums and revision with the
real values; the examples are not a runnable corpus.

```json
{
  "schema": 1,
  "generation": "example-1",
  "generator_revision": "<full capture-tool commit>",
  "reference_now": "2026-10-20T12:00:00Z",
  "timezone": "America/Chicago",
  "source_roots": {
    "claude": ["claude/projects"],
    "codex": ["codex/sessions"]
  },
  "targets": {
    "rich": {"session_id": "session-a"},
    "thinking": {"session_id": "session-a"},
    "code_block": {"session_id": "session-b"},
    "tool_output": {"session_id": "session-a", "message_ordinal": 3}
  },
  "replacements": {
    "/workspace/original-project": "/workspace/project-a",
    "original-session": "session-a"
  },
  "blocked_terms": ["original-project", "original-session"],
  "omit_fields": ["encrypted_content"],
  "files": [
    {
      "source": "selected/conversation.jsonl",
      "path": "claude/projects/project-a/session-a.jsonl",
      "format": "jsonl",
      "sha256": "<SHA-256 of the original file>"
    }
  ],
  "fixtures": {
    "starred_session_ids": ["session-a"],
    "insights": []
  }
}
```

Paths are relative to the intake root or generation root. Supported file formats
are `jsonl`, `json`, and UTF-8 `text`. JSON replacements apply to decoded
strings and object keys, including tool inputs and nested results. The longest
matching replacement wins, without cascading replacements. Remaining blocked
terms are rejected case-insensitively. Source checksums, malformed JSON,
colliding keys, and symlinks are checked before publishing a generation.

`omit_fields` removes the named JSON fields recursively. Codex encrypted
reasoning cannot be reviewed or sanitized as text; generation rejects it unless
the recipe explicitly omits `encrypted_content`. Readable reasoning summaries
remain. Select sessions without unreviewed inline images or other opaque
attachments.

Omission is by field name, including any plaintext stored under that name. Codex
can also use `encrypted_content` for readable agent messages. Review this loss
when selecting examples; the generator does not infer which values to keep.

```bash
docker run --rm --pull=never --network=none --platform=linux/amd64 \
  --user "$(id -u):$(id -g)" \
  --mount type=bind,source=/managed/source/agentsview/docs/screenshots,target=/capture,readonly \
  --mount type=bind,source=/managed/intake,target=/intake,readonly \
  --mount type=bind,source=/managed/corpora,target=/corpora \
  --entrypoint python "$BUILDER_IMAGE" /capture/corpus.py generate \
  --recipe /intake/recipe.json \
  --source /intake/files \
  --output /corpora/example-1
```

The output must not exist. A successful command prints the manifest SHA-256. The
manifest records the sanitized files, recipe digest, generator source hash,
reference time, roots, and scenario targets. Replacement mappings and original
paths stay in the private recipe.

Run the same recipe into a second new directory and compare the printed digest.
An unchanged recipe, intake, and generator must produce the same files and
manifest. File modification times and the worker's clock are not corpus inputs.

## Review and reuse

Review the selected conversations, metadata, and attachments. String replacement
does not identify every private detail. Approve a generation only after
reviewing its content and the screenshots it produces. Keep a second durable
copy of its intake, recipe, tools, and approved output.

```bash
python3 docs/screenshots/corpus.py verify /managed/corpora/example-1/manifest.json
```

Verification checks every file and rejects missing, changed, or unlisted files.
The capture request must supply the expected manifest hash separately.

The current capture cases require Claude and Codex sessions, subagent content,
thinking, a fenced code block, and a tool result at the selected ordinal. The
fixture check validates targets against the database created by normal sync.
`fixtures.insights` entries contain `type`, `date_from`, `date_to`, `agent`, and
`content`, with optional `project` and `model`. Their creation time comes from
`reference_now`. Add examples needed by the visible capture cases deliberately.

The browser uses the recorded clock and timezone. Activity captures select the
day one week before the reference time and its completed calendar week. Choose a
reference time with activity in those windows. Changing the reference time,
selection, policy, or annotations creates a new generation; keep earlier
generations for earlier documentation releases.
