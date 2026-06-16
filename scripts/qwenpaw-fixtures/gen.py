#!/usr/bin/env python3
"""Regenerate the agentsview QwenPaw fixtures.

This script hand-authors synthetic QwenPaw session payloads that
match the on-disk shape documented in
``QwenPaw/src/qwenpaw/app/runner/session.py``. The output is written
as pretty-printed JSON (``indent=2``) for readability — it is NOT
byte-identical to what a live QwenPaw runtime writes (which uses
``json.dumps(..., ensure_ascii=False)`` without indent). The parser
treats whitespace-insensitive JSON, so test fixtures do not need
byte-exact fidelity; they only need the right shape.

Requirements
------------
- The QwenPaw source tree checked out locally. The script reads
  ``src/qwenpaw/app/runner/session.py`` from that tree and inspects
  it with the ``ast`` module purely to sanity-check the source path
  (expected symbols: ``SessionManager``, ``save_session_state``,
  ``_atomic_write_json``). It never imports the ``qwenpaw`` package,
  so a malicious or compromised QwenPaw checkout cannot execute
  arbitrary top-level code during fixture regeneration. No QwenPaw
  runtime is needed.

Usage
-----
    python3 scripts/qwenpaw-fixtures/gen.py \\
        --qwenpaw-src ~/develop/QwenPaw \\
        --out internal/parser/testdata

The qwenpaw/ subtree under --out is replaced wholesale to avoid
stale fixtures. All synthesized data is non-real — no PII,
no live tokens, no real user identifiers.

Serializer reference
--------------------
Source of truth for the on-disk shape:
    QwenPaw/src/qwenpaw/app/runner/session.py
        SessionManager.save_session_state  (line ~314)
        SessionManager._get_save_path      (line ~258)
        _atomic_write_json                 (module-level helper)
"""

from __future__ import annotations

import argparse
import ast
import json
import shutil
from pathlib import Path
from typing import Any


def _parse_args() -> argparse.Namespace:
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument(
        "--qwenpaw-src",
        required=True,
        type=Path,
        help="Path to a checked-out QwenPaw source tree.",
    )
    p.add_argument(
        "--out",
        required=True,
        type=Path,
        help="Output directory (the qwenpaw/ subtree is rebuilt here).",
    )
    return p.parse_args()


# _verify_source_shape sanity-checks the QwenPaw source tree by reading
# session.py and walking its AST — it deliberately does NOT import the
# package, so a compromised QwenPaw checkout cannot run arbitrary code
# during fixture regeneration. Returns True iff session.py defines a
# SessionManager class with a save_session_state method AND a module-
# level _atomic_write_json function. Rejecting "right symbol, wrong
# owner" avoids a degenerate file that defines an unrelated
# SessionManager alongside an unrelated save_session_state free function.
def _verify_source_shape(qwenpaw_src: Path) -> bool:
    session_py = (
        qwenpaw_src / "src" / "qwenpaw" / "app" / "runner" / "session.py"
    )
    if not session_py.is_file():
        return False
    try:
        tree = ast.parse(session_py.read_text(encoding="utf-8"))
    except (OSError, SyntaxError):
        return False

    module_funcs = {
        node.name for node in tree.body
        if isinstance(node, ast.FunctionDef)
    }
    session_manager_methods: set[str] = set()
    for node in tree.body:
        if (
            isinstance(node, ast.ClassDef)
            and node.name == "SessionManager"
        ):
            session_manager_methods = {
                child.name for child in node.body
                if isinstance(child, (ast.FunctionDef, ast.AsyncFunctionDef))
            }
            break
    return (
        "_atomic_write_json" in module_funcs
        and "save_session_state" in session_manager_methods
    )


def _msg(
    mid: str, role: str, name: str, content: list[dict], ts: str,
    metadata: dict | None = None,
) -> dict[str, Any]:
    return {
        "id": mid,
        "name": name,
        "role": role,
        "content": content,
        "metadata": metadata or {},
        "timestamp": ts,
    }


def _pair(m: dict) -> list:
    return [m, []]


def _wrap(messages: list[dict], agent_name: str) -> dict:
    return {
        "agent": {
            "memory": {"content": [_pair(m) for m in messages]},
            "toolkit": {"active_groups": []},
            "name": agent_name,
            "_sys_prompt": "redacted",
        }
    }


def _write(path: Path, payload: dict) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(
        json.dumps(payload, ensure_ascii=False, indent=2) + "\n",
        encoding="utf-8",
    )


def _build_default_basic(out_root: Path) -> None:
    msgs = [
        _msg(
            "msg_0001", "user", "user",
            [{"type": "text", "text": "QwenPaw 的默认会话布局长什么样？"}],
            "2026-03-15 10:00:00.000",
        ),
        _msg(
            "tID1AAAAA", "assistant", "Friday",
            [
                {"type": "thinking", "thinking": "User is asking about the session file format. Let me show them the on-disk shape."},
                {"type": "text", "text": "默认会话写在 <workspace>/sessions/<stem>.json，顶层是 {\"agent\":{\"memory\":{\"content\":[[msg, []], ...]}}}。"},
            ],
            "2026-03-15 10:00:02.500",
        ),
    ]
    _write(
        out_root / "default" / "sessions" / "default_1700000000000.json",
        _wrap(msgs, "Friday"),
    )


