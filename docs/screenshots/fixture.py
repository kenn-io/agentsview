#!/usr/bin/env python3
"""Configure and validate disposable screenshot application state."""

import argparse
import json
from pathlib import Path
import sqlite3

from corpus import verify


def configure(manifest, settings, root, destination):
    providers = sorted(provider["id"] for provider in settings["session_providers"])
    roots = manifest["source_roots"]
    if not set(roots) <= set(providers):
        raise ValueError("application does not expose every corpus provider")
    lines = [
        'local_machine_name = "dev-laptop"',
        'tool_result_images = "keep"',
        "disabled_agents = " + json.dumps([agent for agent in providers if agent not in roots]),
    ]
    for agent in providers:
        lines.extend([
            "", f"[agents.{json.dumps(agent)}]",
            "dirs = " + json.dumps([str(root / path) for path in roots.get(agent, [])]),
        ])
    destination.write_text("\n".join(lines) + "\n")


def apply(manifest, database):
    # mode=rw refuses a missing database; only normal sync creates its schema.
    with sqlite3.connect(database.resolve().as_uri() + "?mode=rw", uri=True) as connection:
        connection.execute("PRAGMA foreign_keys = ON")
        targets = manifest["targets"]
        for name, target in targets.items():
            if connection.execute("SELECT 1 FROM sessions WHERE id = ?",
                                  (target["session_id"],)).fetchone() is None:
                raise ValueError(f"missing parsed session for target {name}")
        for agent in manifest["source_roots"]:
            if not connection.execute("SELECT 1 FROM sessions WHERE agent = ?",
                                      (agent,)).fetchone():
                raise ValueError(f"no parsed sessions for provider {agent}")
        checks = [
            ("thinking", "SELECT 1 FROM messages WHERE session_id = ? AND thinking_text != ''",
             (targets["thinking"]["session_id"],)),
            ("code_block", "SELECT 1 FROM messages WHERE session_id = ? AND instr(content, '```') > 0",
             (targets["code_block"]["session_id"],)),
            ("tool_output", """SELECT 1 FROM tool_calls tc
                JOIN messages m ON m.id = tc.message_id
                WHERE tc.session_id = ? AND m.ordinal = ? AND (
                    tc.result_content != '' OR EXISTS (
                        SELECT 1 FROM tool_result_events e WHERE e.session_id = tc.session_id
                        AND e.tool_call_message_ordinal = m.ordinal AND e.call_index = tc.call_index
                        AND e.content != ''))""",
             (targets["tool_output"]["session_id"], targets["tool_output"]["message_ordinal"])),
            ("subagents", "SELECT 1 FROM sessions WHERE parent_session_id IS NOT NULL", ()),
        ]
        for name, query, params in checks:
            if not connection.execute(query, params).fetchone():
                raise ValueError(f"corpus lacks required {name} content")
        now = manifest["reference_now"]
        connection.execute("""UPDATE sessions SET machine = 'dev-laptop',
            created_at = COALESCE(NULLIF(started_at, ''), ?), local_modified_at = ?""", (now, now))
        for session in manifest["fixtures"].get("starred_session_ids", []):
            connection.execute("INSERT INTO starred_sessions(session_id, created_at) VALUES (?, ?)",
                               (session, now))
        for insight in manifest["fixtures"].get("insights", []):
            connection.execute("""INSERT INTO insights
                (type, date_from, date_to, project, agent, model, content, created_at)
                VALUES (?, ?, ?, ?, ?, ?, ?, ?)""",
                               (insight["type"], insight["date_from"], insight["date_to"],
                                insight.get("project"), insight["agent"], insight.get("model"),
                                insight["content"], now))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=["configure", "apply"])
    parser.add_argument("manifest", type=Path)
    parser.add_argument("target", type=Path)
    parser.add_argument("--settings", type=Path)
    args = parser.parse_args()
    manifest = verify(args.manifest)
    if args.command == "configure":
        configure(manifest, json.loads(args.settings.read_text()), args.manifest.parent, args.target)
    else:
        apply(manifest, args.target)


if __name__ == "__main__":
    main()
