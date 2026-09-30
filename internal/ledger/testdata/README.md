---
last_edited: 2026-09-24
---

# Ledger fixtures

Every file in this directory except `README.md` and `rustgen/` is written by
`rustgen`, a small Rust program that links jilog's own `ledger-core` crate at
commit `9e8e094` (MIT, Copyright (c) 2026 Joichi Ito) and uses jilog's
`Cargo.lock`, which pins serde_json 1.0.149, zmij 1.0.21, chrono 0.4.44,
uuid 1.23.1 and crc32fast 1.5.0. Go never writes these bytes; the tests only
read them.

| File | Contents |
| --- | --- |
| `segments/*.json` | Segments built with fixed IDs and times, sealed with `Segment::seal`, written with `Segment::write_to_file` |
| `checksums.txt` | `<file> <crc> <event count>` for each segment |
| `fixture-a-000001.events.json` | `serde_json::to_vec(&segment.events)`: the exact CRC input |
| `query-json.golden`, `query-text.golden`, `query-text-empty.golden` | `jilog query` output for two zones, produced by the `format!`/`json!` statements of `crates/jilog/src/commands/query.rs:309-361`, copied verbatim (the originals are private to the jilog binary) |
| `uuid-v5.txt` | The v5 namespace and two deterministic IDs from the Rust `uuid` crate |
| `serde-f64.txt` | 2000 `f64` values as `<bits hex> <serde_json output>` |
| `jilog-verify.txt` | jilog's own `read_from_file` + `verify()` result for `fixture-lossy-000001.json` |

To regenerate (needs git and cargo; nothing is written outside a temp dir
except this directory):

```bash
out="$PWD/internal/ledger/testdata"
tmp="$(mktemp -d)"
cp -R internal/ledger/testdata/rustgen/. "$tmp"
git clone https://github.com/Joi/jilog "$tmp/jilog"
git -C "$tmp/jilog" checkout 9e8e094
cp "$tmp/jilog/Cargo.lock" "$tmp/Cargo.lock"
(cd "$tmp" && cargo run --quiet -- "$out")
rm -rf "$tmp"
git status --short internal/ledger/testdata
```

A regeneration against the same commit must leave `git status` clean.
