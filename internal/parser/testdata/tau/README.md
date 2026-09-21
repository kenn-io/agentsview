# Tau fixture

`issue-session.jsonl` is a sanitized derivative of the Tau session attached to
issue [#1634](https://github.com/kenn-io/agentsview/issues/1634).

The captured source files came from these public attachments:

- [index.jsonl](https://github.com/user-attachments/files/31858413/index.jsonl.json),
  SHA256 `069e87052a6fb448f2588669ec6cd31c2456e7a8f7bdee15083d9308197cd3d1`
- [session JSONL](https://github.com/user-attachments/files/31858414/da3907f3c98e4a8d90c03ae42870920c.jsonl.json),
  SHA256 `f0d95655c08002655c7e727afe7249bebd8077c58ec017d76a703d785dddb3bd`

The fixture keeps the captured record count, IDs, parent links, timestamps,
models, tool IDs, roles, and token values. It replaces local paths and names
with example values. The producer source was pinned to
[`93bfc761b43e0a5a646b0e5ac808b3a15918e74d`](https://github.com/huggingface/tau/tree/93bfc761b43e0a5a646b0e5ac808b3a15918e74d).

Branch, compaction, and cache-write cases in the parser tests are constructed
from that source revision. They are source-derived fixtures, not live captures.
