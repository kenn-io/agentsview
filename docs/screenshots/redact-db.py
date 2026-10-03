"""Redact a disposable screenshot archive and preserve native byte ranges."""

import json
import re
import sqlite3
import sys


def redact_archive(path, home):
    rules = []
    if home and home != "/":
        encoded = re.sub(r"[^a-zA-Z0-9-]", "-", home)
        rules = [
            (home.encode("utf-8"), b"~"),
            (encoded.encode("utf-8"), b"-home-user"),
        ]

    def map_boundary(offset, matches, replacement_size):
        delta = 0
        for match in matches:
            if offset < match.start():
                break
            if offset <= match.end():
                return (
                    match.start() + delta
                    + min(offset - match.start(), replacement_size)
                )
            delta += replacement_size - (match.end() - match.start())
        return offset + delta

    fields = {
        "text": "content",
        "thinking": "thinking_text",
        "tool_result": "tool_result_text",
    }
    with sqlite3.connect(path) as connection:
        connection.row_factory = sqlite3.Row
        for row in connection.execute(
            "SELECT id, content, thinking_text, tool_result_text, content_layout FROM messages"
        ):
            bodies = {field: row[field] or "" for field in fields.values()}
            layout = json.loads(row["content_layout"]) if row["content_layout"] else None
            for kind, field in fields.items():
                body = bodies[field].encode("utf-8")
                for original, replacement in rules:
                    matches = list(re.finditer(re.escape(original), body))
                    if layout and layout.get("version") == 1:
                        for block in layout["blocks"]:
                            if block["kind"] == kind:
                                for boundary in ("start", "end"):
                                    block[boundary] = map_boundary(
                                        block[boundary], matches, len(replacement)
                                    )
                    body = body.replace(original, replacement)
                bodies[field] = body.decode("utf-8")
            connection.execute(
                "UPDATE messages SET content=?, thinking_text=?, "
                "tool_result_text=?, content_layout=? WHERE id=?",
                (
                    bodies["content"], bodies["thinking_text"],
                    bodies["tool_result_text"],
                    json.dumps(layout, ensure_ascii=False)
                    if layout else row["content_layout"],
                    row["id"],
                ),
            )

        # Purge full-transcript derived text before this copy enters Docker.
        # The writable archive open rebuilds it with the canonical Go composer.
        if connection.execute(
            "SELECT 1 FROM sqlite_master WHERE name='palette_messages'"
        ).fetchone():
            connection.execute("DELETE FROM palette_messages")
            connection.execute("INSERT INTO palette_fts(palette_fts) VALUES('rebuild')")
            connection.execute(
                "DELETE FROM stats WHERE key IN "
                "('palette_corpus_recipe', 'messages_cjk_fts_fingerprint_v1')"
            )


if __name__ == "__main__":
    redact_archive(sys.argv[1], sys.argv[2])
