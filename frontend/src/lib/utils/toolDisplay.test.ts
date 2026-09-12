import { describe, it, expect } from "vite-plus/test";
import {
  displayFormattedToolResult,
  displayToolName,
  displayToolResult,
} from "./toolDisplay.js";
import retainedFixtureSource from "./__fixtures__/retained-tool-image-1735.json?raw";

const SMALL_PNG_DATA_URI =
  "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=";

describe("displayToolName", () => {
  it("returns category for codex exec_command", () => {
    expect(displayToolName({ tool_name: "exec_command", category: "Bash" })).toBe("Bash");
  });

  it("returns category for Claude Bash", () => {
    expect(displayToolName({ tool_name: "Bash", category: "Bash" })).toBe("Bash");
  });

  it("returns category for codex apply_patch (Edit)", () => {
    expect(displayToolName({ tool_name: "apply_patch", category: "Edit" })).toBe("Edit");
  });

  it("returns tool_name when category is Other", () => {
    expect(displayToolName({ tool_name: "weird_tool", category: "Other" })).toBe("weird_tool");
  });

  it("returns tool_name when category is Tool (skills/MCP)", () => {
    expect(displayToolName({ tool_name: "Skill", category: "Tool" })).toBe("Skill");
  });

  it("returns tool_name when category is missing", () => {
    expect(displayToolName({ tool_name: "Read" })).toBe("Read");
  });

  it("returns tool_name when category is null", () => {
    expect(displayToolName({ tool_name: "Read", category: null })).toBe("Read");
  });

  it("returns tool_name when category is empty string", () => {
    expect(displayToolName({ tool_name: "Read", category: "" })).toBe("Read");
  });
});

describe("displayToolResult", () => {
  it("preserves grouped summary labels and order while displaying image placeholders", () => {
    const content =
      'agent-b:\n[{"type":"input_text","text":"Before"},{"type":"agentsview_image","version":1,"text":"[Image: image/png, 3 bytes]"}]\n\nagent-a:\n[{"type":"agentsview_image","version":1,"text":"[Image: image/jpeg, 6 bytes]"},{"type":"text","text":"After"}]\n\nagent-c:\n[{"type":"custom","value":42}]\n\n[{"type":"agentsview_image","version":1,"text":"[Image: image/gif, 9 bytes]"}]';
    expect(displayToolResult(content)).toBe(
      'agent-b:\nBefore\n\n[Image: image/png, 3 bytes]\n\nagent-a:\n[Image: image/jpeg, 6 bytes]\n\nAfter\n\nagent-c:\n[{"type":"custom","value":42}]\n\n[Image: image/gif, 9 bytes]',
    );
  });

  it("displays an unlabelled single-agent summary followed by anonymous output", () => {
    const content =
      '[{"type":"agentsview_image","version":1,"text":"[Image: image/png, 3 bytes]"}]\n\nFinished';
    expect(displayToolResult(content)).toBe("[Image: image/png, 3 bytes]\n\nFinished");
  });

  it.each([
    "ordinary output",
    '[{"type":"text","text":"ordinary JSON output"}]',
    '[{"type":"agentsview_image","version":1,"text":"[Image]"},{"type":"input_image","image_url":"https://example.com/image.png"}]',
    '[{"type":"agentsview_image","version":2,"text":"[Image]"}]',
    '[{"type":"agentsview_image","version":1,"text":"[Image]"},{"type":"input_text"}]',
    '[{"type":"agentsview_image",',
  ])("preserves unrecognized result content: %s", (content) => {
    expect(displayToolResult(content)).toBe(content);
  });
});

