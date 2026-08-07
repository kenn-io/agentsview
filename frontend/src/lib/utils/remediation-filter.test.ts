import { describe, expect, it } from "vite-plus/test";
import type { Message } from "../api/types.js";
import {
  countResolvedRemediation,
  isHighConfidenceLocalRemediation,
  shouldHideResolvedRemediation,
} from "./remediation-filter.js";

const examples = [
  "Root cause confirmed: both failures compare raw text containing \\n, but this Windows worktree has CRLF files; the behavior and expected source are unchanged. I’m making the two assertions newline-portable so the required full suite can pass on both Windows and CI.",
  "The database dump finished, but the checkpoint finalization hit a local PowerShell argument bug while reading its count manifest; production was untouched. I’m validating the completed dump, fixing that local-only checkpoint step, then I’ll resume the restore drill.",
  "The drill exposed a normal isolated-container setup issue—the default PostgreSQL public schema existed before restore. I’m changing only the temporary restore command to clean that empty schema, then running a fresh drill directory; no live database object was touched.",
  "The SQL migration itself committed successfully. Its separate history-record insert failed because this psql build does not interpolate variables in -c; I will not rerun the migration. I’m reconciling the committed ledger first, then recording its exact hash with a literal-only metadata insert.",
  "GitNexus’ local graph is currently broken before any code analysis (.gitnexus/csv is incomplete). I’m repairing only that generated index, then I’ll run the required impact checks; no application or service state has changed.",
  "Source Lens 401, and the DataForSEO gate/credential are present without revealing them. The deploy stopped before build because /opt requires root to create the release worktree; I’m creating only the named empty directory with sudo, then continuing.",
];

function commentary(content: string): Message {
  return {
    id: 1,
    session_id: "session",
    ordinal: 1,
    role: "assistant",
    content,
    timestamp: "2026-08-08T00:00:00Z",
    has_thinking: false,
    thinking_text: "",
    has_tool_use: false,
    content_length: content.length,
    model: "",
    context_tokens: 0,
    output_tokens: 0,
    is_system: false,
    source_subtype: "commentary",
  };
}

describe("resolved remediation filter", () => {
  it("recognizes the six confirmed remediation patterns", () => {
    for (const [index, content] of examples.entries()) {
      expect(isHighConfidenceLocalRemediation(content), `example ${index + 1}`).toBe(true);
    }
  });

  it("requires a completed session, enabled filter, and commentary source", () => {
    const message = commentary(examples[0]!);

    expect(shouldHideResolvedRemediation(message, "completed", true)).toBe(true);
    expect(shouldHideResolvedRemediation(message, "abandoned", true)).toBe(false);
    expect(shouldHideResolvedRemediation(message, "completed", false)).toBe(false);
    expect(shouldHideResolvedRemediation(
      { ...message, source_subtype: "" }, "completed", true,
    )).toBe(false);
  });

  it("does not match a final task result", () => {
    expect(isHighConfidenceLocalRemediation(
      "The production deployment completed successfully and all checks passed.",
    )).toBe(false);
  });

  it("does not hide a successful outcome opener", () => {
    expect(isHighConfidenceLocalRemediation(
      "Release fixed a PowerShell issue in the isolated worktree; production was untouched while I repaired it.",
    )).toBe(false);
  });

  it("counts only updates that would be hidden", () => {
    expect(countResolvedRemediation([
      commentary(examples[0]!),
      commentary("The release completed successfully."),
    ], "completed")).toBe(1);
  });
});
