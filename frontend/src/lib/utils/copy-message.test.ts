import { describe, it, expect } from "vite-plus/test";
import { formatMessageForCopy } from "./copy-message.js";
import type { Message } from "../api/types/core.js";

describe("formatMessageForCopy", () => {
  it("includes tool call params", () => {
    const msg: Message = {
      id: 1,
      session_id: "s1",
      ordinal: 1,
      role: "assistant",
      content: "Here is the edit",
      timestamp: "",
      has_thinking: false,
      thinking_text: "",
      has_tool_use: true,
      content_length: 16,
      model: "",
      context_tokens: 0,
      output_tokens: 0,
      is_system: false,
      tool_calls: [
        {
          tool_name: "Edit",
          category: "Edit",
          input_json: JSON.stringify({
            file: "src/app.ts",
            old_string: "const x = 1;",
            new_string: "const x = 2;",
          }),
        },
      ],
    };
    const result = formatMessageForCopy(msg);
    expect(result).toContain("Here is the edit");
    expect(result).toContain("[Edit]");
    expect(result).toContain("file: src/app.ts");
    expect(result).toContain("-const x = 1;");
    expect(result).toContain("+const x = 2;");
  });

  it("includes Write content", () => {
    const msg: Message = {
      id: 2,
      session_id: "s1",
      ordinal: 2,
      role: "assistant",
      content: "Creating file",
      timestamp: "",
      has_thinking: false,
      thinking_text: "",
      has_tool_use: true,
      content_length: 13,
      model: "",
      context_tokens: 0,
      output_tokens: 0,
      is_system: false,
      tool_calls: [
        {
          tool_name: "Write",
          category: "Write",
          input_json: JSON.stringify({
            file: "new.ts",
            content: "export const y = 42;",
          }),
        },
      ],
    };
    const result = formatMessageForCopy(msg);
    expect(result).toContain("[Write]");
    expect(result).toContain("file: new.ts");
    expect(result).toContain("+export const y = 42;");
  });

  it("includes kiro-ide Edit with diff key", () => {
    const msg = {
      id: 3, session_id: "s1", ordinal: 3, role: "assistant",
      content: "Updating config", timestamp: "",
      has_thinking: false, thinking_text: "", has_tool_use: true, content_length: 15,
      model: "", context_tokens: 0, output_tokens: 0, is_system: false,
      tool_calls: [{
        tool_name: "Edit", category: "Edit",
        input_json: JSON.stringify({
          file: "config.ts",
          diff: "--- a/config.ts\n+++ b/config.ts\n@@ -1,2 +1,2 @@\n-port: 3000\n+port: 8080",
        }),
      }],
    } as Message;
    const result = formatMessageForCopy(msg);
    expect(result).toContain("[Edit]");
    expect(result).toContain("file: config.ts");
    expect(result).toContain("-port: 3000");
    expect(result).toContain("+port: 8080");
  });
});
