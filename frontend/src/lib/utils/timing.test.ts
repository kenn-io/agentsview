import { describe, expect, it } from "vite-plus/test";
import { turnHasCategory } from "./timing.js";

describe("turnHasCategory", () => {
  it("uses call categories instead of the summary category", () => {
    const turn = {
      message_id: 1,
      ordinal: 0,
      started_at: "2026-09-14T00:00:00Z",
      duration_ms: 1000,
      primary_category: "Mixed",
      calls: [
        {
          tool_use_id: "bash",
          tool_name: "Bash",
          category: "Bash",
          duration_ms: 1000,
          is_parallel: true,
          input_preview: "pwd",
        },
        {
          tool_use_id: "read",
          tool_name: "Read",
          category: "Read",
          duration_ms: null,
          is_parallel: true,
          input_preview: "main.go",
        },
      ],
    };

    expect(turnHasCategory(turn, "Read")).toBe(true);
    expect(turnHasCategory(turn, "Mixed")).toBe(false);
  });
});