describe("displayFormattedToolResult", () => {
  it("renders the retained fixture between surrounding text", () => {
    const blocks = JSON.parse(retainedFixtureSource) as Array<{ image_url?: string }>;
    const imageURL = blocks[1]?.image_url;

    expect(new TextEncoder().encode(retainedFixtureSource).byteLength).toBe(1_441_138);
    const formatted = displayFormattedToolResult(retainedFixtureSource);
    expect(formatted).toBe(`Before\n\n![image/png](${imageURL})\n\nAfter`);
    expect(displayToolResult(retainedFixtureSource)).toBe(retainedFixtureSource);
  });

  it("renders an image-only retained array", () => {
    const content = JSON.stringify([{ type: "input_image", image_url: SMALL_PNG_DATA_URI }]);
    expect(displayFormattedToolResult(content)).toBe(`![image/png](${SMALL_PNG_DATA_URI})`);
    expect(displayToolResult(content)).toBe(content);
  });

  it.each(["image/png", "image/jpeg", "image/webp", "image/gif"])(
    "accepts %s data URIs only in formatted output",
    (mediaType) => {
      const imageURL = `data:${mediaType};base64,AAAA`;
      const content = JSON.stringify([{ type: "input_image", image_url: imageURL }]);
      expect(displayFormattedToolResult(content)).toBe(`![${mediaType}](${imageURL})`);
      expect(displayToolResult(content)).toBe(content);
    },
  );

  it("preserves labels, block order, and mixed placeholder content", () => {
    const firstImage = "data:image/png;base64,AAAA";
    const secondImage = "data:image/gif;base64,BBBB";
    const content =
      `agent-b:\n${JSON.stringify([
        { type: "input_image", image_url: firstImage },
        { type: "input_text", text: "B" },
      ])}\n\nagent-a:\n${JSON.stringify([
        { type: "input_text", text: "A" },
        { type: "agentsview_image", version: 1, text: "[Image: image/jpeg, 6 bytes]" },
        { type: "input_image", image_url: secondImage },
      ])}\n\nagent-c:\n[{"type":"custom","value":42}]`;

    expect(displayFormattedToolResult(content)).toBe(
      `agent-b:\n![image/png](${firstImage})\n\nB\n\nagent-a:\nA\n\n[Image: image/jpeg, 6 bytes]\n\n![image/gif](${secondImage})\n\nagent-c:\n[{"type":"custom","value":42}]`,
    );
  });

  it.each([
    JSON.stringify([{ type: "input_image", image_url: "https://example.com/image.png" }]),
    JSON.stringify([{ type: "input_image", image_url: "data:image/svg+xml;base64,AAAA" }]),
    JSON.stringify([{ type: "input_image", image_url: "data:text/html;base64,AAAA" }]),
    JSON.stringify([{ type: "input_image", image_url: "blob:https://example.com/id" }]),
    JSON.stringify([{ type: "input_image", image_url: "data:image/png,AAAA" }]),
    JSON.stringify([{ type: "input_image", image_url: "data:image/png;base64," }]),
    JSON.stringify([{ type: "input_image", image_url: "data:image/png;base64,AA=" }]),
    JSON.stringify([{ type: "input_image", image_url: "data:image/png;base64,AAAA=" }]),
    JSON.stringify([{ type: "input_image", image_url: "data:image/png;base64,AA AA" }]),
    JSON.stringify([{ type: "input_image", image_url: "data:image/png;base64,AAAA\n" }]),
    JSON.stringify([{ type: "input_image" }]),
    JSON.stringify([{ type: "input_image", image_url: 42 }]),
    JSON.stringify([
      { type: "input_image", image_url: SMALL_PNG_DATA_URI },
      { type: "input_text", text: 42 },
    ]),
    JSON.stringify([
      { type: "input_image", image_url: SMALL_PNG_DATA_URI },
      [{ type: "input_text", text: "nested" }],
    ]),
    JSON.stringify([
      { type: "input_image", image_url: SMALL_PNG_DATA_URI },
      { type: "agentsview_image", version: 2, text: "unsupported" },
    ]),
    JSON.stringify([
      { type: "input_image", image_url: SMALL_PNG_DATA_URI },
      { type: "custom", text: "unsupported" },
    ]),
    "not JSON [\"input_image\"]",
    "{\"type\":\"input_image\"}",
    "ordinary prose mentioning input_image",
    JSON.stringify([{ type: "input_text", text: "input_image" }]),
  ])("preserves unsupported or malformed content exactly: %s", (content) => {
    expect(displayFormattedToolResult(content)).toBe(content);
  });

  it("keeps unsupported non-array results and preserves anonymous summary order", () => {
    const content =
      `${JSON.stringify({ type: "input_image", image_url: SMALL_PNG_DATA_URI })}\n\n${JSON.stringify([
        { type: "input_image", image_url: SMALL_PNG_DATA_URI },
      ])}\n\nTrailing prose`;

    expect(displayFormattedToolResult(content)).toBe(
      `${JSON.stringify({ type: "input_image", image_url: SMALL_PNG_DATA_URI })}\n\n![image/png](${SMALL_PNG_DATA_URI})\n\nTrailing prose`,
    );
  });
});
