# Grok Build parser goldens

These sanitized sessions pin the persisted formats emitted by
[`xai-org/grok-build`](https://github.com/xai-org/grok-build) at commit
`7cfcb20d2b50b0d18801a6c0af2e401c0e060894`.

`generate.rs` is copied to
`crates/codegen/xai-grok-shell/examples/agentsview_golden.rs` in a disposable
checkout of that commit. It parses and reserializes metadata and current history
rows through Grok Build's own Rust types. The legacy history is then produced by
Grok Build's `chat-history-downgrade` binary.

From the AgentsView repository root, regenerate the fixtures with:

```sh
GROK_BUILD_CHECKOUT=/tmp/grok-build-upstream-20260719
cp internal/parser/testdata/grok-build/generate.rs \
  "$GROK_BUILD_CHECKOUT/crates/codegen/xai-grok-shell/examples/agentsview_golden.rs"
cargo run --manifest-path "$GROK_BUILD_CHECKOUT/Cargo.toml" \
  -p xai-grok-shell --example agentsview_golden -- \
  internal/parser/testdata/grok-build
cargo run --manifest-path "$GROK_BUILD_CHECKOUT/Cargo.toml" \
  -p xai-grok-shell --bin chat-history-downgrade -- \
  internal/parser/testdata/grok-build/legacy/%2Fworkspace%2Fagentsview/019f6000-0000-7000-8000-000000000002/source-v1.jsonl \
  internal/parser/testdata/grok-build/legacy/%2Fworkspace%2Fagentsview/019f6000-0000-7000-8000-000000000002/chat_history.jsonl
```

The fixtures use invented workspace paths, session IDs, prompts, and model
metadata. Go-side expectations remain hand-authored so the fixture producer is
not also the test oracle.