def _build_default_tool_use(out_root: Path) -> None:
    msgs = [
        _msg(
            "msg_0010", "user", "user",
            [{"type": "text", "text": "读一下 NOTES.md 的内容"}],
            "2026-03-16 09:00:00.000",
        ),
        _msg(
            "aID0BBBBB", "assistant", "Friday",
            [{
                "type": "tool_use",
                "id": "call_aaaaaaaaaaaa",
                "name": "read_file",
                "input": {"file_path": "NOTES.md"},
                "raw_input": "{\"file_path\":\"NOTES.md\"}",
            }],
            "2026-03-16 09:00:01.250",
        ),
        _msg(
            "sID0CCCCC", "system", "system",
            [{
                "type": "tool_result",
                "id": "call_aaaaaaaaaaaa",
                "name": "read_file",
                "output": [{
                    "type": "text",
                    "text": "# NOTES\n\n- synthetic fixture for agentsview tests\n- no real user data",
                }],
            }],
            "2026-03-16 09:00:01.500",
        ),
        _msg(
            "aID1DDDDD", "assistant", "Friday",
            [{"type": "text", "text": "NOTES.md 是一个合成 fixture，无真实用户数据。"}],
            "2026-03-16 09:00:02.000",
        ),
    ]
    _write(
        out_root / "default" / "sessions" / "main_main.json",
        _wrap(msgs, "Friday"),
    )


def _build_default_console(out_root: Path) -> None:
    msgs = [
        _msg(
            "msg_console_0001", "user", "user",
            [{"type": "text", "text": "这是从 console 启动的会话"}],
            "2026-03-17 14:30:00.000",
        ),
        _msg(
            "aID0ConAAAA", "assistant", "Default",
            [{"type": "text", "text": "Console 会话写入 sessions/console/<stem>.json，与根布局同名文件互不冲突。"}],
            "2026-03-17 14:30:01.000",
        ),
    ]
    _write(
        out_root / "default" / "sessions" / "console" / "default_1700000000001.json",
        _wrap(msgs, "Default"),
    )


def _build_note_keeper_channel_scoped(out_root: Path) -> None:
    msgs = [
        _msg(
            "msg_chan_0001", "user", "user",
            [{"type": "text", "text": "Channel-scoped 会话：文件名形如 <user_id>_<session_id>.json，可包含 @ 与点号。"}],
            "2026-03-18 08:15:30.123",
            metadata={"source": "im_wechat"},
        ),
        _msg(
            "aID0ChanAAA", "assistant", "Keeper",
            [{"type": "text", "text": "记下来了。这种文件名 challenge agentsview 的 ID 校验，需要放宽到 IsValidQwenPawIDPart。"}],
            "2026-03-18 08:15:31.456",
        ),
    ]
    _write(
        out_root / "note_keeper" / "sessions" / "user@example.com_1700000000002.json",
        _wrap(msgs, "Keeper"),
    )


def _build_researcher_empty(out_root: Path) -> None:
    _write(
        out_root / "researcher" / "sessions" / "empty.json",
        _wrap([], "Researcher"),
    )


def _reject_symlink(path: Path) -> None:
    """Abort if path is a symlink, so rmtree cannot escape the tree."""
    if path.is_symlink():
        raise SystemExit(
            f"error: refusing to operate on symlink {path}; the fixture "
            "output path and its qwenpaw/ subtree must be real directories"
        )


def main() -> int:
    args = _parse_args()
    if not _verify_source_shape(args.qwenpaw_src):
        raise SystemExit(
            f"error: {args.qwenpaw_src} does not look like a QwenPaw "
            "source tree (missing src/qwenpaw/app/runner/session.py "
            "with SessionManager.save_session_state and "
            "_atomic_write_json)"
        )
    # Do NOT resolve() the deletion target: resolve() follows symlinks,
    # so a `testdata/qwenpaw` symlink would make rmtree delete whatever
    # it points at. Operate on the literal path and refuse symlinks for
    # the output dir and the qwenpaw/ subtree before removing anything.
    out_root = args.out / "qwenpaw"
    _reject_symlink(args.out)
    _reject_symlink(out_root)
    if out_root.exists():
        shutil.rmtree(out_root)
    out_root.mkdir(parents=True)

    _build_default_basic(out_root)
    _build_default_tool_use(out_root)
    _build_default_console(out_root)
    _build_note_keeper_channel_scoped(out_root)
    _build_researcher_empty(out_root)

    print(f"Wrote QwenPaw fixtures under {out_root}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
