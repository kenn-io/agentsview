import { describe, expect, it } from "vite-plus/test";
import { codebuddyCanContinue } from "./codebuddy.js";

const conversation = { agent: "codebuddy", source_session_id: "run-1" };

describe("codebuddyCanContinue", () => {
  it("allows a CLI or IDE 5.x conversation, including an IDE fork", () => {
    expect(codebuddyCanContinue(conversation)).toBe(true);
    expect(codebuddyCanContinue({ ...conversation, relationship_type: "fork" })).toBe(true);
  });

  it.each([
    ["a preserved branch or 4.x manifest", { source_session_id: "" }],
    ["a side question", { session_kind: "btw" }],
    ["a subagent", { relationship_type: "subagent" }],
  ])("refuses %s", (_name, edit) => {
    expect(codebuddyCanContinue({ ...conversation, ...edit })).toBe(false);
  });

  it("leaves other agents to their own rules", () => {
    expect(codebuddyCanContinue({ agent: "claude" })).toBe(true);
  });
});
