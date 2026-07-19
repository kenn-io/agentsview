use serde::{de::DeserializeOwned, Serialize};
use std::{env, fs, path::Path};
use xai_grok_shell::{
    sampling::ConversationItem,
    session::{persistence::Summary, signals::SessionSignals},
};

const CURRENT_ID: &str = "019f6000-0000-7000-8000-000000000001";
const LEGACY_ID: &str = "019f6000-0000-7000-8000-000000000002";

fn normalize_json<T: DeserializeOwned + Serialize>(raw: &str) -> Vec<u8> {
    let value: T = serde_json::from_str(raw).expect("fixture must match upstream type");
    let mut bytes = serde_json::to_vec_pretty(&value).expect("fixture must serialize");
    bytes.push(b'\n');
    bytes
}

fn write_summary(path: &Path, id: &str, cwd: &str, version: u8) {
    let raw = format!(
        r#"{{
      "info": {{"id": "{id}", "cwd": "{cwd}"}},
      "session_summary": "Inspect parser compatibility",
      "generated_title": "Audit Grok compatibility",
      "created_at": "2026-07-18T10:00:00Z",
      "updated_at": "2026-07-18T10:30:00Z",
      "last_active_at": "2026-07-18T10:29:00Z",
      "num_messages": 12,
      "num_chat_messages": 8,
      "current_model_id": "grok-4.5",
      "chat_format_version": {version},
      "parent_session_id": "019f5000-0000-7000-8000-000000000000",
      "source_workspace_dir": "/workspace/agentsview",
      "git_root_dir": "/workspace/agentsview",
      "head_branch": "feature/parser-audit",
      "agent_name": "grok-build"
    }}"#
    );
    fs::write(
        path.join("summary.json"),
        normalize_json::<Summary>(&raw),
    )
    .unwrap();
}

fn write_signals(path: &Path) {
    let raw = r#"{
      "turnCount": 2,
      "userMessageCount": 2,
      "assistantMessageCount": 2,
      "contextTokensUsed": 12000,
      "contextWindowTokens": 200000,
      "primaryModelId": "grok-4.5"
    }"#;
    fs::write(
        path.join("signals.json"),
        normalize_json::<SessionSignals>(raw),
    )
    .unwrap();
}

fn write_v1(path: &Path) {
    let rows = [
        r#"{"type":"system","content":"You are Grok"}"#,
        r#"{"type":"user","content":[{"type":"text","text":"project policy"}],"synthetic_reason":"project_instructions"}"#,
        r#"{"type":"user","content":[{"type":"text","text":"Review parser compatibility"}],"prompt_index":0}"#,
        r#"{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"Inspect both formats"}]}"#,
        r#"{"type":"backend_tool_call","kind":{"tool_type":"web_search","id":"ws_1","status":"completed","action":{"type":"search","query":"Grok Build persistence","sources":[]}}}"#,
        r#"{"type":"assistant","content":"Reading the implementation.","tool_calls":[{"id":"call_1","name":"read_file","arguments":"{\"target_file\":\"src/session/persistence.rs\"}"}],"model_id":"grok-4.5"}"#,
        r#"{"type":"tool_result","tool_call_id":"call_1","content":"source body"}"#,
        r#"{"type":"user","content":[{"type":"text","text":"also keep interjections"}],"synthetic_reason":"interjection"}"#,
        r#"{"type":"assistant","content":"Compatibility checked.","model_id":"grok-4.5"}"#,
    ];
    let mut out = Vec::new();
    for row in rows {
        let value: ConversationItem = serde_json::from_str(row).unwrap();
        serde_json::to_writer(&mut out, &value).unwrap();
        out.push(b'\n');
    }
    fs::write(path.join("chat_history.jsonl"), out).unwrap();
}

fn write_legacy_source(path: &Path) {
    let rows = [
        r#"{"type":"system","content":"You are Grok"}"#,
        r#"{"type":"user","content":[{"type":"text","text":"Review parser compatibility"}],"prompt_index":0}"#,
        r#"{"type":"reasoning","id":"rs_v0","summary":[{"type":"summary_text","text":"Check the old format"}]}"#,
        r#"{"type":"assistant","content":"Reading the implementation.","tool_calls":[{"id":"call_1","name":"read_file","arguments":"{\"target_file\":\"src/session/persistence.rs\"}"}],"model_id":"grok-4.5"}"#,
        r#"{"type":"tool_result","tool_call_id":"call_1","content":"source body"}"#,
        r#"{"type":"assistant","content":"Compatibility checked.","model_id":"grok-4.5"}"#,
    ];
    let mut out = Vec::new();
    for row in rows {
        let value: ConversationItem = serde_json::from_str(row).unwrap();
        serde_json::to_writer(&mut out, &value).unwrap();
        out.push(b'\n');
    }
    fs::write(path.join("source-v1.jsonl"), out).unwrap();
}

fn main() {
    let output = env::args()
        .nth(1)
        .expect("usage: agentsview_golden OUTPUT");
    let root = Path::new(&output);
    let current = root
        .join("current")
        .join("%2Fworkspace%2Fgrok-worktrees%2Fparser-audit")
        .join(CURRENT_ID);
    let legacy = root
        .join("legacy")
        .join("%2Fworkspace%2Fagentsview")
        .join(LEGACY_ID);
    fs::create_dir_all(&current).unwrap();
    fs::create_dir_all(&legacy).unwrap();
    write_summary(
        &current,
        CURRENT_ID,
        "/workspace/grok-worktrees/parser-audit",
        1,
    );
    write_summary(&legacy, LEGACY_ID, "/workspace/agentsview", 0);
    write_signals(&current);
    write_signals(&legacy);
    write_v1(&current);
    write_legacy_source(&legacy);
}
